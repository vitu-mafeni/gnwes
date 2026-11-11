package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
	"gnwes/internal/pool"
)

func newReadyPool(mgr *pool.Manager, key string, size int32) *pool.Pool {
	p := mgr.EnsurePool(key, "mock-model", 1, 1) // 1ms init/attach: fast and deterministic in tests
	p.Resize(size)
	time.Sleep(10 * time.Millisecond) // let slots promote to Available
	return p
}

func TestGPULeaseReconcile_FullAttachFlow(t *testing.T) {
	pod := newTestPod("actor-1", "ns")
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "actor-1"},
	}
	c := newFakeClient(t, pod, lease)
	mgr := pool.NewManager()
	newReadyPool(mgr, "ns/pool-a", 1)

	r := &GPULeaseReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "lease-1", Namespace: "ns"}}

	// First reconcile only adds the finalizer.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	// Second reconcile does the actual acquire + attach.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	var got gnwesv1alpha1.GPULease
	if err := c.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("get lease: %v", err)
	}
	if got.Status.Phase != gnwesv1alpha1.LeasePhaseBound {
		t.Fatalf("expected Bound, got %q (%s)", got.Status.Phase, got.Status.Message)
	}
	if got.Status.SlotID == "" {
		t.Fatalf("expected a slot ID to be recorded")
	}

	var gotPod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Name: "actor-1", Namespace: "ns"}, &gotPod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if gotPod.Annotations[annoSlot] != got.Status.SlotID {
		t.Fatalf("expected pod annotation %s=%s, got %q", annoSlot, got.Status.SlotID, gotPod.Annotations[annoSlot])
	}

	if _, available, leased, _ := mgr.GetPool("ns/pool-a").Counts(); available != 0 || leased != 1 {
		t.Fatalf("expected the slot to show leased in the pool, got available=%d leased=%d", available, leased)
	}
}

func TestGPULeaseReconcile_InheritsPodGateReservation(t *testing.T) {
	mgr := pool.NewManager()
	p := newReadyPool(mgr, "ns/pool-a", 1)

	reservationKey := "podgate/ns/actor-1"
	slotID, ok := p.Acquire(reservationKey)
	if !ok {
		t.Fatalf("setup: expected to reserve the slot")
	}

	pod := newTestPod("actor-1", "ns")
	pod.Annotations = map[string]string{annoReservedSlot: slotID}
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "actor-1"},
	}
	c := newFakeClient(t, pod, lease)
	r := &GPULeaseReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "lease-1", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	var got gnwesv1alpha1.GPULease
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if got.Status.Phase != gnwesv1alpha1.LeasePhaseBound || got.Status.SlotID != slotID {
		t.Fatalf("expected Bound with inherited slot %s, got phase=%q slot=%q", slotID, got.Status.Phase, got.Status.SlotID)
	}

	// Must not have acquired a SECOND slot: still exactly 1 leased, 0 available.
	if _, available, leased, _ := p.Counts(); available != 0 || leased != 1 {
		t.Fatalf("expected exactly one leased slot (inherited, not double-acquired), got available=%d leased=%d", available, leased)
	}

	var gotPod corev1.Pod
	_ = c.Get(context.Background(), types.NamespacedName{Name: "actor-1", Namespace: "ns"}, &gotPod)
	if _, stillReserved := gotPod.Annotations[annoReservedSlot]; stillReserved {
		t.Fatalf("expected the reserved-slot annotation to be cleared once the lease took it over")
	}
}

func TestGPULeaseReconcile_PoolNotFoundYet(t *testing.T) {
	pod := newTestPod("actor-1", "ns")
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "does-not-exist", TargetPod: "actor-1"},
	}
	c := newFakeClient(t, pod, lease)
	r := &GPULeaseReconciler{Client: c, Manager: pool.NewManager()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "lease-1", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil { // finalizer
		t.Fatalf("reconcile 1: %v", err)
	}
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected a requeue while waiting for the pool to exist")
	}

	var got gnwesv1alpha1.GPULease
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if got.Status.Phase != gnwesv1alpha1.LeasePhasePending {
		t.Fatalf("expected Pending, got %q", got.Status.Phase)
	}
}

