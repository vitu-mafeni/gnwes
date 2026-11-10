package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	leaseFinalizer = "gnwes.io/lease-cleanup"
	annoSlot       = "gnwes.io/gpu-slot"
	annoAttachedAt = "gnwes.io/gpu-attached-at"
)

// GPULeaseReconciler implements the attach/detach flow: it acquires a warm
// slot from the referenced GPUWorkerPool, simulates the CDI-injection attach
// cost, annotates the target pod, and releases the slot back to the pool on
// delete or TTL expiry. This is the "lease a GPU by the second" primitive.
type GPULeaseReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Manager *pool.Manager

	mu        sync.Mutex
	firstSeen map[types.UID]time.Time
}

// +kubebuilder:rbac:groups=gnwes.io,resources=gpuleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gnwes.io,resources=gpuleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch

func (r *GPULeaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var lease gnwesv1alpha1.GPULease
	if err := r.Get(ctx, req.NamespacedName, &lease); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	leaseKey := req.NamespacedName.String()
	poolKey := req.Namespace + "/" + lease.Spec.PoolRef

	// Deletion: release the slot (if any) and drop the finalizer.
	if !lease.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&lease, leaseFinalizer) {
			if lease.Status.SlotID != "" {
				if p := r.Manager.GetPool(poolKey); p != nil {
					p.Release(lease.Status.SlotID, leaseKey)
				}
				r.unannotatePod(ctx, req.Namespace, lease.Spec.TargetPod)
			}
			controllerutil.RemoveFinalizer(&lease, leaseFinalizer)
			if err := r.Update(ctx, &lease); err != nil {
				return ctrl.Result{}, err
			}
		}
		r.forgetFirstSeen(lease.UID)
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&lease, leaseFinalizer) {
		controllerutil.AddFinalizer(&lease, leaseFinalizer)
		if err := r.Update(ctx, &lease); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	requestedAt := r.markFirstSeen(lease.UID)

	// Already bound: only work left is TTL-driven auto-release.
	if lease.Status.Phase == gnwesv1alpha1.LeasePhaseBound {
		if lease.Spec.TTLSeconds == nil || lease.Status.AttachedAt == nil {
			return ctrl.Result{}, nil
		}
		ttl := time.Duration(*lease.Spec.TTLSeconds) * time.Second
		elapsed := time.Since(lease.Status.AttachedAt.Time)
		if elapsed < ttl {
			return ctrl.Result{RequeueAfter: ttl - elapsed}, nil
		}
		if p := r.Manager.GetPool(poolKey); p != nil {
			p.Release(lease.Status.SlotID, leaseKey)
		}
		r.unannotatePod(ctx, req.Namespace, lease.Spec.TargetPod)
		lease.Status.Phase = gnwesv1alpha1.LeasePhaseReleased
		lease.Status.ReleasedAt = ptrTime(metav1.Now())
		lease.Status.Message = "TTL expired; slot released back to pool"
		return ctrl.Result{}, r.Status().Update(ctx, &lease)
	}

	if lease.Status.Phase == gnwesv1alpha1.LeasePhaseReleased ||
		lease.Status.Phase == gnwesv1alpha1.LeasePhaseFailed {
		return ctrl.Result{}, nil
	}

	p := r.Manager.GetPool(poolKey)
	if p == nil {
		lease.Status.Phase = gnwesv1alpha1.LeasePhasePending
		lease.Status.Message = fmt.Sprintf("waiting for GPUWorkerPool %q to exist", lease.Spec.PoolRef)
		_ = r.Status().Update(ctx, &lease)
		return ctrl.Result{RequeueAfter: 250 * time.Millisecond}, nil
	}

	var targetPod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: lease.Spec.TargetPod}, &targetPod); err != nil {
		lease.Status.Phase = gnwesv1alpha1.LeasePhaseFailed
		lease.Status.Message = fmt.Sprintf("target pod %q not found: %v", lease.Spec.TargetPod, err)
		_ = r.Status().Update(ctx, &lease)
		return ctrl.Result{}, nil
	}

	// If a PodGateReconciler already reserved a slot for this pod ahead of
	// scheduling, inherit that exact slot instead of acquiring a second one
	// — Transfer only succeeds while the reservation is still unclaimed, so
	// this can never double-book a slot.
	var slotID string
	var ok bool
	if reserved := targetPod.Annotations[annoReservedSlot]; reserved != "" {
		reservationKey := "podgate/" + req.Namespace + "/" + lease.Spec.TargetPod
		if p.Transfer(reserved, reservationKey, leaseKey) {
			slotID, ok = reserved, true
		}
	}
	if !ok {
		slotID, ok = p.Acquire(leaseKey)
	}
	if !ok {
		lease.Status.Phase = gnwesv1alpha1.LeasePhasePending
		lease.Status.Message = "no warm slot available yet; waiting for one to free up"
		_ = r.Status().Update(ctx, &lease)
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
	}

	// Simulate the CDI-injection attach cost: this is the one number the
	// whole demo exists to keep small, however large the pool's one-time
	// warm-up cost (SlotInitMillis) was.
	time.Sleep(time.Duration(p.AttachLatencyMillis) * time.Millisecond)

	if err := r.annotatePod(ctx, &targetPod, slotID); err != nil {
		p.Release(slotID, leaseKey)
		lease.Status.Phase = gnwesv1alpha1.LeasePhaseFailed
		lease.Status.Message = fmt.Sprintf("failed to attach to pod %q: %v", lease.Spec.TargetPod, err)
		_ = r.Status().Update(ctx, &lease)
		return ctrl.Result{}, err
	}

	now := metav1.Now()
	lease.Status.Phase = gnwesv1alpha1.LeasePhaseBound
	lease.Status.SlotID = slotID
	lease.Status.AttachedAt = &now
	lease.Status.AttachLatencyMillis = time.Since(requestedAt).Milliseconds()
	lease.Status.Message = "attached"
	if err := r.Status().Update(ctx, &lease); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("lease bound", "lease", leaseKey, "slot", slotID, "attachMs", lease.Status.AttachLatencyMillis)

	if lease.Spec.TTLSeconds != nil {
		return ctrl.Result{RequeueAfter: time.Duration(*lease.Spec.TTLSeconds) * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *GPULeaseReconciler) annotatePod(ctx context.Context, pod *corev1.Pod, slotID string) error {
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[annoSlot] = slotID
	pod.Annotations[annoAttachedAt] = time.Now().Format(time.RFC3339Nano)
	delete(pod.Annotations, annoReservedSlot)
	return r.Patch(ctx, pod, patch)
}

func (r *GPULeaseReconciler) unannotatePod(ctx context.Context, namespace, podName string) {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, &pod); err != nil {
		return // best-effort: pod may already be gone
	}
	if pod.Annotations == nil {
		return
	}
	if _, ok := pod.Annotations[annoSlot]; !ok {
		return
	}
	patch := client.MergeFrom(pod.DeepCopy())
	delete(pod.Annotations, annoSlot)
	delete(pod.Annotations, annoAttachedAt)
	_ = r.Patch(ctx, &pod, patch)
}

func (r *GPULeaseReconciler) markFirstSeen(uid types.UID) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.firstSeen == nil {
		r.firstSeen = map[types.UID]time.Time{}
	}
	if t, ok := r.firstSeen[uid]; ok {
		return t
	}
	now := time.Now()
	r.firstSeen[uid] = now
	return now
}

func (r *GPULeaseReconciler) forgetFirstSeen(uid types.UID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.firstSeen, uid)
}

func ptrTime(t metav1.Time) *metav1.Time { return &t }

func (r *GPULeaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gnwesv1alpha1.GPULease{}).
		Named("gpulease").
		Complete(r)
}
