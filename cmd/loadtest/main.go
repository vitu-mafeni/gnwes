// Command loadtest exercises the REAL GPUWorkerPoolReconciler/GPULeaseReconciler
// concurrently, against a live cluster — not just internal/pool's in-memory
// bookkeeping in isolation (see internal/pool's own concurrency unit tests
// for that). Many actors race to lease a deliberately undersized pool at
// once; a monitor goroutine polls every Bound GPULease's slot ID throughout
// the run and fails immediately if it ever sees two leases simultaneously
// bound to the same slot. Exit code is non-zero on any failure or violation,
// so this doubles as a CI gate.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gnwesv1alpha1.AddToScheme(scheme))
}

func main() {
	namespace := flag.String("namespace", "gnwes-loadtest", "namespace to run in")
	poolSize := flag.Int("pool-size", 3, "warm pool size (deliberately small to force contention)")
	actors := flag.Int("actors", 40, "total number of actors to cycle through")
	concurrency := flag.Int("concurrency", 10, "number of actors in flight at once")
	holdMs := flag.Int("hold-ms", 15, "time each actor holds its lease before releasing")
	flag.Parse()

	ctx := context.Background()
	cfg, err := config.GetConfig()
	must(err, "loading kubeconfig")
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	must(err, "building client")
	must(ensureNamespace(ctx, c, *namespace), "creating namespace")

	const poolName = "loadtest-pool"
	must(deletePoolIfExists(ctx, c, *namespace, poolName), "cleaning up any previous pool")
	must(createPool(ctx, c, *namespace, poolName, int32(*poolSize)), "creating pool")
	must(waitPoolReady(ctx, c, *namespace, poolName, 60*time.Second), "waiting for pool to be ready")
	fmt.Printf("pool %q ready with %d slots; racing %d actors (concurrency=%d)\n", poolName, *poolSize, *actors, *concurrency)

	monitorCtx, stopMonitor := context.WithCancel(ctx)
	var violations int64
	var monitorWG sync.WaitGroup
	monitorWG.Add(1)
	go monitorForDoubleBooking(monitorCtx, &monitorWG, c, *namespace, &violations)

	type result struct {
		idx     int
		latency time.Duration
		err     error
	}
	jobs := make(chan int, *actors)
	for i := 0; i < *actors; i++ {
		jobs <- i
	}
	close(jobs)

	results := make(chan result, *actors)
	var wg sync.WaitGroup
	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				lat, err := runOneActor(ctx, c, *namespace, poolName, idx, time.Duration(*holdMs)*time.Millisecond)
				results <- result{idx: idx, latency: lat, err: err}
			}
		}()
	}
	wg.Wait()
	close(results)

	stopMonitor()
	monitorWG.Wait()

	var latencies []time.Duration
	var failed int
	for r := range results {
		if r.err != nil {
			failed++
			fmt.Printf("actor %d FAILED: %v\n", r.idx, r.err)
			continue
		}
		latencies = append(latencies, r.latency)
	}

	must(deletePoolIfExists(ctx, c, *namespace, poolName), "cleaning up pool")

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	var sum time.Duration
	for _, d := range latencies {
		sum += d
	}
	fmt.Println("----------------------------------------------------------------")
	fmt.Printf("succeeded=%d failed=%d double-booking violations=%d\n", len(latencies), failed, violations)
	if len(latencies) > 0 {
		fmt.Printf("attach latency: min=%s avg=%s max=%s\n",
			latencies[0], sum/time.Duration(len(latencies)), latencies[len(latencies)-1])
	}

	if failed > 0 || violations > 0 {
		fmt.Println("RESULT: FAIL")
		os.Exit(1)
	}
	fmt.Println("RESULT: PASS")
}

