package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
	"gnwes/internal/pool"
)

const (
	// gateName is the scheduling gate GNWES-aware actor pods are created
	// with. The default scheduler will not consider a pod for scheduling
	// while any gate remains in spec.schedulingGates — a stable, GA
	// Kubernetes extension point (KEP-3521), not a scheduler-plugin or
	// kube-scheduler-fork requirement.
	gateName = "gnwes.io/gpu-capacity"

	// labelPoolRef names which GPUWorkerPool (same namespace) a gated pod
	// needs capacity in before it is allowed to be scheduled.
	labelPoolRef = "gnwes.io/gpu-pool"

	// annoReservedSlot records the slot this controller reserved for the pod
	// ahead of scheduling. The GPULease created for the pod afterwards
	// inherits (Transfers) this reservation instead of acquiring a second,
	// independent slot. Backend=Shared only.
	annoReservedSlot = "gnwes.io/reserved-slot"

	gateFinalizer = "gnwes.io/gate-cleanup"
)

// PodGateReconciler is the admission half of GNWES: it holds a GPU-needing
// actor pod un-schedulable until a warm slot in its target pool is actually
// reserved for it, so the cluster never schedules (and starts billing node
// resources for) an agent that has nothing to attach to. Once reserved, it
// removes the pod's scheduling gate so the default scheduler proceeds
// normally; the GPULease created for the pod later claims this exact
// reservation.
type PodGateReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Manager *pool.Manager
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gnwes.io,resources=gpuworkerpools;vfiogpupools,verbs=get;list;watch

func (r *PodGateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	reservationKey := "podgate/" + req.Namespace + "/" + req.Name

	if !pod.DeletionTimestamp.IsZero() {
		return r.cleanupOnDelete(ctx, &pod, reservationKey)
	}

	if !hasGate(&pod) {
		return ctrl.Result{}, nil
	}

	poolRef := pod.Labels[labelPoolRef]
	if poolRef == "" {
		logger.Info("pod carries the gnwes gate but no gnwes.io/gpu-pool label; removing gate with no reservation", "pod", req.String())
		removeGate(&pod)
		return ctrl.Result{}, r.Update(ctx, &pod)
	}

	var wp gnwesv1alpha1.GPUWorkerPool
	if err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: poolRef}, &wp); err != nil {
		return ctrl.Result{RequeueAfter: 250 * time.Millisecond}, client.IgnoreNotFound(err)
	}

	if wp.Spec.Backend == gnwesv1alpha1.BackendExclusive {
		return r.admitExclusive(ctx, &pod, &wp, reservationKey, logger)
	}

	p := r.Manager.GetPool(req.Namespace + "/" + poolRef)
	if p == nil {
		return ctrl.Result{RequeueAfter: 250 * time.Millisecond}, nil
	}

	slotID, ok := p.Acquire(reservationKey)
	if !ok {
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
	}

	controllerutil.AddFinalizer(&pod, gateFinalizer)
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[annoReservedSlot] = slotID
	removeGate(&pod)

	if err := r.Update(ctx, &pod); err != nil {
		p.Release(slotID, reservationKey)
		return ctrl.Result{}, err
	}

	logger.Info("pod admitted: GPU budget ticket reserved ahead of scheduling", "pod", req.String(), "pool", poolRef, "ticket", slotID)
	return ctrl.Result{}, nil
}

// admitExclusive gates admission on the referenced VFIOGPUPool's REAL
// available-device count — no in-memory bookkeeping, since VFIOGPUPoolReconciler
// already mirrors live vfiopool.Manager state into that CR's status. This
// only bounds how many microVM worker pods get admitted; the actual device
// hot-attach happens later, driven by GPULeaseReconciler.
func (r *PodGateReconciler) admitExclusive(ctx context.Context, pod *corev1.Pod, wp *gnwesv1alpha1.GPUWorkerPool, reservationKey string, logger logr.Logger) (ctrl.Result, error) {
	if wp.Spec.VFIOPoolRef == "" {
		logger.Info("GPUWorkerPool is Backend=Exclusive but has no vfioPoolRef; removing gate with no reservation", "pool", wp.Name)
		removeGate(pod)
		return ctrl.Result{}, r.Update(ctx, pod)
	}
	var vfioPool gnwesv1alpha1.VFIOGPUPool
	if err := r.Get(ctx, types.NamespacedName{Namespace: wp.Namespace, Name: wp.Spec.VFIOPoolRef}, &vfioPool); err != nil {
		return ctrl.Result{RequeueAfter: 250 * time.Millisecond}, client.IgnoreNotFound(err)
	}
	if vfioPool.Status.AvailableDevices < 1 {
		return ctrl.Result{RequeueAfter: 250 * time.Millisecond}, nil
	}

	controllerutil.AddFinalizer(pod, gateFinalizer)
	removeGate(pod)
	if err := r.Update(ctx, pod); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("pod admitted: real VFIO device capacity available in pool", "pod", pod.Namespace+"/"+pod.Name, "vfioPool", vfioPool.Name, "available", vfioPool.Status.AvailableDevices)
	return ctrl.Result{}, nil
}

// cleanupOnDelete releases a reservation that was never claimed by a
// GPULease (e.g. the pod was deleted before one got created). If the
// reservation WAS already transferred to a lease, Release is a no-op here
// since the slot's ownership key no longer matches reservationKey — the
// lease's own finalizer owns releasing it at that point.
func (r *PodGateReconciler) cleanupOnDelete(ctx context.Context, pod *corev1.Pod, reservationKey string) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(pod, gateFinalizer) {
		return ctrl.Result{}, nil
	}
	if slotID := pod.Annotations[annoReservedSlot]; slotID != "" {
		if poolRef := pod.Labels[labelPoolRef]; poolRef != "" {
			if p := r.Manager.GetPool(pod.Namespace + "/" + poolRef); p != nil {
				p.Release(slotID, reservationKey)
			}
		}
	}
	controllerutil.RemoveFinalizer(pod, gateFinalizer)
	return ctrl.Result{}, r.Update(ctx, pod)
}

func hasGate(pod *corev1.Pod) bool {
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name == gateName {
			return true
		}
	}
	return false
}

// removeGate filters gateName out of the pod's scheduling gates in place.
// The scheduling-gates API only ever allows removing entries post-creation,
// never adding them, so this is the only mutation this controller makes to
// spec.schedulingGates.
func removeGate(pod *corev1.Pod) {
	kept := pod.Spec.SchedulingGates[:0]
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name != gateName {
			kept = append(kept, g)
		}
	}
	pod.Spec.SchedulingGates = kept
}

func (r *PodGateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Named("podgate").
		Complete(r)
}
