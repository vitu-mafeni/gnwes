package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
	"gnwes/internal/vfiopool"
)

// VFIOGPUPoolReconciler keeps a VFIOGPUPool's status in sync with the
// in-memory vfiopool.Manager, the real source of truth for device state --
// same relationship GPUWorkerPoolReconciler has to internal/pool, but for
// real, physical VFIO devices instead of simulated slots. Spec.Devices is a
// fixed inventory (RegisterDevice is idempotent), not something grown toward
// a target size the way GPUWorkerPool's Resize is.
type VFIOGPUPoolReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	VFIOManager *vfiopool.Manager
}

// +kubebuilder:rbac:groups=gnwes.io,resources=vfiogpupools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gnwes.io,resources=vfiogpupools/status,verbs=get;update;patch

func (r *VFIOGPUPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var vp gnwesv1alpha1.VFIOGPUPool
	if err := r.Get(ctx, req.NamespacedName, &vp); err != nil {
		if client.IgnoreNotFound(err) == nil {
			r.VFIOManager.DeletePool(req.NamespacedName.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	key := req.NamespacedName.String()
	p := r.VFIOManager.EnsurePool(key)
	for _, d := range vp.Spec.Devices {
		p.RegisterDevice(d.ID, d.PCIPath)
	}

	total, available, attached := p.Counts()
	vp.Status.TotalDevices = total
	vp.Status.AvailableDevices = available
	vp.Status.AttachedDevices = attached

	if err := r.Status().Update(ctx, &vp); err != nil {
		return ctrl.Result{}, err
	}

	// VMGPUAttachment reconciles mutate this pool's in-memory device state
	// directly (vfiopool.Manager, not this CR), so without a periodic
	// resync this status would only refresh when the VFIOGPUPool object
	// itself next changes -- stale counts, not stale bookkeeping (the
	// actual Acquire/Release calls are synchronous and correct the moment
	// they happen; only this CR's reported snapshot of them lags).
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *VFIOGPUPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gnwesv1alpha1.VFIOGPUPool{}).
		Named("vfiogpupool").
		Complete(r)
}
