package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
	"gnwes/internal/vfiopool"
)

func TestVFIOGPUPoolReconcile_RegistersDevicesAndReportsCounts(t *testing.T) {
	vp := &gnwesv1alpha1.VFIOGPUPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "ns1"},
		Spec: gnwesv1alpha1.VFIOGPUPoolSpec{
			NodeName: "test-node",
			Devices: []gnwesv1alpha1.VFIODeviceSpec{
				{ID: "gpu-0", PCIPath: "/sys/bus/pci/devices/0000:01:00.0/"},
				{ID: "gpu-1", PCIPath: "/sys/bus/pci/devices/0000:02:00.0/"},
			},
		},
	}
	fc := newFakeClient(t, vp)
	mgr := vfiopool.NewManager()
	r := &VFIOGPUPoolReconciler{Client: fc, VFIOManager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "pool-a", Namespace: "ns1"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got gnwesv1alpha1.VFIOGPUPool
	if err := fc.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.TotalDevices != 2 || got.Status.AvailableDevices != 2 || got.Status.AttachedDevices != 0 {
		t.Fatalf("status = %+v, want total=2 available=2 attached=0", got.Status)
	}

	p := mgr.GetPool("ns1/pool-a")
	if p == nil {
		t.Fatal("expected vfiopool.Pool to exist after reconcile")
	}
	if d, ok := p.Get("gpu-0"); !ok || d.PCIPath != "/sys/bus/pci/devices/0000:01:00.0/" {
		t.Fatalf("gpu-0 = %+v, ok=%v", d, ok)
	}
}

func TestVFIOGPUPoolReconcile_IsIdempotentAndReflectsAttachedCount(t *testing.T) {
	vp := &gnwesv1alpha1.VFIOGPUPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "ns1"},
		Spec: gnwesv1alpha1.VFIOGPUPoolSpec{
			NodeName: "test-node",
			Devices:  []gnwesv1alpha1.VFIODeviceSpec{{ID: "gpu-0", PCIPath: "/sys/bus/pci/devices/0000:01:00.0/"}},
		},
	}
	fc := newFakeClient(t, vp)
	mgr := vfiopool.NewManager()
	r := &VFIOGPUPoolReconciler{Client: fc, VFIOManager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "pool-a", Namespace: "ns1"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	// Simulate an attachment having acquired the device out-of-band.
	mgr.GetPool("ns1/pool-a").Acquire("attachment/x")

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	var got gnwesv1alpha1.VFIOGPUPool
	if err := fc.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.TotalDevices != 1 || got.Status.AvailableDevices != 0 || got.Status.AttachedDevices != 1 {
		t.Fatalf("status = %+v, want total=1 available=0 attached=1", got.Status)
	}
}

func TestVFIOGPUPoolReconcile_DeleteClearsPoolState(t *testing.T) {
	vp := &gnwesv1alpha1.VFIOGPUPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "ns1"},
		Spec: gnwesv1alpha1.VFIOGPUPoolSpec{
			NodeName: "test-node",
			Devices:  []gnwesv1alpha1.VFIODeviceSpec{{ID: "gpu-0", PCIPath: "/x"}},
		},
	}
	fc := newFakeClient(t, vp)
	mgr := vfiopool.NewManager()
	r := &VFIOGPUPoolReconciler{Client: fc, VFIOManager: mgr}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "pool-a", Namespace: "ns1"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if mgr.GetPool("ns1/pool-a") == nil {
		t.Fatal("expected pool to exist")
	}

	if err := fc.Delete(context.Background(), vp); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile after delete: %v", err)
	}
	if mgr.GetPool("ns1/pool-a") != nil {
		t.Fatal("expected pool state to be removed after the VFIOGPUPool was deleted")
	}
}
