package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
	"gnwes/internal/pool"
)

// GPUWorkerPoolReconciler keeps a GPUWorkerPool's status in sync with its
// real backend: for Backend=Shared, the in-memory admission-budget
// pool.Manager; for Backend=Exclusive, the real VFIOGPUPool/VMGPUAttachment
// CRs, whose status is itself real (see internal/vfiopool,
// vfiogpupool_controller.go).
type GPUWorkerPoolReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Manager *pool.Manager
}

// +kubebuilder:rbac:groups=gnwes.io,resources=gpuworkerpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gnwes.io,resources=gpuworkerpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gnwes.io,resources=vfiogpupools,verbs=get;list;watch
// +kubebuilder:rbac:groups=gnwes.io,resources=vmgpuattachments,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

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

	if wp.Spec.Backend == gnwesv1alpha1.BackendExclusive {
		return r.reconcileExclusive(ctx, &wp, logger)
	}
	return r.reconcileShared(ctx, &wp)
}

// reconcileShared maintains the admission-budget ticket count for a real,
// Kubernetes-scheduled GPU resource (see internal/pool's doc comment). It
// does not and cannot discover real device identity — that's the
// scheduler's and device plugin's job, not GNWES's.
func (r *GPUWorkerPoolReconciler) reconcileShared(ctx context.Context, wp *gnwesv1alpha1.GPUWorkerPool) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(wp).String()
	p := r.Manager.EnsurePool(key, wp.Spec.GPUModel)
	p.Resize(wp.Spec.PoolSize)

	total, available, leased := p.Counts()

	wp.Status.Phase = gnwesv1alpha1.PoolPhaseReady
	wp.Status.TotalSlots = total
	wp.Status.AvailableSlots = available
	wp.Status.LeasedSlots = leased
	setCondition(&wp.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		Message:            "admission budget provisioned; real GPU binding is Kubernetes' own scheduler's job",
		LastTransitionTime: metav1.Now(),
	})

	return ctrl.Result{}, r.Status().Update(ctx, wp)
}

// reconcileExclusive ensures one VMGPUAttachment exists per real worker pod
// in this pool (created Detached, with an empty apiSocket -- the real
// microVM worker pod reuses its VM lifecycle across many sequential actors,
// so there is no single socket path to seed at creation time; a
// cmd/substrate-gpu-bridge sidecar in that same pod resolves the currently
// assigned actor's UID and writes both DesiredState and apiSocket together
// as actors come and go), and mirrors status straight from the real
// VFIOGPUPool this pool draws from.
func (r *GPUWorkerPoolReconciler) reconcileExclusive(ctx context.Context, wp *gnwesv1alpha1.GPUWorkerPool, logger logr.Logger) (ctrl.Result, error) {
	if wp.Spec.VFIOPoolRef == "" {
		wp.Status.Phase = gnwesv1alpha1.PoolPhaseWarming
		setCondition(&wp.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "MissingVFIOPoolRef",
			Message:            "spec.vfioPoolRef is required for Backend=Exclusive",
			LastTransitionTime: metav1.Now(),
		})
		return ctrl.Result{}, r.Status().Update(ctx, wp)
	}

	var vfioPool gnwesv1alpha1.VFIOGPUPool
	if err := r.Get(ctx, types.NamespacedName{Namespace: wp.Namespace, Name: wp.Spec.VFIOPoolRef}, &vfioPool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(wp.Namespace), client.MatchingLabels{labelPoolRef: wp.Name}); err != nil {
		return ctrl.Result{}, err
	}

	for i := range pods.Items {
		p := &pods.Items[i]
		var existing gnwesv1alpha1.VMGPUAttachment
		err := r.Get(ctx, types.NamespacedName{Namespace: wp.Namespace, Name: p.Name}, &existing)
		if err == nil {
			continue // already created
		}
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}

		attachment := &gnwesv1alpha1.VMGPUAttachment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      p.Name,
				Namespace: wp.Namespace,
			},
			Spec: gnwesv1alpha1.VMGPUAttachmentSpec{
				PoolRef:      wp.Spec.VFIOPoolRef,
				DesiredState: gnwesv1alpha1.VMGPUDesiredDetached,
			},
		}
		if err := controllerutil.SetOwnerReference(p, attachment, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting owner reference: %w", err)
		}
		if err := r.Create(ctx, attachment); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		logger.Info("created VMGPUAttachment for real microVM worker pod", "pod", p.Name, "vfioPool", wp.Spec.VFIOPoolRef)
	}

	wp.Status.Phase = gnwesv1alpha1.PoolPhaseReady
	wp.Status.TotalSlots = vfioPool.Status.TotalDevices
	wp.Status.AvailableSlots = vfioPool.Status.AvailableDevices
	wp.Status.LeasedSlots = vfioPool.Status.AttachedDevices
	setCondition(&wp.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		Message:            fmt.Sprintf("backed by real VFIOGPUPool %q", wp.Spec.VFIOPoolRef),
		LastTransitionTime: metav1.Now(),
	})
	return ctrl.Result{}, r.Status().Update(ctx, wp)
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
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.mapPodToWorkerPool)).
		Named("gpuworkerpool").
		Complete(r)
}

// mapPodToWorkerPool re-reconciles the GPUWorkerPool a pod's labelPoolRef
// names whenever that pod is created, deleted, or otherwise changes.
// Without this, reconcileExclusive's "one VMGPUAttachment per real worker
// pod" invariant is only restored on the next change to the GPUWorkerPool
// object itself -- which can simply never happen again once a pod is
// deleted and replaced (e.g. a manual delete, or a future upgrade
// rollout): the replacement pod never gets a VMGPUAttachment, and the
// bridge sidecar inside it polls forever against one that doesn't exist.
// Confirmed live on 2026-11-13: after a worker pod was manually recreated,
// no VMGPUAttachment appeared for 3+ hours because nothing had touched the
// owning GPUWorkerPool object in the meantime.
func (r *GPUWorkerPoolReconciler) mapPodToWorkerPool(_ context.Context, obj client.Object) []reconcile.Request {
	poolName, ok := obj.GetLabels()[labelPoolRef]
	if !ok || poolName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: poolName}}}
}
