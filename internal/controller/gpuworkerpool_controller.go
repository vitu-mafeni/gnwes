package controller

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
	"gnwes/internal/pool"
)

// GPUWorkerPoolReconciler keeps a GPUWorkerPool's status in sync with the
// in-memory pool.Manager, which is the actual source of truth for slot state.
type GPUWorkerPoolReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Manager *pool.Manager
}

// +kubebuilder:rbac:groups=gnwes.io,resources=gpuworkerpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gnwes.io,resources=gpuworkerpools/status,verbs=get;update;patch

func (r *GPUWorkerPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var wp gnwesv1alpha1.GPUWorkerPool
	if err := r.Get(ctx, req.NamespacedName, &wp); err != nil {
		if client.IgnoreNotFound(err) == nil {
			r.Manager.DeletePool(req.NamespacedName.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	slotInit := wp.Spec.SlotInitMillis
	if slotInit <= 0 {
		slotInit = 4000
	}
	attachMs := wp.Spec.AttachLatencyMillis
	if attachMs <= 0 {
		attachMs = 150
	}

	key := req.NamespacedName.String()
	p := r.Manager.EnsurePool(key, wp.Spec.GPUModel, slotInit, attachMs)
	p.Resize(wp.Spec.PoolSize)

	total, available, leased, initializing := p.Counts()

	phase := gnwesv1alpha1.PoolPhaseWarming
	if initializing == 0 && total >= wp.Spec.PoolSize {
		phase = gnwesv1alpha1.PoolPhaseReady
	}

	wp.Status.Phase = phase
	wp.Status.TotalSlots = total
	wp.Status.AvailableSlots = available
	wp.Status.LeasedSlots = leased
	meta := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "Warming",
		Message:            "pool is still warming up slots",
		LastTransitionTime: metav1.Now(),
	}
	if phase == gnwesv1alpha1.PoolPhaseReady {
		meta.Status = metav1.ConditionTrue
		meta.Reason = "Ready"
		meta.Message = "all slots warm and available for lease"
	}
	setCondition(&wp.Status.Conditions, meta)

	if err := r.Status().Update(ctx, &wp); err != nil {
		return ctrl.Result{}, err
	}

	if phase == gnwesv1alpha1.PoolPhaseWarming {
		logger.Info("pool warming", "pool", key, "total", total, "available", available, "initializing", initializing)
		return ctrl.Result{RequeueAfter: 300 * time.Millisecond}, nil
	}

	return ctrl.Result{}, nil
}

func setCondition(conditions *[]metav1.Condition, newCond metav1.Condition) {
	for i, c := range *conditions {
		if c.Type == newCond.Type {
			if c.Status != newCond.Status {
				(*conditions)[i] = newCond
			}
			return
		}
	}
	*conditions = append(*conditions, newCond)
}

func (r *GPUWorkerPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gnwesv1alpha1.GPUWorkerPool{}).
		Named("gpuworkerpool").
		Complete(r)
}
