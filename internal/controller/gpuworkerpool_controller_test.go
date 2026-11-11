package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
	"gnwes/internal/pool"
)

func TestGPUWorkerPoolReconcile_WarmsUpThenReady(t *testing.T) {
	wp := &gnwesv1alpha1.GPUWorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "ns"},
		Spec: gnwesv1alpha1.GPUWorkerPoolSpec{
			PoolSize:            2,
			SlotInitMillis:      20,
			AttachLatencyMillis: 10,
		},
	}
	c := newFakeClient(t, wp)
	r := &GPUWorkerPoolReconciler{Client: c, Manager: pool.NewManager()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "pool-a", Namespace: "ns"}}

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected a requeue while warming")
	}

	var got gnwesv1alpha1.GPUWorkerPool
	if err := c.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != gnwesv1alpha1.PoolPhaseWarming {
		t.Fatalf("expected phase Warming immediately after creation, got %q", got.Status.Phase)
	}

	time.Sleep(40 * time.Millisecond)

	res, err = r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no further requeue once Ready")
	}

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
	mgr.EnsurePool("ns/gone", "model", 100, 10).Resize(1)
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

func TestGPUWorkerPoolReconcile_DefaultsAppliedWhenUnset(t *testing.T) {
	wp := &gnwesv1alpha1.GPUWorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-b", Namespace: "ns"},
		Spec:       gnwesv1alpha1.GPUWorkerPoolSpec{PoolSize: 0}, // SlotInitMillis/AttachLatencyMillis left zero
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
	if p.SlotInitMillis != 4000 || p.AttachLatencyMillis != 150 {
		t.Fatalf("expected documented defaults (4000/150), got slotInit=%d attach=%d", p.SlotInitMillis, p.AttachLatencyMillis)
	}
}