func runOneActor(ctx context.Context, c client.Client, ns, poolName string, idx int, hold time.Duration) (time.Duration, error) {
	podName := fmt.Sprintf("loadtest-actor-%d", idx)
	leaseName := fmt.Sprintf("loadtest-actor-%d-lease", idx)

	if err := createActorPod(ctx, c, ns, podName); err != nil {
		return 0, fmt.Errorf("create pod: %w", err)
	}
	defer func() { _ = deletePod(ctx, c, ns, podName) }()

	if err := waitPodRunning(ctx, c, ns, podName, 90*time.Second); err != nil {
		return 0, fmt.Errorf("wait pod running: %w", err)
	}

	start := time.Now()
	if err := createLease(ctx, c, ns, leaseName, poolName, podName); err != nil {
		return 0, fmt.Errorf("create lease: %w", err)
	}
	defer func() { _ = deleteLease(ctx, c, ns, leaseName) }()

	if err := waitLeaseBound(ctx, c, ns, leaseName, 20*time.Second); err != nil {
		return 0, fmt.Errorf("wait lease bound: %w", err)
	}
	latency := time.Since(start)

	time.Sleep(hold)
	return latency, nil
}

// monitorForDoubleBooking polls every GPULease in the namespace and fails
// loudly (atomically counted) if two DISTINCT leases are ever simultaneously
// Bound to the same slot ID — the real-controller equivalent of
// internal/pool's TestConcurrentAcquireReleaseNeverDoubleBooks.
func monitorForDoubleBooking(ctx context.Context, wg *sync.WaitGroup, c client.Client, ns string, violations *int64) {
	defer wg.Done()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var list gnwesv1alpha1.GPULeaseList
			if err := c.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
				continue
			}
			bySlot := map[string]string{}
			for _, l := range list.Items {
				if l.Status.Phase != gnwesv1alpha1.LeasePhaseBound || l.Status.SlotID == "" {
					continue
				}
				if owner, already := bySlot[l.Status.SlotID]; already && owner != l.Name {
					atomic.AddInt64(violations, 1)
					fmt.Printf("DOUBLE-BOOKING DETECTED: slot %s held by both %q and %q\n", l.Status.SlotID, owner, l.Name)
				}
				bySlot[l.Status.SlotID] = l.Name
			}
		}
	}
}

// ---------- k8s helpers (same shapes as cmd/demo, kept independent on
// purpose: this tool's job is to be a blunt, obviously-correct outside
// observer, not to share code paths with the thing it's testing) ----------

func ensureNamespace(ctx context.Context, c client.Client, ns string) error {
	err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func createPool(ctx context.Context, c client.Client, ns, name string, size int32) error {
	return c.Create(ctx, &gnwesv1alpha1.GPUWorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gnwesv1alpha1.GPUWorkerPoolSpec{
			PoolSize: size, GPUModel: "A100-40GB",
		},
	})
}

func waitPoolReady(ctx context.Context, c client.Client, ns, name string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		var p gnwesv1alpha1.GPUWorkerPool
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return p.Status.Phase == gnwesv1alpha1.PoolPhaseReady, nil
	})
}

func deletePoolIfExists(ctx context.Context, c client.Client, ns, name string) error {
	err := c.Delete(ctx, &gnwesv1alpha1.GPUWorkerPool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func createActorPod(ctx context.Context, c client.Client, ns, name string) error {
	return c.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": "gnwes-loadtest-actor"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "actor", Image: "registry.k8s.io/pause:3.9"}},
		},
	})
}

func waitPodRunning(ctx context.Context, c client.Client, ns, name string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		var pod corev1.Pod
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pod); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return pod.Status.Phase == corev1.PodRunning, nil
	})
}

func deletePod(ctx context.Context, c client.Client, ns, name string) error {
	err := c.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, client.GracePeriodSeconds(0))
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func createLease(ctx context.Context, c client.Client, ns, name, poolRef, targetPod string) error {
	return c.Create(ctx, &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: poolRef, TargetPod: targetPod},
	})
}

func waitLeaseBound(ctx context.Context, c client.Client, ns, name string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		var l gnwesv1alpha1.GPULease
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &l); err != nil {
			return false, err
		}
		switch l.Status.Phase {
		case gnwesv1alpha1.LeasePhaseBound:
			return true, nil
		case gnwesv1alpha1.LeasePhaseFailed:
			return false, fmt.Errorf("lease failed: %s", l.Status.Message)
		default:
			return false, nil
		}
	})
}

func deleteLease(ctx context.Context, c client.Client, ns, name string) error {
	err := c.Delete(ctx, &gnwesv1alpha1.GPULease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func must(err error, what string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %s: %v\n", what, err)
		os.Exit(1)
	}
}
