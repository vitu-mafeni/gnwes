package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
	"gnwes/internal/pool"
)

// runningGPUPod is what bindShared actually needs to consider a Shared-tier
// pod bound: Running, with a real GPU extended-resource request on its spec
// (what the real scheduler/device-plugin would have admitted against).
func runningGPUPod(name, namespace string) *corev1.Pod {
	pod := newTestPod(name, namespace)
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
	}
	pod.Spec.NodeName = "node-1"
	pod.Status.Phase = corev1.PodRunning
	return pod
}

func TestGPULeaseReconcile_FullBindFlow(t *testing.T) {
	pod := runningGPUPod("actor-1", "ns")
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "actor-1"},
	}
	wp := sharedPool("pool-a", "ns", 1)
	c := newFakeClient(t, pod, lease, wp)
	mgr := pool.NewManager()
	mgr.EnsurePool("ns/pool-a", "mock-model").Resize(1)

	r := &GPULeaseReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "lease-1", Namespace: "ns"}}

	// First reconcile only adds the finalizer.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	// Second reconcile does the actual admit + bind.
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
		t.Fatalf("expected a real node/resource descriptor to be recorded")
	}

	var gotPod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Name: "actor-1", Namespace: "ns"}, &gotPod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if gotPod.Annotations[annoSlot] != got.Status.SlotID {
		t.Fatalf("expected pod annotation %s=%s, got %q", annoSlot, got.Status.SlotID, gotPod.Annotations[annoSlot])
	}

	if _, available, leased := mgr.GetPool("ns/pool-a").Counts(); available != 0 || leased != 1 {
		t.Fatalf("expected the ticket to show leased in the pool, got available=%d leased=%d", available, leased)
	}
}

func TestGPULeaseReconcile_PendingUntilPodRunning(t *testing.T) {
	pod := newTestPod("actor-1", "ns") // not Running yet
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "actor-1"},
	}
	wp := sharedPool("pool-a", "ns", 1)
	c := newFakeClient(t, pod, lease, wp)
	mgr := pool.NewManager()
	mgr.EnsurePool("ns/pool-a", "mock-model").Resize(1)

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
		t.Fatalf("expected a requeue while waiting for the pod to really be Running")
	}

	var got gnwesv1alpha1.GPULease
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if got.Status.Phase != gnwesv1alpha1.LeasePhasePending {
		t.Fatalf("expected Pending, got %q", got.Status.Phase)
	}

	// The ticket IS admitted already (that's what gates scheduling in the
	// first place) — only the real-bind confirmation is still pending.
	if _, available, leased := mgr.GetPool("ns/pool-a").Counts(); available != 0 || leased != 1 {
		t.Fatalf("expected the ticket already admitted while waiting, got available=%d leased=%d", available, leased)
	}
}

func TestGPULeaseReconcile_InheritsPodGateReservation(t *testing.T) {
	mgr := pool.NewManager()
	p := mgr.EnsurePool("ns/pool-a", "mock-model")
	p.Resize(1)

	reservationKey := "podgate/ns/actor-1"
	ticketID, ok := p.Acquire(reservationKey)
	if !ok {
		t.Fatalf("setup: expected to reserve the ticket")
	}

	pod := runningGPUPod("actor-1", "ns")
	pod.Annotations = map[string]string{annoReservedSlot: ticketID}
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "actor-1"},
	}
	wp := sharedPool("pool-a", "ns", 1)
	c := newFakeClient(t, pod, lease, wp)
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
	if got.Status.Phase != gnwesv1alpha1.LeasePhaseBound {
		t.Fatalf("expected Bound, got phase=%q message=%q", got.Status.Phase, got.Status.Message)
	}

	// Must not have acquired a SECOND ticket: still exactly 1 leased, 0 available.
	if _, available, leased := p.Counts(); available != 0 || leased != 1 {
		t.Fatalf("expected exactly one leased ticket (inherited, not double-acquired), got available=%d leased=%d", available, leased)
	}

	var gotPod corev1.Pod
	_ = c.Get(context.Background(), types.NamespacedName{Name: "actor-1", Namespace: "ns"}, &gotPod)
	if _, stillReserved := gotPod.Annotations[annoReservedSlot]; stillReserved {
		t.Fatalf("expected the reserved-slot annotation to be cleared once the lease took it over")
	}
}

func TestGPULeaseReconcile_PoolNotFoundYet(t *testing.T) {
	pod := runningGPUPod("actor-1", "ns")
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "does-not-exist", TargetPod: "actor-1"},
	}
	c := newFakeClient(t, pod, lease) // no GPUWorkerPool object
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

func TestGPULeaseReconcile_NoAvailableTicket(t *testing.T) {
	wp := sharedPool("pool-a", "ns", 1)
	mgr := pool.NewManager()
	mgr.EnsurePool("ns/pool-a", "model") // zero tickets

	pod := runningGPUPod("actor-1", "ns")
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "actor-1"},
	}
	c := newFakeClient(t, pod, lease, wp)
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
		t.Fatalf("expected a requeue while waiting for a ticket")
	}

	var got gnwesv1alpha1.GPULease
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if got.Status.Phase != gnwesv1alpha1.LeasePhasePending {
		t.Fatalf("expected Pending, got %q", got.Status.Phase)
	}
}

