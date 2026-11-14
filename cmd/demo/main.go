// Command demo drives the GNWES control plane against a real,
// Kubernetes-scheduled GPU resource (Backend=Shared): how fast a GPULease
// binds against an already-admitted pool's budget, versus standing up a
// brand-new single-ticket pool and pod from scratch for every single actor
// ("cold" path, today's default on most clusters). Both numbers are real
// wall-clock time against the real scheduler and device plugin/HAMi — there
// is no injected delay on either path.
//
// Requires: CRDs installed and cmd/manager running against the same cluster,
// plus a real GPU device plugin (or HAMi) actually installed if you want
// GPUResourceName/GPUResourceQuantity on the pool's pods to bind to
// anything — otherwise the pods will stay Pending until one is.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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

// These must match the constants PodGateReconciler uses
// (internal/controller/podgate_controller.go); kept local rather than
// imported so the demo driver only depends on the public API convention
// (label + scheduling gate name), not the controller package's internals.
const (
	gnwesGateName     = "gnwes.io/gpu-capacity"
	gnwesPoolRefLabel = "gnwes.io/gpu-pool"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gnwesv1alpha1.AddToScheme(scheme))
}

func main() {
	mode := flag.String("mode", "both", "warm | cold | both")
	namespace := flag.String("namespace", "gnwes-demo", "namespace to run the demo in")
	actors := flag.Int("actors", 8, "number of ephemeral actors to cycle through")
	poolSize := flag.Int("pool-size", 3, "warm pool admission budget (warm mode only)")
	workMs := flag.Int("work-ms", 100, "simulated time each actor spends doing GPU work before releasing")
	gate := flag.Bool("gate", true, "hold each actor pod un-schedulable (scheduling gate) until a GPU slot is actually reserved for it")
	gpuResourceName := flag.String("gpu-resource-name", "nvidia.com/gpu", "real Kubernetes extended-resource key each actor pod requests; needs a real device plugin or HAMi installed to ever bind")
	gpuResourceQty := flag.String("gpu-resource-qty", "1", "quantity of gpu-resource-name each actor pod requests")
	flag.Parse()

	ctx := context.Background()
	cfg, err := config.GetConfig()
	must(err, "loading kubeconfig (is KUBECONFIG / ~/.kube/config pointed at your kind cluster?)")

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	must(err, "building client")

	must(ensureNamespace(ctx, c, *namespace), "creating namespace")

	opts := demoOpts{
		c: c, ns: *namespace, actors: *actors,
		poolSize:        int32(*poolSize),
		workMs:          time.Duration(*workMs) * time.Millisecond,
		gate:            *gate,
		gpuResourceName: *gpuResourceName,
		gpuResourceQty:  *gpuResourceQty,
	}
	if *gate {
		fmt.Println("scheduling gate: ON — actor pods are held un-schedulable until a GPU slot is reserved for them")
	} else {
		fmt.Println("scheduling gate: OFF — actor pods are scheduled immediately, with no admission check against GPU capacity")
	}

	var warmLatencies, coldLatencies []time.Duration

	if *mode == "warm" || *mode == "both" {
		fmt.Println("==================================================================")
		fmt.Println(" WARM PATH — lease attach from a pre-warmed GNWES pool")
		fmt.Println("==================================================================")
		warmLatencies = runWarm(ctx, opts)
		printSummary("warm (lease attach)", warmLatencies)
	}

	if *mode == "cold" || *mode == "both" {
		fmt.Println()
		fmt.Println("==================================================================")
		fmt.Println(" COLD PATH — stand up one dedicated slot per actor, from scratch")
		fmt.Println("==================================================================")
		coldLatencies = runCold(ctx, opts)
		printSummary("cold (dedicated slot)", coldLatencies)
	}

	if *mode == "both" {
		printComparison(warmLatencies, coldLatencies)
	}
}

type demoOpts struct {
	c               client.Client
	ns              string
	actors          int
	poolSize        int32
	workMs          time.Duration
	gate            bool
	gpuResourceName string
	gpuResourceQty  string
}

// podTimeout bounds how long we'll wait for an actor pod to reach Running.
// This now covers a REAL Kubernetes scheduling + device-plugin/HAMi GPU
// binding cost (and, on a cold cluster, an image pull), not a simulated
// delay — generous enough to cover a cold image pull on a freshly admitted
// node.
func (o demoOpts) podTimeout() time.Duration {
	return 90 * time.Second
}