func TestGPULeaseReconcile_NoAvailableSlot(t *testing.T) {
	mgr := pool.NewManager()
	mgr.EnsurePool("ns/pool-a", "model", 100, 1).Resize(1) // long init: stays Initializing, never Available

	pod := newTestPod("actor-1", "ns")
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "actor-1"},
	}
	c := newFakeClient(t, pod, lease)
	r := &GPULeaseReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "lease-1", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected a requeue while waiting for a slot")
	}

	var got gnwesv1alpha1.GPULease
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if got.Status.Phase != gnwesv1alpha1.LeasePhasePending {
		t.Fatalf("expected Pending, got %q", got.Status.Phase)
	}
}

func TestGPULeaseReconcile_TargetPodNotFound(t *testing.T) {
	mgr := pool.NewManager()
	newReadyPool(mgr, "ns/pool-a", 1)

	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "no-such-pod"},
	}
	c := newFakeClient(t, lease) // no pod object
	r := &GPULeaseReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "lease-1", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	var got gnwesv1alpha1.GPULease
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if got.Status.Phase != gnwesv1alpha1.LeasePhaseFailed {
		t.Fatalf("expected Failed, got %q", got.Status.Phase)
	}
}

func TestGPULeaseReconcile_DeleteReleasesSlotAndUnannotatesPod(t *testing.T) {
	pod := newTestPod("actor-1", "ns")
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "actor-1"},
	}
	c := newFakeClient(t, pod, lease)
	mgr := pool.NewManager()
	p := newReadyPool(mgr, "ns/pool-a", 1)

	r := &GPULeaseReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "lease-1", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1 (finalizer): %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2 (attach): %v", err)
	}

	var bound gnwesv1alpha1.GPULease
	_ = c.Get(context.Background(), req.NamespacedName, &bound)
	if bound.Status.Phase != gnwesv1alpha1.LeasePhaseBound {
		t.Fatalf("setup: expected Bound before testing delete, got %q", bound.Status.Phase)
	}

	if err := c.Delete(context.Background(), &bound); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Finalizer present: the fake client keeps the object with a DeletionTimestamp.
	var deleting gnwesv1alpha1.GPULease
	if err := c.Get(context.Background(), req.NamespacedName, &deleting); err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if deleting.DeletionTimestamp.IsZero() {
		t.Fatalf("expected a DeletionTimestamp while the finalizer is still present")
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 3 (cleanup): %v", err)
	}

	if _, available, leased, _ := p.Counts(); available != 1 || leased != 0 {
		t.Fatalf("expected the slot released back to Available, got available=%d leased=%d", available, leased)
	}

	var gotPod corev1.Pod
	_ = c.Get(context.Background(), types.NamespacedName{Name: "actor-1", Namespace: "ns"}, &gotPod)
	if _, hasSlot := gotPod.Annotations[annoSlot]; hasSlot {
		t.Fatalf("expected the gpu-slot annotation to be removed from the pod on release")
	}
}

func TestGPULeaseReconcile_TTLAutoReleases(t *testing.T) {
	pod := newTestPod("actor-1", "ns")
	zero := int32(0)
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "actor-1", TTLSeconds: &zero},
	}
	c := newFakeClient(t, pod, lease)
	mgr := pool.NewManager()
	p := newReadyPool(mgr, "ns/pool-a", 1)
	r := &GPULeaseReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "lease-1", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil { // binds, TTL=0 so next reconcile releases
		t.Fatalf("reconcile 2: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 3 (ttl release): %v", err)
	}

	var got gnwesv1alpha1.GPULease
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if got.Status.Phase != gnwesv1alpha1.LeasePhaseReleased {
		t.Fatalf("expected Released after TTL expiry, got %q (%s)", got.Status.Phase, got.Status.Message)
	}
	if _, available, leased, _ := p.Counts(); available != 1 || leased != 0 {
		t.Fatalf("expected the slot released back to Available after TTL expiry, got available=%d leased=%d", available, leased)
	}
}