func TestGPULeaseReconcile_TargetPodNotFound(t *testing.T) {
	wp := sharedPool("pool-a", "ns", 1)
	mgr := pool.NewManager()
	mgr.EnsurePool("ns/pool-a", "model").Resize(1)

	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "no-such-pod"},
	}
	c := newFakeClient(t, lease, wp) // no pod object
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

func TestGPULeaseReconcile_DeleteReleasesTicketAndUnannotatesPod(t *testing.T) {
	pod := runningGPUPod("actor-1", "ns")
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "actor-1"},
	}
	wp := sharedPool("pool-a", "ns", 1)
	c := newFakeClient(t, pod, lease, wp)
	mgr := pool.NewManager()
	p := mgr.EnsurePool("ns/pool-a", "mock-model")
	p.Resize(1)

	r := &GPULeaseReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "lease-1", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1 (finalizer): %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2 (bind): %v", err)
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

	if _, available, leased := p.Counts(); available != 1 || leased != 0 {
		t.Fatalf("expected the ticket released back to Available, got available=%d leased=%d", available, leased)
	}

	var gotPod corev1.Pod
	_ = c.Get(context.Background(), types.NamespacedName{Name: "actor-1", Namespace: "ns"}, &gotPod)
	if _, hasSlot := gotPod.Annotations[annoSlot]; hasSlot {
		t.Fatalf("expected the gpu-slot annotation to be removed from the pod on release")
	}
}

func TestGPULeaseReconcile_TTLAutoReleases(t *testing.T) {
	pod := runningGPUPod("actor-1", "ns")
	zero := int32(0)
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "actor-1", TTLSeconds: &zero},
	}
	wp := sharedPool("pool-a", "ns", 1)
	c := newFakeClient(t, pod, lease, wp)
	mgr := pool.NewManager()
	p := mgr.EnsurePool("ns/pool-a", "mock-model")
	p.Resize(1)
	r := &GPULeaseReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "lease-1", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil { // binds, TTL=0 so next reconcile releases
		t.Fatalf("reconcile 2: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 3 (ttl release): %v", err)
	}

	var got gnwesv1alpha1.GPULease
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if got.Status.Phase != gnwesv1alpha1.LeasePhaseReleased {
		t.Fatalf("expected Released after TTL expiry, got %q (%s)", got.Status.Phase, got.Status.Message)
	}
	if _, available, leased := p.Counts(); available != 1 || leased != 0 {
		t.Fatalf("expected the ticket released back to Available after TTL expiry, got available=%d leased=%d", available, leased)
	}
}

func exclusivePool(name, namespace, vfioPoolRef string) *gnwesv1alpha1.GPUWorkerPool {
	return &gnwesv1alpha1.GPUWorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: gnwesv1alpha1.GPUWorkerPoolSpec{
			PoolSize:    1,
			Backend:     gnwesv1alpha1.BackendExclusive,
			VFIOPoolRef: vfioPoolRef,
		},
	}
}

// TestGPULeaseReconcile_ExclusiveNeverForcesAttach is the regression test for
// the exact bug this design avoids: GPULeaseReconciler must NEVER write
// VMGPUAttachment.Spec.DesiredState itself (that's cmd/substrate-gpu-bridge's
// job, driven by the real actor's real state) — only observe Status. A
// lease targeting a pod whose attachment nobody has flipped to Attached yet
// must stay Pending forever, with the attachment's Spec left untouched.
func TestGPULeaseReconcile_ExclusiveNeverForcesAttach(t *testing.T) {
	pod := newTestPod("actor-1", "ns")
	wp := exclusivePool("pool-a", "ns", "vfio-a")
	attachment := &gnwesv1alpha1.VMGPUAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "actor-1", Namespace: "ns"},
		Spec: gnwesv1alpha1.VMGPUAttachmentSpec{
			PoolRef:      "vfio-a",
			APISocket:    "/run/vc/vm/x/clh-api.sock",
			DesiredState: gnwesv1alpha1.VMGPUDesiredDetached, // nobody has resumed an actor onto it yet
		},
	}
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "actor-1"},
	}
	c := newFakeClient(t, pod, wp, attachment, lease)
	r := &GPULeaseReconciler{Client: c, Manager: pool.NewManager()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "lease-1", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1 (finalizer): %v", err)
	}
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected a requeue while waiting for a real actor to resume")
	}

	var got gnwesv1alpha1.GPULease
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if got.Status.Phase != gnwesv1alpha1.LeasePhasePending {
		t.Fatalf("expected Pending, got %q", got.Status.Phase)
	}

	var gotAttachment gnwesv1alpha1.VMGPUAttachment
	_ = c.Get(context.Background(), types.NamespacedName{Name: "actor-1", Namespace: "ns"}, &gotAttachment)
	if gotAttachment.Spec.DesiredState != gnwesv1alpha1.VMGPUDesiredDetached {
		t.Fatalf("GPULeaseReconciler must never write VMGPUAttachment.Spec.DesiredState itself, but it changed to %q", gotAttachment.Spec.DesiredState)
	}
}

