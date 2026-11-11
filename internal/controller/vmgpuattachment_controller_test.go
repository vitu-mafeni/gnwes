package controller

import (
	"context"
	"fmt"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
	"gnwes/internal/chclient"
	"gnwes/internal/vfiopool"
)

// fakeCH is an in-memory stand-in for a cloud-hypervisor VMM, used so these
// tests exercise the reconciler's real logic (pool bookkeeping, status
// transitions, idempotency, finalizer handling) without dialing a real
// unix socket. internal/chclient's own tests already prove the real
// transport/wire-format half works against a real unix-socket HTTP server.
type fakeCH struct {
	mu        sync.Mutex
	added     map[string]chclient.DeviceConfig
	removed   []string
	addErr    error
	removeErr error
	bdfPrefix string
}

func newFakeCH() *fakeCH {
	return &fakeCH{added: map[string]chclient.DeviceConfig{}, bdfPrefix: "0000:00:0"}
}

func (f *fakeCH) AddDevice(_ context.Context, cfg chclient.DeviceConfig) (chclient.PciDeviceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return chclient.PciDeviceInfo{}, f.addErr
	}
	if _, exists := f.added[cfg.ID]; exists {
		return chclient.PciDeviceInfo{}, fmt.Errorf("device with id %q already exists", cfg.ID)
	}
	f.added[cfg.ID] = cfg
	return chclient.PciDeviceInfo{ID: cfg.ID, BDF: fmt.Sprintf("%s%d.0", f.bdfPrefix, len(f.added))}, nil
}

func (f *fakeCH) RemoveDevice(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	if _, exists := f.added[id]; !exists {
		return fmt.Errorf("no device with id %q", id)
	}
	delete(f.added, id)
	f.removed = append(f.removed, id)
	return nil
}

func newVMGPUTestObjects(t *testing.T) (att *gnwesv1alpha1.VMGPUAttachment, mgr *vfiopool.Manager, pool *vfiopool.Pool) {
	t.Helper()
	att = &gnwesv1alpha1.VMGPUAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "att-a", Namespace: "ns1", UID: "uid-att-a"},
		Spec: gnwesv1alpha1.VMGPUAttachmentSpec{
			PoolRef:      "pool-a",
			APISocket:    "/run/vc/vm/x/clh-api.sock",
			DesiredState: gnwesv1alpha1.VMGPUDesiredAttached,
		},
	}
	mgr = vfiopool.NewManager()
	pool = mgr.EnsurePool("ns1/pool-a")
	pool.RegisterDevice("gpu-0", "/sys/bus/pci/devices/0000:01:00.0/")
	return att, mgr, pool
}

func TestVMGPUAttachment_AttachesRealDeviceViaCloudHypervisor(t *testing.T) {
	att, mgr, pool := newVMGPUTestObjects(t)
	fc := newFakeClient(t, att)
	ch := newFakeCH()
	r := &VMGPUAttachmentReconciler{
		Client:      fc,
		VFIOManager: mgr,
		NewCHClient: func(string) CHDevicePlugger { return ch },
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "att-a", Namespace: "ns1"}}

	// First reconcile only adds the finalizer (matches GPULeaseReconciler's
	// own pattern: finalizer-then-requeue before doing real work).
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first Reconcile (finalizer): %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second Reconcile (attach): %v", err)
	}

	var got gnwesv1alpha1.VMGPUAttachment
	if err := fc.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != gnwesv1alpha1.VMGPUPhaseAttached {
		t.Fatalf("Phase = %q, want Attached (message: %s)", got.Status.Phase, got.Status.Message)
	}
	if got.Status.DeviceID != "gpu-0" {
		t.Fatalf("DeviceID = %q, want gpu-0", got.Status.DeviceID)
	}
	if got.Status.GuestBDF == "" {
		t.Fatal("expected GuestBDF to be populated from the (fake) cloud-hypervisor response")
	}

	// The real effect: cloud-hypervisor (faked) actually has the device.
	ch.mu.Lock()
	_, added := ch.added["gpu-0"]
	ch.mu.Unlock()
	if !added {
		t.Fatal("expected AddDevice to have been called for gpu-0")
	}

	// And the pool reflects it as attached, not available.
	d, _ := pool.Get("gpu-0")
	if d.State != vfiopool.DeviceAttached {
		t.Fatalf("pool device state = %v, want Attached", d.State)
	}
}