// runWarm stands up ONE pool once, then cycles every actor's lease through
// it, reusing the same warm slots. Returns each actor's attach latency.
func runWarm(ctx context.Context, o demoOpts) []time.Duration {
	const poolName = "demo-warm-pool"
	must(deletePoolIfExists(ctx, o.c, o.ns, poolName), "cleaning up previous pool")

	warmup := timeIt(func() {
		must(createPool(ctx, o.c, o.ns, poolName, o.poolSize, o.gpuResourceName, o.gpuResourceQty), "creating warm pool")
		must(waitPoolReady(ctx, o.c, o.ns, poolName, 60*time.Second), "waiting for pool to be ready")
	})
	fmt.Printf("pool admission budget provisioned (one-time, paid ahead of any actor): %s for %d tickets\n", fmtMs(warmup), o.poolSize)

	latencies := make([]time.Duration, 0, o.actors)
	for i := 1; i <= o.actors; i++ {
		podName := fmt.Sprintf("actor-%d", i)
		leaseName := fmt.Sprintf("actor-%d-lease", i)

		must(createActorPod(ctx, o.c, o.ns, podName, poolName, o.gate, o.gpuResourceName, o.gpuResourceQty), "creating actor pod")
		must(waitPodRunning(ctx, o.c, o.ns, podName, o.podTimeout()), "waiting for actor pod to run")

		start := time.Now()
		must(createLease(ctx, o.c, o.ns, leaseName, poolName, podName, nil), "creating lease")
		status, err := waitLeaseBound(ctx, o.c, o.ns, leaseName, 10*time.Second)
		must(err, "waiting for lease to bind")
		latency := time.Since(start)
		latencies = append(latencies, latency)

		fmt.Printf("  actor %-2d  slot=%-12s attach=%-8s (controller-measured=%dms)\n",
			i, status.SlotID, fmtMs(latency), status.AttachLatencyMillis)

		time.Sleep(o.workMs) // simulated GPU work
		must(deleteLease(ctx, o.c, o.ns, leaseName), "releasing lease")
		must(deletePod(ctx, o.c, o.ns, podName), "deleting actor pod")
	}

	must(deletePoolIfExists(ctx, o.c, o.ns, poolName), "cleaning up pool")
	return latencies
}

// runCold stands up a brand-new single-slot pool from scratch for EVERY
// actor — no reuse — so each actor pays the full slot-init cost on its own
// critical path, the way a non-warm-pooled cluster behaves today.
func runCold(ctx context.Context, o demoOpts) []time.Duration {
	latencies := make([]time.Duration, 0, o.actors)
	podTimeout := o.podTimeout()
	for i := 1; i <= o.actors; i++ {
		poolName := fmt.Sprintf("demo-cold-pool-%d", i)
		podName := fmt.Sprintf("cold-actor-%d", i)
		leaseName := fmt.Sprintf("cold-actor-%d-lease", i)

		start := time.Now()
		must(createPool(ctx, o.c, o.ns, poolName, 1, o.gpuResourceName, o.gpuResourceQty), "creating dedicated pool")
		// Pod is created right away, before the pool has warmed up. With
		// gating on, the pod simply sits un-schedulable until the pool's one
		// slot is ready and reserved for it — no separate wait needed. With
		// gating off, nothing stops the pod from running immediately, so we
		// fall back to explicitly waiting for the pool instead.
		must(createActorPod(ctx, o.c, o.ns, podName, poolName, o.gate, o.gpuResourceName, o.gpuResourceQty), "creating actor pod")
		if !o.gate {
			must(waitPoolReady(ctx, o.c, o.ns, poolName, 60*time.Second), "waiting for dedicated pool")
		}
		must(waitPodRunning(ctx, o.c, o.ns, podName, podTimeout), "waiting for actor pod to run")
		must(createLease(ctx, o.c, o.ns, leaseName, poolName, podName, nil), "creating lease")
		status, err := waitLeaseBound(ctx, o.c, o.ns, leaseName, 10*time.Second)
		must(err, "waiting for lease to bind")
		latency := time.Since(start)
		latencies = append(latencies, latency)

		fmt.Printf("  actor %-2d  slot=%-12s total=%-8s (slot-init+attach, no reuse)\n",
			i, status.SlotID, fmtMs(latency))

		time.Sleep(o.workMs)
		must(deleteLease(ctx, o.c, o.ns, leaseName), "releasing lease")
		must(deletePod(ctx, o.c, o.ns, podName), "deleting actor pod")
		must(deletePoolIfExists(ctx, o.c, o.ns, poolName), "deleting dedicated pool")
	}
	return latencies
}

// ---------- k8s helpers ----------