// TestGPULeaseReconcile_ExclusiveReportsRealAttachOnceBridgeAttachesIt
// simulates cmd/substrate-gpu-bridge having already driven the attachment to
// Attached (as it would once the real actor resumes) — the lease must then
// report Bound using the attachment's own real, hardware-measured status.
func TestGPULeaseReconcile_ExclusiveReportsRealAttachOnceBridgeAttachesIt(t *testing.T) {
	pod := newTestPod("actor-1", "ns")
	wp := exclusivePool("pool-a", "ns", "vfio-a")
	now := metav1.Now()
	attachment := &gnwesv1alpha1.VMGPUAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "actor-1", Namespace: "ns"},
		Spec: gnwesv1alpha1.VMGPUAttachmentSpec{
			PoolRef:      "vfio-a",
			APISocket:    "/run/vc/vm/x/clh-api.sock",
			DesiredState: gnwesv1alpha1.VMGPUDesiredAttached, // the bridge already flipped this
		},
		Status: gnwesv1alpha1.VMGPUAttachmentStatus{
			Phase:               gnwesv1alpha1.VMGPUPhaseAttached,
			DeviceID:            "gpu-0",
			GuestBDF:            "0000:00:05.0",
			LastTransitionAt:    &now,
			AttachLatencyMillis: 42,
		},
	}
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "actor-1"},
	}
	c := newFakeClient(t, pod, wp, attachment, lease)
	r := &GPULeaseReconciler{Client: c, Manager: pool.NewManager()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "lease-1", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1 (finalizer): %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	var got gnwesv1alpha1.GPULease
	_ = c.Get(context.Background(), req.NamespacedName, &got)
	if got.Status.Phase != gnwesv1alpha1.LeasePhaseBound {
		t.Fatalf("expected Bound, got %q (%s)", got.Status.Phase, got.Status.Message)
	}
	if got.Status.SlotID != "gpu-0" {
		t.Fatalf("expected SlotID to be the real device ID gpu-0, got %q", got.Status.SlotID)
	}
	if got.Status.AttachLatencyMillis != 42 {
		t.Fatalf("expected the REAL measured latency (42ms) from VMGPUAttachment.Status to be copied through, got %d", got.Status.AttachLatencyMillis)
	}
}

// TestGPULeaseReconcile_ExclusiveDeleteDoesNotTouchAttachmentSpec confirms
// release() is a no-op on Spec.DesiredState for Backend=Exclusive — the real
// hot-detach must stay entirely cmd/substrate-gpu-bridge's decision, driven
// by the real actor actually suspending.
func TestGPULeaseReconcile_ExclusiveDeleteDoesNotTouchAttachmentSpec(t *testing.T) {
	pod := newTestPod("actor-1", "ns")
	wp := exclusivePool("pool-a", "ns", "vfio-a")
	now := metav1.Now()
	attachment := &gnwesv1alpha1.VMGPUAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "actor-1", Namespace: "ns"},
		Spec: gnwesv1alpha1.VMGPUAttachmentSpec{
			PoolRef: "vfio-a", APISocket: "/run/vc/vm/x/clh-api.sock",
			DesiredState: gnwesv1alpha1.VMGPUDesiredAttached,
		},
		Status: gnwesv1alpha1.VMGPUAttachmentStatus{Phase: gnwesv1alpha1.VMGPUPhaseAttached, DeviceID: "gpu-0", LastTransitionAt: &now},
	}
	lease := &gnwesv1alpha1.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: "lease-1", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPULeaseSpec{PoolRef: "pool-a", TargetPod: "actor-1"},
	}
	c := newFakeClient(t, pod, wp, attachment, lease)
	r := &GPULeaseReconciler{Client: c, Manager: pool.NewManager()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "lease-1", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1 (finalizer): %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2 (bind): %v", err)
	}

	var bound gnwesv1alpha1.GPULease
	_ = c.Get(context.Background(), req.NamespacedName, &bound)
	if bound.Status.Phase != gnwesv1alpha1.LeasePhaseBound {
		t.Fatalf("setup: expected Bound, got %q", bound.Status.Phase)
	}
	if err := c.Delete(context.Background(), &bound); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 3 (cleanup): %v", err)
	}

	var gotAttachment gnwesv1alpha1.VMGPUAttachment
	_ = c.Get(context.Background(), types.NamespacedName{Name: "actor-1", Namespace: "ns"}, &gotAttachment)
	if gotAttachment.Spec.DesiredState != gnwesv1alpha1.VMGPUDesiredAttached {
		t.Fatalf("deleting a GPULease must not force a detach; expected DesiredState to stay Attached (the bridge's call), got %q", gotAttachment.Spec.DesiredState)
	}
}
