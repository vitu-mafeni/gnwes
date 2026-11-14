package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
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

// GPULeaseReconciler implements the real bind/release flow for both
// backends: for Backend=Shared it admits a real Kubernetes-scheduled GPU
// pod against the pool's budget and records what the scheduler actually
// bound; for Backend=Exclusive it drives a real VFIO hot-attach/detach by
// flipping the target pod's VMGPUAttachment. See GPULeaseSpec's doc
// comment for the exact distinction.
type GPULeaseReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Manager *pool.Manager

	mu        sync.Mutex
	firstSeen map[types.UID]time.Time
}

// +kubebuilder:rbac:groups=gnwes.io,resources=gpuleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gnwes.io,resources=gpuleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gnwes.io,resources=gpuworkerpools,verbs=get;list;watch
// +kubebuilder:rbac:groups=gnwes.io,resources=vmgpuattachments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch

func (r *GPULeaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var lease gnwesv1alpha1.GPULease
	if err := r.Get(ctx, req.NamespacedName, &lease); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var wp gnwesv1alpha1.GPUWorkerPool
	wpErr := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: lease.Spec.PoolRef}, &wp)

	if !lease.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &lease, &wp, wpErr == nil)
	}

	if !controllerutil.ContainsFinalizer(&lease, leaseFinalizer) {
		controllerutil.AddFinalizer(&lease, leaseFinalizer)
		if err := r.Update(ctx, &lease); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	requestedAt := r.markFirstSeen(lease.UID)

	if lease.Status.Phase == gnwesv1alpha1.LeasePhaseBound {
		return r.reconcileTTL(ctx, &lease, &wp, wpErr == nil)
	}
	if lease.Status.Phase == gnwesv1alpha1.LeasePhaseReleased || lease.Status.Phase == gnwesv1alpha1.LeasePhaseFailed {
		return ctrl.Result{}, nil
	}

	if wpErr != nil {
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

	if wp.Spec.Backend == gnwesv1alpha1.BackendExclusive {
		return r.bindExclusive(ctx, &lease, &targetPod, requestedAt, logger)
	}
	return r.bindShared(ctx, &lease, &targetPod, requestedAt, logger)
}

// bindShared admits targetPod against the pool's admission budget and waits
// for it to actually reach Running — i.e. for Kubernetes' own scheduler and
// device plugin to have really bound the GPU resource its spec already
// carries. There is nothing left for GNWES to "attach": binding happened at
// pod admission, which is why this records the real node/resource rather
// than performing any action of its own.
func (r *GPULeaseReconciler) bindShared(ctx context.Context, lease *gnwesv1alpha1.GPULease, targetPod *corev1.Pod, requestedAt time.Time, logger logr.Logger) (ctrl.Result, error) {
	leaseKey := client.ObjectKeyFromObject(lease).String()
	poolKey := lease.Namespace + "/" + lease.Spec.PoolRef

	p := r.Manager.GetPool(poolKey)
	if p == nil {
		lease.Status.Phase = gnwesv1alpha1.LeasePhasePending
		lease.Status.Message = fmt.Sprintf("waiting for GPUWorkerPool %q to exist", lease.Spec.PoolRef)
		_ = r.Status().Update(ctx, lease)
		return ctrl.Result{RequeueAfter: 250 * time.Millisecond}, nil
	}

	// AdmissionTicket, once recorded, makes acquiring idempotent across the
	// many reconciles it can take for targetPod to actually reach Running —
	// without it, a later reconcile would try to Acquire a SECOND ticket
	// (the pod's annoReservedSlot annotation isn't cleared until bind
	// completes, but by then it no longer matches its original owner key,
	// so a naive re-Transfer/re-Acquire here would either double-book or,
	// in a size-1 pool, deadlock against the ticket this same lease already
	// holds).
	ticketID := lease.Status.AdmissionTicket
	if ticketID == "" {
		var ok bool
		if reserved := targetPod.Annotations[annoReservedSlot]; reserved != "" {
			reservationKey := "podgate/" + lease.Namespace + "/" + lease.Spec.TargetPod
			if p.Transfer(reserved, reservationKey, leaseKey) {
				ticketID, ok = reserved, true
			}
		}
		if !ok {
			ticketID, ok = p.Acquire(leaseKey)
		}
		if !ok {
			lease.Status.Phase = gnwesv1alpha1.LeasePhasePending
			lease.Status.Message = "no admission budget available yet; waiting for one to free up"
			_ = r.Status().Update(ctx, lease)
			return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
		}
		lease.Status.AdmissionTicket = ticketID
		lease.Status.Phase = gnwesv1alpha1.LeasePhasePending
		lease.Status.Message = fmt.Sprintf("admitted (ticket %s); waiting for the scheduler to actually bind the pod's real GPU resource", ticketID)
		if err := r.Status().Update(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
	}

	if targetPod.Status.Phase != corev1.PodRunning {
		return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
	}

	realResource := "unknown"
	for _, c := range targetPod.Spec.Containers {
		for name, qty := range c.Resources.Limits {
			if qty.Sign() > 0 && isGPUResource(string(name)) {
				realResource = string(name) + "=" + qty.String()
			}
		}
	}
	slotID := fmt.Sprintf("%s/%s", targetPod.Spec.NodeName, realResource)

	if err := r.annotatePod(ctx, targetPod, slotID); err != nil {
		p.Release(ticketID, leaseKey)
		lease.Status.Phase = gnwesv1alpha1.LeasePhaseFailed
		lease.Status.Message = fmt.Sprintf("failed to annotate pod %q: %v", lease.Spec.TargetPod, err)
		_ = r.Status().Update(ctx, lease)
		return ctrl.Result{}, err
	}

	now := metav1.Now()
	lease.Status.Phase = gnwesv1alpha1.LeasePhaseBound
	lease.Status.SlotID = slotID
	lease.Status.AttachedAt = &now
	lease.Status.AttachLatencyMillis = time.Since(requestedAt).Milliseconds()
	lease.Status.Message = "bound: real GPU resource confirmed on a Running pod"
	if err := r.Status().Update(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("lease bound (Shared)", "lease", leaseKey, "slot", slotID, "latencyMs", lease.Status.AttachLatencyMillis)
	if lease.Spec.TTLSeconds != nil {
		return ctrl.Result{RequeueAfter: time.Duration(*lease.Spec.TTLSeconds) * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// bindExclusive observes (never drives) the target pod's VMGPUAttachment.
// The real hot-attach/detach DECISION — when, based on the actor's actual
// resume/suspend state — belongs entirely to cmd/substrate-gpu-bridge
// (internal/substratebridge), already built and hardware-verified
// (2026-11-11): it polls the real actor's state and is the sole writer of
// Spec.DesiredState. If GPULeaseReconciler ALSO wrote DesiredState with its
// own policy (e.g. "always Attached once a lease exists"), the two would
// fight — the bridge would keep correctly detaching an idle pod while this
// controller kept forcing it back to Attached. So a GPULease over
// Backend=Exclusive only reports what cmd/substrate-gpu-bridge has already
// achieved; deploy one bridge instance per Exclusive-tier worker pod
// (naturally as a sidecar in that pod, watching its own current actor) for
// this to ever reach Bound.
func (r *GPULeaseReconciler) bindExclusive(ctx context.Context, lease *gnwesv1alpha1.GPULease, targetPod *corev1.Pod, requestedAt time.Time, logger logr.Logger) (ctrl.Result, error) {
	var attachment gnwesv1alpha1.VMGPUAttachment
	if err := r.Get(ctx, types.NamespacedName{Namespace: lease.Namespace, Name: lease.Spec.TargetPod}, &attachment); err != nil {
		lease.Status.Phase = gnwesv1alpha1.LeasePhasePending
		lease.Status.Message = fmt.Sprintf("waiting for VMGPUAttachment %q to exist", lease.Spec.TargetPod)
		_ = r.Status().Update(ctx, lease)
		return ctrl.Result{RequeueAfter: 250 * time.Millisecond}, nil
	}

	if attachment.Spec.DesiredState != gnwesv1alpha1.VMGPUDesiredAttached {
		lease.Status.Phase = gnwesv1alpha1.LeasePhasePending
		lease.Status.Message = "waiting for a real actor to actually resume onto this pod (desiredState is driven externally by cmd/substrate-gpu-bridge, not by this lease)"
		_ = r.Status().Update(ctx, lease)
		return ctrl.Result{RequeueAfter: 250 * time.Millisecond}, nil
	}

	switch attachment.Status.Phase {
	case gnwesv1alpha1.VMGPUPhaseAttached:
		now := metav1.Now()
		lease.Status.Phase = gnwesv1alpha1.LeasePhaseBound
		lease.Status.SlotID = attachment.Status.DeviceID
		lease.Status.AttachedAt = &now
		lease.Status.AttachLatencyMillis = attachment.Status.AttachLatencyMillis
		lease.Status.Message = fmt.Sprintf("bound: real VFIO device %s hot-attached (guest BDF %s)", attachment.Status.DeviceID, attachment.Status.GuestBDF)
		if err := r.Status().Update(ctx, lease); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("lease bound (Exclusive, real VFIO hot-attach)", "lease", client.ObjectKeyFromObject(lease).String(), "device", attachment.Status.DeviceID, "latencyMs", attachment.Status.AttachLatencyMillis)
		if lease.Spec.TTLSeconds != nil {
			return ctrl.Result{RequeueAfter: time.Duration(*lease.Spec.TTLSeconds) * time.Second}, nil
		}
		return ctrl.Result{}, nil
	case gnwesv1alpha1.VMGPUPhaseFailed:
		lease.Status.Phase = gnwesv1alpha1.LeasePhaseFailed
		lease.Status.Message = fmt.Sprintf("real VFIO hot-attach failed: %s", attachment.Status.Message)
		return ctrl.Result{}, r.Status().Update(ctx, lease)
	default:
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
	}
}

func (r *GPULeaseReconciler) reconcileTTL(ctx context.Context, lease *gnwesv1alpha1.GPULease, wp *gnwesv1alpha1.GPUWorkerPool, wpOK bool) (ctrl.Result, error) {
	if lease.Spec.TTLSeconds == nil || lease.Status.AttachedAt == nil {
		return ctrl.Result{}, nil
	}
	ttl := time.Duration(*lease.Spec.TTLSeconds) * time.Second
	elapsed := time.Since(lease.Status.AttachedAt.Time)
	if elapsed < ttl {
		return ctrl.Result{RequeueAfter: ttl - elapsed}, nil
	}
	if err := r.release(ctx, lease, wp, wpOK); err != nil {
		return ctrl.Result{}, err
	}
	lease.Status.Phase = gnwesv1alpha1.LeasePhaseReleased
	lease.Status.ReleasedAt = ptrTime(metav1.Now())
	lease.Status.Message = "TTL expired; real GPU capacity released back to the pool"
	return ctrl.Result{}, r.Status().Update(ctx, lease)
}

func (r *GPULeaseReconciler) reconcileDelete(ctx context.Context, lease *gnwesv1alpha1.GPULease, wp *gnwesv1alpha1.GPUWorkerPool, wpOK bool) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(lease, leaseFinalizer) {
		return ctrl.Result{}, nil
	}
	if lease.Status.SlotID != "" || lease.Status.AdmissionTicket != "" {
		if err := r.release(ctx, lease, wp, wpOK); err != nil {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(lease, leaseFinalizer)
	if err := r.Update(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	r.forgetFirstSeen(lease.UID)
	return ctrl.Result{}, nil
}

// release returns real capacity to the pool: for Shared, the admission
// ticket. For Exclusive there is nothing for GPULease to actively do —
// cmd/substrate-gpu-bridge owns detaching the real device, driven by the
// real actor actually suspending, not by this lease going away (see
// bindExclusive's doc comment for why GPULease must never write
// VMGPUAttachment.Spec itself).
func (r *GPULeaseReconciler) release(ctx context.Context, lease *gnwesv1alpha1.GPULease, wp *gnwesv1alpha1.GPUWorkerPool, wpOK bool) error {
	if wpOK && wp.Spec.Backend == gnwesv1alpha1.BackendExclusive {
		return nil
	}

	leaseKey := client.ObjectKeyFromObject(lease).String()
	poolKey := lease.Namespace + "/" + lease.Spec.PoolRef
	if p := r.Manager.GetPool(poolKey); p != nil {
		p.Release(lease.Status.AdmissionTicket, leaseKey)
	}
	r.unannotatePod(ctx, lease.Namespace, lease.Spec.TargetPod)
	return nil
}

// isGPUResource reports whether a resource name looks like a GPU extended
// resource (the NVIDIA device plugin's "nvidia.com/gpu", or a HAMi virtual
// slice key such as "nvidia.com/gpucores"/"nvidia.com/gpumem") rather than
// cpu/memory/ephemeral-storage or an unrelated extended resource.
func isGPUResource(name string) bool {
	return len(name) > 0 && (name == "nvidia.com/gpu" ||
		(len(name) > len("nvidia.com/") && name[:len("nvidia.com/")] == "nvidia.com/"))
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