func ensureNamespace(ctx context.Context, c client.Client, ns string) error {
	n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	err := c.Create(ctx, n)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func createPool(ctx context.Context, c client.Client, ns, name string, size int32, gpuResourceName, gpuResourceQty string) error {
	p := &gnwesv1alpha1.GPUWorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gnwesv1alpha1.GPUWorkerPoolSpec{
			PoolSize:            size,
			GPUModel:            "A100-40GB",
			GPUResourceName:     gpuResourceName,
			GPUResourceQuantity: gpuResourceQty,
		},
	}
	return c.Create(ctx, p)
}

func waitPoolReady(ctx context.Context, c client.Client, ns, name string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
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
	p := &gnwesv1alpha1.GPUWorkerPool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	err := c.Delete(ctx, p)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// createActorPod makes a minimal pod using the pause image, which kind
// preloads on every node, so no image pull is needed for the demo to run.
// It requests a real gpuResourceName/gpuResourceQty extended resource, so
// this only ever reaches Running on a cluster where that resource is real
// (a real device plugin or HAMi is installed) — there is no fallback that
// fakes a GPU being present. When gated, the pod carries the
// gnwes.io/gpu-capacity scheduling gate and is held un-schedulable until
// PodGateReconciler admits it against poolRef's real budget.
func createActorPod(ctx context.Context, c client.Client, ns, name, poolRef string, gated bool, gpuResourceName, gpuResourceQty string) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": "gnwes-demo-actor"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "actor",
				Image: "registry.k8s.io/pause:3.9",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceName(gpuResourceName): resource.MustParse(gpuResourceQty),
					},
				},
			}},
		},
	}
	if gated {
		pod.Labels[gnwesPoolRefLabel] = poolRef
		pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: gnwesGateName}}
	}
	return c.Create(ctx, pod)
}

func waitPodRunning(ctx context.Context, c client.Client, ns, name string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
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
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	err := c.Delete(ctx, pod, client.GracePeriodSeconds(0))
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func createLease(ctx context.Context, c client.Client, ns, name, poolRef, targetPod string, ttlSeconds *int32) error {
	l := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gnwesv1alpha1.GPULeaseSpec{
			PoolRef:    poolRef,
			TargetPod:  targetPod,
			TTLSeconds: ttlSeconds,
		},
	}
	return c.Create(ctx, l)
}

func waitLeaseBound(ctx context.Context, c client.Client, ns, name string, timeout time.Duration) (gnwesv1alpha1.GPULeaseStatus, error) {
	var last gnwesv1alpha1.GPULeaseStatus
	err := wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		var l gnwesv1alpha1.GPULease
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &l); err != nil {
			return false, err
		}
		last = l.Status
		switch l.Status.Phase {
		case gnwesv1alpha1.LeasePhaseBound:
			return true, nil
		case gnwesv1alpha1.LeasePhaseFailed:
			return false, fmt.Errorf("lease failed: %s", l.Status.Message)
		default:
			return false, nil
		}
	})
	return last, err
}

func deleteLease(ctx context.Context, c client.Client, ns, name string) error {
	l := &gnwesv1alpha1.GPULease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	err := c.Delete(ctx, l)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ---------- reporting ----------

func timeIt(f func()) time.Duration {
	start := time.Now()
	f()
	return time.Since(start)
}

func fmtMs(d time.Duration) string {
	return fmt.Sprintf("%.0fms", float64(d.Microseconds())/1000.0)
}

func printSummary(label string, latencies []time.Duration) {
	if len(latencies) == 0 {
		return
	}
	sorted := append([]time.Duration(nil), latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}
	avg := sum / time.Duration(len(sorted))
	fmt.Printf("\n--- %s: n=%d  min=%s  avg=%s  max=%s ---\n",
		label, len(sorted), fmtMs(sorted[0]), fmtMs(avg), fmtMs(sorted[len(sorted)-1]))
}

func printComparison(warm, cold []time.Duration) {
	if len(warm) == 0 || len(cold) == 0 {
		return
	}
	avg := func(ds []time.Duration) time.Duration {
		var sum time.Duration
		for _, d := range ds {
			sum += d
		}
		return sum / time.Duration(len(ds))
	}
	wa, ca := avg(warm), avg(cold)
	speedup := float64(ca) / float64(wa)
	fmt.Println()
	fmt.Println("==================================================================")
	fmt.Println(" WARM vs COLD — the number this demo exists to show")
	fmt.Println("==================================================================")
	fmt.Printf(" warm (lease attach, pool reused):   avg %s per actor\n", fmtMs(wa))
	fmt.Printf(" cold (dedicated slot per actor):    avg %s per actor\n", fmtMs(ca))
	fmt.Printf(" => leasing from a warm GNWES pool is ~%.0fx faster than provisioning a dedicated GPU slot per actor\n", speedup)
}

func must(err error, what string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %s: %v\n", what, err)
		os.Exit(1)
	}
}
