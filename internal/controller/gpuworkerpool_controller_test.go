package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
	"gnwes/internal/pool"
)

// TestGPUWorkerPoolReconcile_ExclusiveCreatesAttachmentWithEmptyAPISocket is
// the regression test for the fix to the stale-socket bug: a microVM worker
// pod reuses its VM lifecycle across many sequential actors, so there is no
// correct socket path to seed at VMGPUAttachment creation time (unlike the
// old static gnwes.io/ch-api-socket annotation this reconciler used to
// require). The attachment must still be created right away, with an empty
// apiSocket -- cmd/substrate-gpu-bridge fills it in once it resolves which
// actor is actually assigned to this pod.
func TestGPUWorkerPoolReconcile_ExclusiveCreatesAttachmentWithEmptyAPISocket(t *testing.T) {
	wp := exclusivePool("pool-x", "ns", "vfio-pool-a")
	vfioPool := &gnwesv1alpha1.VFIOGPUPool{
		ObjectMeta: metav1.ObjectMeta{Name: "vfio-pool-a", Namespace: "ns"},
		Status:     gnwesv1alpha1.VFIOGPUPoolStatus{TotalDevices: 1, AvailableDevices: 1},
	}
	pod := newTestPod("worker-1", "ns")
	pod.Labels = map[string]string{labelPoolRef: "pool-x"}
	c := newFakeClient(t, wp, vfioPool, pod)
	r := &GPUWorkerPoolReconciler{Client: c, Scheme: testScheme(t), Manager: pool.NewManager()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "pool-x", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var att gnwesv1alpha1.VMGPUAttachment
	if err := c.Get(context.Background(), types.NamespacedName{Name: "worker-1", Namespace: "ns"}, &att); err != nil {
		t.Fatalf("expected a VMGPUAttachment to be created immediately, got: %v", err)
	}
	if att.Spec.APISocket != "" {
		t.Fatalf("expected an empty apiSocket at creation (bridge-managed), got %q", att.Spec.APISocket)
	}
	if att.Spec.DesiredState != gnwesv1alpha1.VMGPUDesiredDetached {
		t.Fatalf("expected DesiredState Detached at creation, got %q", att.Spec.DesiredState)
	}
	if att.Spec.PoolRef != "vfio-pool-a" {
		t.Fatalf("expected poolRef vfio-pool-a, got %q", att.Spec.PoolRef)
	}
}

func TestGPUWorkerPoolReconcile_SharedIsReadyImmediately(t *testing.T) {
	wp := &gnwesv1alpha1.GPUWorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPUWorkerPoolSpec{PoolSize: 2},
	}
	c := newFakeClient(t, wp)
	r := &GPUWorkerPoolReconciler{Client: c, Manager: pool.NewManager()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "pool-a", Namespace: "ns"}}

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue: there is no warm-up delay to simulate any more")
	}

	var got gnwesv1alpha1.GPUWorkerPool
	if err := c.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != gnwesv1alpha1.PoolPhaseReady {
		t.Fatalf("expected phase Ready, got %q", got.Status.Phase)
	}
	if got.Status.TotalSlots != 2 || got.Status.AvailableSlots != 2 || got.Status.LeasedSlots != 0 {
		t.Fatalf("unexpected slot counts: total=%d available=%d leased=%d",
			got.Status.TotalSlots, got.Status.AvailableSlots, got.Status.LeasedSlots)
	}
	found := false
	for _, cond := range got.Status.Conditions {
		if cond.Type == "Ready" {
			found = true
			if cond.Status != metav1.ConditionTrue {
				t.Fatalf("expected Ready condition True, got %s", cond.Status)
			}
		}
	}
	if !found {
		t.Fatalf("expected a Ready condition to be set")
	}
}

func TestGPUWorkerPoolReconcile_DeleteClearsManagerState(t *testing.T) {
	c := newFakeClient(t)
	mgr := pool.NewManager()
	r := &GPUWorkerPoolReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "gone", Namespace: "ns"}}

	// Pre-populate manager state as if the pool had existed before.
	mgr.EnsurePool("ns/gone", "model").Resize(1)
	if mgr.GetPool("ns/gone") == nil {
		t.Fatalf("setup: expected pool to exist in the manager")
	}

	// Reconciling a NotFound object (nothing in the fake client) must clear it.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if mgr.GetPool("ns/gone") != nil {
		t.Fatalf("expected the manager's pool state to be cleared on delete")
	}
}

func TestGPUWorkerPoolReconcile_ZeroPoolSizeIsValid(t *testing.T) {
	wp := &gnwesv1alpha1.GPUWorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-b", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPUWorkerPoolSpec{PoolSize: 0},
	}
	c := newFakeClient(t, wp)
	mgr := pool.NewManager()
	r := &GPUWorkerPoolReconciler{Client: c, Manager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "pool-b", Namespace: "ns"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	p := mgr.GetPool("ns/pool-b")
	if p == nil {
		t.Fatalf("expected a pool to be created even with PoolSize 0")
	}
	if total, _, _ := p.Counts(); total != 0 {
		t.Fatalf("expected zero tickets, got %d", total)
	}
}
