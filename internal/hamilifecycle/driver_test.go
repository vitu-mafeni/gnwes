package hamilifecycle

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"gnwes/internal/fluidcrclient"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := fluidcrclient.AddToScheme(scheme); err != nil {
		t.Fatalf("adding fluidcrclient scheme: %v", err)
	}
	return scheme
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&fluidcrclient.FluidCRCheckpoint{}, &fluidcrclient.FluidCRRestore{}).
		WithObjects(objs...).
		Build()
}

func testPodSpec() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{{Name: "trainer", Image: "example/trainer:test"}},
	}
}

func TestEnsureRunning_CreatesFreshPodWhenNoCheckpointExists(t *testing.T) {
	c := newFakeClient(t)
	d := &Driver{Client: c, Namespace: "ns1", PodName: "actor-pod", ContainerName: "trainer", PodSpec: testPodSpec()}

	if err := d.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: "actor-pod"}, &pod); err != nil {
		t.Fatalf("expected a fresh pod to be created: %v", err)
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != "trainer" {
		t.Fatalf("pod spec not as expected: %+v", pod.Spec)
	}

	// Calling again while the pod still exists must be a no-op (idempotent).
	if err := d.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("second EnsureRunning: %v", err)
	}
}

func TestEnsureSuspended_NoPodIsNoop(t *testing.T) {
	c := newFakeClient(t)
	d := &Driver{Client: c, Namespace: "ns1", PodName: "actor-pod", ContainerName: "trainer", PodSpec: testPodSpec(), PollInterval: time.Millisecond}

	if err := d.EnsureSuspended(context.Background()); err != nil {
		t.Fatalf("EnsureSuspended with no pod: %v", err)
	}
}

// simulateFluidCRCheckpointCompletion stands in for FluidCR's own real
// controller: marks the FluidCRCheckpoint Completed with a recorded archive
// path, and deletes the target pod -- exactly what FluidCR's documented
// behavior does ("removes the pod so the devices it held are free").
func simulateFluidCRCheckpointCompletion(t *testing.T, c client.Client, namespace, checkpointName, podName, archivePath string) {
	t.Helper()
	var ckpt fluidcrclient.FluidCRCheckpoint
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: checkpointName}, &ckpt); err != nil {
		t.Fatalf("getting FluidCRCheckpoint to simulate completion: %v", err)
	}
	ckpt.Status.Phase = fluidcrclient.FCPhaseCompleted
	ckpt.Status.Pods = []fluidcrclient.PodCheckpointStatus{{
		PodName:       podName,
		ContainerName: "trainer",
		CheckpointFiles: []fluidcrclient.CheckpointFile{
			{ContainerName: "trainer", FilePath: archivePath},
		},
	}}
	if err := c.Status().Update(context.Background(), &ckpt); err != nil {
		t.Fatalf("updating FluidCRCheckpoint status: %v", err)
	}

	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: podName}, &pod); err == nil {
		if err := c.Delete(context.Background(), &pod); err != nil {
			t.Fatalf("deleting pod to simulate FluidCR teardown: %v", err)
		}
	}
}

func TestEnsureSuspended_PollsUntilCompletedAndRecordsArchive(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "actor-pod", Namespace: "ns1"}, Spec: testPodSpec()}
	c := newFakeClient(t, pod)
	d := &Driver{Client: c, Namespace: "ns1", PodName: "actor-pod", ContainerName: "trainer", PodSpec: testPodSpec(), PollInterval: 5 * time.Millisecond}

	done := make(chan error, 1)
	go func() { done <- d.EnsureSuspended(context.Background()) }()

	// Give EnsureSuspended a moment to create the FluidCRCheckpoint, then
	// simulate FluidCR's controller completing it.
	time.Sleep(20 * time.Millisecond)
	simulateFluidCRCheckpointCompletion(t, c, "ns1", "actor-pod-ckpt", "actor-pod", "/var/lib/kubelet/checkpoints/test-archive.tar")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EnsureSuspended: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureSuspended did not return after checkpoint completed")
	}

	d.mu.Lock()
	cp := d.checkpoint
	d.mu.Unlock()
	if cp == nil || cp.path != "/var/lib/kubelet/checkpoints/test-archive.tar" {
		t.Fatalf("expected recorded checkpoint path, got %+v", cp)
	}

	var gone corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: "actor-pod"}, &gone); err == nil {
		t.Fatal("expected the pod to be gone after checkpoint completion")
	}
}

