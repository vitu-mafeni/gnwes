package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
	"gnwes/internal/pool"
)

func gatedPod(name, namespace, poolRef string) *corev1.Pod {
	pod := newTestPod(name, namespace)
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: gateName}}
	if poolRef != "" {
		pod.Labels = map[string]string{labelPoolRef: poolRef}
	}
	return pod
}

func sharedPool(name, namespace string, size int32) *gnwesv1alpha1.GPUWorkerPool {
	return &gnwesv1alpha1.GPUWorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       gnwesv1alpha1.GPUWorkerPoolSpec{PoolSize: size},
	}
}

// newReadyPool creates a real GPUWorkerPool(Backend=Shared) CR in c AND its
// matching in-memory admission pool in mgr, already resized — the shape
// most GPULease/PodGate tests need as setup.
func newReadyPool(t *testing.T, c client.Client, mgr *pool.Manager, namespace, name string, size int32) *pool.Pool {
	t.Helper()
	wp := sharedPool(name, namespace, size)
	if err := c.Create(context.Background(), wp); err != nil {
		t.Fatalf("setup: creating GPUWorkerPool: %v", err)
	}
	p := mgr.EnsurePool(namespace+"/"+name, "mock-model")
	p.Resize(size)
	return p
}

func TestPodGateReconcile_UngatedPodIsNoop(t *testing.T) {
	pod := newTestPod("plain", "ns") // no scheduling gate at all
	c := newFakeClient(t, pod)
	r := &PodGateReconciler{Client: c, Manager: pool.NewManager()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "plain", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got corev1.Pod
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if len(got.Spec.SchedulingGates) != 0 {
		t.Fatalf("a pod with no gate must be left alone")
	}
}

func TestPodGateReconcile_MissingPoolLabelRemovesGateImmediately(t *testing.T) {
	pod := gatedPod("orphan", "ns", "") // gate present, no pool label
	c := newFakeClient(t, pod)
	r := &PodGateReconciler{Client: c, Manager: pool.NewManager()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "orphan", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got corev1.Pod
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if len(got.Spec.SchedulingGates) != 0 {
		t.Fatalf("expected the gate removed when there's no pool to check capacity against")
	}
}

func TestPodGateReconcile_WaitsWhenPoolDoesNotExistYet(t *testing.T) {
	pod := gatedPod("actor-1", "ns", "pool-a")
	c := newFakeClient(t, pod)
	r := &PodGateReconciler{Client: c, Manager: pool.NewManager()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "actor-1", Namespace: "ns"}}

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected a requeue while the pool doesn't exist yet")
	}

	var got corev1.Pod
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if !hasGate(&got) {
		t.Fatalf("the gate must stay while capacity cannot be confirmed")
	}
}

func TestPodGateReconcile_WaitsWhenNoCapacity(t *testing.T) {
	pod := gatedPod("actor-1", "ns", "pool-a")
	c := newFakeClient(t, pod)
	mgr := pool.NewManager()
	wp := sharedPool("pool-a", "ns", 1)
	if err := c.Create(context.Background(), wp); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mgr.EnsurePool("ns/pool-a", "model") // zero tickets: never any capacity

	r := &PodGateReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "actor-1", Namespace: "ns"}}

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected a requeue while no ticket is available")
	}

	var got corev1.Pod
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if !hasGate(&got) {
		t.Fatalf("the gate must stay while there is no available ticket")
	}
}

func TestPodGateReconcile_AdmitsAndReservesWhenCapacityExists(t *testing.T) {
	pod := gatedPod("actor-1", "ns", "pool-a")
	c := newFakeClient(t, pod)
	mgr := pool.NewManager()
	p := newReadyPool(t, c, mgr, "ns", "pool-a", 1)

	r := &PodGateReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "actor-1", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got corev1.Pod
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if hasGate(&got) {
		t.Fatalf("expected the gate to be removed once a ticket was reserved")
	}
	if got.Annotations[annoReservedSlot] == "" {
		t.Fatalf("expected a reserved-slot annotation to be recorded")
	}
	if !controllerutil.ContainsFinalizer(&got, gateFinalizer) {
		t.Fatalf("expected the cleanup finalizer to be added")
	}
	if _, available, leased := p.Counts(); available != 0 || leased != 1 {
		t.Fatalf("expected the ticket to show reserved/leased, got available=%d leased=%d", available, leased)
	}
}

func TestPodGateReconcile_DeleteReleasesUnclaimedReservation(t *testing.T) {
	pod := gatedPod("actor-1", "ns", "pool-a")
	c := newFakeClient(t, pod)
	mgr := pool.NewManager()
	p := newReadyPool(t, c, mgr, "ns", "pool-a", 1)

	r := &PodGateReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "actor-1", Namespace: "ns"}}

	// Admit it first so it holds a reservation + finalizer.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (admit): %v", err)
	}

	var admitted corev1.Pod
	_ = c.Get(context.Background(), req.NamespacedName, &admitted)
	if err := c.Delete(context.Background(), &admitted); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (cleanup): %v", err)
	}

	if _, available, leased := p.Counts(); available != 1 || leased != 0 {
		t.Fatalf("expected the never-claimed reservation released back to Available, got available=%d leased=%d", available, leased)
	}
}

func TestPodGateReconcile_DeleteAfterTransferDoesNotDoubleRelease(t *testing.T) {
	pod := gatedPod("actor-1", "ns", "pool-a")
	c := newFakeClient(t, pod)
	mgr := pool.NewManager()
	p := newReadyPool(t, c, mgr, "ns", "pool-a", 1)

	r := &PodGateReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "actor-1", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (admit): %v", err)
	}

	var admitted corev1.Pod
	_ = c.Get(context.Background(), req.NamespacedName, &admitted)
	slotID := admitted.Annotations[annoReservedSlot]

	// Simulate a GPULease having already transferred the reservation away.
	if !p.Transfer(slotID, "podgate/ns/actor-1", "lease/ns/some-lease") {
		t.Fatalf("setup: expected the transfer to succeed")
	}

	if err := c.Delete(context.Background(), &admitted); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (cleanup after transfer): %v", err)
	}

	// The ticket must still be owned by the lease's key, not have been
	// incorrectly released back to Available by the pod's own cleanup.
	if _, available, leased := p.Counts(); available != 0 || leased != 1 {
		t.Fatalf("expected the ticket to remain leased under the lease's ownership, got available=%d leased=%d", available, leased)
	}
}