func TestVMGPUAttachment_ReconcileIsIdempotentOnceAttached(t *testing.T) {
	att, mgr, _ := newVMGPUTestObjects(t)
	fc := newFakeClient(t, att)
	ch := newFakeCH()
	r := &VMGPUAttachmentReconciler{Client: fc, VFIOManager: mgr, NewCHClient: func(string) CHDevicePlugger { return ch }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "att-a", Namespace: "ns1"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 3 (should be a no-op): %v", err)
	}

	ch.mu.Lock()
	addCount := len(ch.added)
	ch.mu.Unlock()
	if addCount != 1 {
		t.Fatalf("AddDevice effectively called more than once: %d devices added", addCount)
	}
}

func TestVMGPUAttachment_DetachReleasesDeviceBackToPool(t *testing.T) {
	att, mgr, pool := newVMGPUTestObjects(t)
	fc := newFakeClient(t, att)
	ch := newFakeCH()
	r := &VMGPUAttachmentReconciler{Client: fc, VFIOManager: mgr, NewCHClient: func(string) CHDevicePlugger { return ch }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "att-a", Namespace: "ns1"}}

	// Get to Attached first.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	// Flip to Detached, matching how a resume/suspend bridge would drive this.
	var got gnwesv1alpha1.VMGPUAttachment
	if err := fc.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Spec.DesiredState = gnwesv1alpha1.VMGPUDesiredDetached
	if err := fc.Update(context.Background(), &got); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (detach): %v", err)
	}

	if err := fc.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("Get after detach: %v", err)
	}
	if got.Status.Phase != gnwesv1alpha1.VMGPUPhaseDetached {
		t.Fatalf("Phase = %q, want Detached (message: %s)", got.Status.Phase, got.Status.Message)
	}
	if got.Status.DeviceID != "" {
		t.Fatalf("DeviceID = %q, want empty after detach", got.Status.DeviceID)
	}

	ch.mu.Lock()
	removed := len(ch.removed) == 1 && ch.removed[0] == "gpu-0"
	ch.mu.Unlock()
	if !removed {
		t.Fatalf("expected RemoveDevice(gpu-0) to have been called exactly once, got %+v", ch.removed)
	}

	d, ok := pool.Get("gpu-0")
	if !ok || d.State != vfiopool.DeviceAvailable {
		t.Fatalf("device after detach = %+v (ok=%v), want Available", d, ok)
	}
}

func TestVMGPUAttachment_ReAttachAfterDetachAcquiresAgain(t *testing.T) {
	att, mgr, _ := newVMGPUTestObjects(t)
	fc := newFakeClient(t, att)
	ch := newFakeCH()
	r := &VMGPUAttachmentReconciler{Client: fc, VFIOManager: mgr, NewCHClient: func(string) CHDevicePlugger { return ch }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "att-a", Namespace: "ns1"}}

	reconcileUntilSettled := func(wantPhase gnwesv1alpha1.VMGPUAttachmentPhase) {
		t.Helper()
		for i := 0; i < 3; i++ {
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
		}
		var got gnwesv1alpha1.VMGPUAttachment
		if err := fc.Get(context.Background(), req.NamespacedName, &got); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status.Phase != wantPhase {
			t.Fatalf("Phase = %q, want %q (message: %s)", got.Status.Phase, wantPhase, got.Status.Message)
		}
	}

	reconcileUntilSettled(gnwesv1alpha1.VMGPUPhaseAttached)

	var got gnwesv1alpha1.VMGPUAttachment
	_ = fc.Get(context.Background(), req.NamespacedName, &got)
	got.Spec.DesiredState = gnwesv1alpha1.VMGPUDesiredDetached
	_ = fc.Update(context.Background(), &got)
	reconcileUntilSettled(gnwesv1alpha1.VMGPUPhaseDetached)

	_ = fc.Get(context.Background(), req.NamespacedName, &got)
	got.Spec.DesiredState = gnwesv1alpha1.VMGPUDesiredAttached
	_ = fc.Update(context.Background(), &got)
	reconcileUntilSettled(gnwesv1alpha1.VMGPUPhaseAttached)

	ch.mu.Lock()
	addCount := len(ch.added)
	removeCount := len(ch.removed)
	ch.mu.Unlock()
	if addCount != 1 {
		t.Fatalf("expected exactly 1 device currently added after attach-detach-reattach, got %d", addCount)
	}
	if removeCount != 1 {
		t.Fatalf("expected exactly 1 RemoveDevice call across the whole cycle, got %d", removeCount)
	}
}