func TestEnsureSuspended_FailedCheckpointReturnsError(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "actor-pod", Namespace: "ns1"}, Spec: testPodSpec()}
	c := newFakeClient(t, pod)
	d := &Driver{Client: c, Namespace: "ns1", PodName: "actor-pod", ContainerName: "trainer", PodSpec: testPodSpec(), PollInterval: 5 * time.Millisecond}

	done := make(chan error, 1)
	go func() { done <- d.EnsureSuspended(context.Background()) }()

	time.Sleep(20 * time.Millisecond)
	var ckpt fluidcrclient.FluidCRCheckpoint
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: "actor-pod-ckpt"}, &ckpt); err != nil {
		t.Fatalf("getting FluidCRCheckpoint: %v", err)
	}
	ckpt.Status.Phase = fluidcrclient.FCPhaseFailed
	ckpt.Status.Message = "simulated checkpoint failure"
	if err := c.Status().Update(context.Background(), &ckpt); err != nil {
		t.Fatalf("updating status: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected EnsureSuspended to return an error on a failed checkpoint")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureSuspended did not return after checkpoint failed")
	}
}

// simulateFluidCRRestoreCompletion stands in for FluidCR's own real
// controller: creates the restored pod and marks the FluidCRRestore
// Completed.
func simulateFluidCRRestoreCompletion(t *testing.T, c client.Client, namespace, restoreName, podName string) {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace}, Spec: testPodSpec()}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatalf("creating restored pod to simulate FluidCR: %v", err)
	}

	var restore fluidcrclient.FluidCRRestore
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: restoreName}, &restore); err != nil {
		t.Fatalf("getting FluidCRRestore to simulate completion: %v", err)
	}
	restore.Status.Phase = fluidcrclient.FRPhaseCompleted
	restore.Status.PodName = podName
	if err := c.Status().Update(context.Background(), &restore); err != nil {
		t.Fatalf("updating FluidCRRestore status: %v", err)
	}
}

func TestEnsureRunning_RestoresFromCheckpointWhenOneExists(t *testing.T) {
	c := newFakeClient(t)
	d := &Driver{Client: c, Namespace: "ns1", PodName: "actor-pod", ContainerName: "trainer", PodSpec: testPodSpec(), PollInterval: 5 * time.Millisecond}
	d.checkpoint = &savedCheckpoint{path: "/var/lib/kubelet/checkpoints/test-archive.tar"}

	done := make(chan error, 1)
	go func() { done <- d.EnsureRunning(context.Background()) }()

	time.Sleep(20 * time.Millisecond)
	simulateFluidCRRestoreCompletion(t, c, "ns1", "actor-pod-restore", "actor-pod")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EnsureRunning: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureRunning did not return after restore completed")
	}

	var restore fluidcrclient.FluidCRRestore
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: "actor-pod-restore"}, &restore); err != nil {
		t.Fatalf("getting FluidCRRestore: %v", err)
	}
	if restore.Spec.Archive.Path != "/var/lib/kubelet/checkpoints/test-archive.tar" {
		t.Fatalf("restore archive path = %q, want the saved checkpoint path", restore.Spec.Archive.Path)
	}
	if restore.Spec.Archive.Container != "trainer" {
		t.Fatalf("restore archive container = %q, want trainer", restore.Spec.Archive.Container)
	}
}

func TestEnsureRunning_WhenPodAlreadyExistsIsNoop(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "actor-pod", Namespace: "ns1"}, Spec: testPodSpec()}
	c := newFakeClient(t, pod)
	d := &Driver{Client: c, Namespace: "ns1", PodName: "actor-pod", ContainerName: "trainer", PodSpec: testPodSpec()}

	if err := d.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	// No FluidCRRestore should have been created, since the pod already existed.
	var restore fluidcrclient.FluidCRRestore
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: "actor-pod-restore"}, &restore)
	if err == nil {
		t.Fatal("expected no FluidCRRestore to be created when the pod already exists")
	}
}

type fakeFetcher struct {
	mu    sync.Mutex
	state string
}

func (f *fakeFetcher) FetchActorState(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

func (f *fakeFetcher) setState(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

func TestRun_DrivesEnsureRunningAndSuspendedOnTransitions(t *testing.T) {
	c := newFakeClient(t)
	d := &Driver{Client: c, Namespace: "ns1", PodName: "actor-pod", ContainerName: "trainer", PodSpec: testPodSpec(), PollInterval: 2 * time.Millisecond}
	fetcher := &fakeFetcher{state: "ACTOR_STATE_SUSPENDED"}

	var transitions []Transition
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		Run(ctx, fetcher, d, 5*time.Millisecond, func(tr Transition) {
			transitions = append(transitions, tr)
		}, func(err error) {
			t.Errorf("unexpected error from Run: %v", err)
		})
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	fetcher.setState("ACTOR_STATE_RUNNING")
	time.Sleep(30 * time.Millisecond)

	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns1", Name: "actor-pod"}, &pod); err != nil {
		t.Fatalf("expected Run to have created the pod once actor went RUNNING: %v", err)
	}

	cancel()
	<-done

	if len(transitions) == 0 {
		t.Fatal("expected at least one transition to be reported")
	}
	last := transitions[len(transitions)-1]
	if !last.NowRunning {
		t.Fatalf("last transition = %+v, want NowRunning=true", last)
	}
}