func TestVMGPUAttachment_AddDeviceFailureReleasesDeviceAndSetsFailed(t *testing.T) {
	att, mgr, pool := newVMGPUTestObjects(t)
	fc := newFakeClient(t, att)
	ch := newFakeCH()
	ch.addErr = fmt.Errorf("simulated vm.add-device failure")
	r := &VMGPUAttachmentReconciler{Client: fc, VFIOManager: mgr, NewCHClient: func(string) CHDevicePlugger { return ch }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "att-a", Namespace: "ns1"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	var got gnwesv1alpha1.VMGPUAttachment
	if err := fc.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != gnwesv1alpha1.VMGPUPhaseFailed {
		t.Fatalf("Phase = %q, want Failed", got.Status.Phase)
	}

	// Critically: the device must NOT be left stuck in Attached in the pool
	// just because the real hotplug call failed.
	d, ok := pool.Get("gpu-0")
	if !ok || d.State != vfiopool.DeviceAvailable {
		t.Fatalf("device after failed AddDevice = %+v (ok=%v), want Available (released back)", d, ok)
	}
}

func TestVMGPUAttachment_DeleteWhileAttachedDetachesAndReleases(t *testing.T) {
	att, mgr, pool := newVMGPUTestObjects(t)
	fc := newFakeClient(t, att)
	ch := newFakeCH()
	r := &VMGPUAttachmentReconciler{Client: fc, VFIOManager: mgr, NewCHClient: func(string) CHDevicePlugger { return ch }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "att-a", Namespace: "ns1"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2 (attach): %v", err)
	}

	var got gnwesv1alpha1.VMGPUAttachment
	if err := fc.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := fc.Delete(context.Background(), &got); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (delete): %v", err)
	}

	ch.mu.Lock()
	removed := len(ch.removed)
	ch.mu.Unlock()
	if removed != 1 {
		t.Fatalf("expected RemoveDevice to be called once on delete-while-attached, got %d calls", removed)
	}

	d, ok := pool.Get("gpu-0")
	if !ok || d.State != vfiopool.DeviceAvailable {
		t.Fatalf("device after delete = %+v (ok=%v), want released back to Available", d, ok)
	}

	if err := fc.Get(context.Background(), req.NamespacedName, &gnwesv1alpha1.VMGPUAttachment{}); err == nil {
		t.Fatal("expected the VMGPUAttachment object to be gone after finalizer removal")
	}
}

func TestVMGPUAttachment_NoDeviceAvailableStaysPending(t *testing.T) {
	att, mgr, pool := newVMGPUTestObjects(t)
	// Exhaust the only device before the reconciler ever runs.
	pool.Acquire("someone-else")

	fc := newFakeClient(t, att)
	ch := newFakeCH()
	r := &VMGPUAttachmentReconciler{Client: fc, VFIOManager: mgr, NewCHClient: func(string) CHDevicePlugger { return ch }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "att-a", Namespace: "ns1"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected a requeue while waiting for a device to free up")
	}

	var got gnwesv1alpha1.VMGPUAttachment
	if err := fc.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != gnwesv1alpha1.VMGPUPhasePending {
		t.Fatalf("Phase = %q, want Pending", got.Status.Phase)
	}
}
