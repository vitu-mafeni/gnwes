package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
	"gnwes/internal/chclient"
	"gnwes/internal/vfiopool"
)

const vmgpuAttachmentFinalizer = "gnwes.io/vmgpu-attachment-cleanup"

// CHDevicePlugger is the subset of chclient.Client this controller needs,
// narrowed to an interface so tests can inject a fake instead of dialing a
// real cloud-hypervisor unix socket.
type CHDevicePlugger interface {
	AddDevice(ctx context.Context, cfg chclient.DeviceConfig) (chclient.PciDeviceInfo, error)
	RemoveDevice(ctx context.Context, id string) error
}

// VMGPUAttachmentReconciler implements the REAL hot-attach/detach flow: it
// acquires a physical VFIO device from the referenced VFIOGPUPool and
// hot-plugs/unplugs it into/out of a running cloud-hypervisor guest via its
// REST API, driven by Spec.DesiredState rather than object create/delete --
// so the same object can track one device across many attach/detach cycles
// as Substrate reuses a worker across many actors. This is GPULease's real
// counterpart: GPULease simulates the attach step (sleep + annotation) and
// never recreates a pod either; this reconciler does the real thing against
// real cloud-hypervisor hardware, and was added because vanilla Kubernetes
// DRA cannot attach a device to a pod after it's already running (see
// config/dra/README.md and research/live-attach-poc/README.md).
type VMGPUAttachmentReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	VFIOManager *vfiopool.Manager

	// NewCHClient builds the client this reconciler drives for a given
	// api-socket path. Defaults to a real chclient.Client in
	// SetupWithManager; tests inject a fake here directly.
	NewCHClient func(apiSocket string) CHDevicePlugger

	mu                sync.Mutex
	transitionStarted map[string]time.Time // uid+"/"+desiredState -> when this transition was first observed
}

// +kubebuilder:rbac:groups=gnwes.io,resources=vmgpuattachments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gnwes.io,resources=vmgpuattachments/status,verbs=get;update;patch

func (r *VMGPUAttachmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var att gnwesv1alpha1.VMGPUAttachment
	if err := r.Get(ctx, req.NamespacedName, &att); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	attachKey := req.NamespacedName.String()
	poolKey := req.Namespace + "/" + att.Spec.PoolRef
	ch := r.chClient(att.Spec.APISocket)

	// Deletion: detach first (best effort) if currently attached, release
	// the device, then drop the finalizer. This never leaves a device
	// marked Attached in the pool with nothing left to release it.
	if !att.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&att, vmgpuAttachmentFinalizer) {
			if att.Status.Phase == gnwesv1alpha1.VMGPUPhaseAttached && att.Status.DeviceID != "" {
				if err := ch.RemoveDevice(ctx, att.Status.DeviceID); err != nil {
					logger.Error(err, "best-effort detach on delete failed; releasing pool bookkeeping anyway", "attachment", attachKey)
				}
				if p := r.VFIOManager.GetPool(poolKey); p != nil {
					p.Release(att.Status.DeviceID, attachKey)
				}
			}
			controllerutil.RemoveFinalizer(&att, vmgpuAttachmentFinalizer)
			if err := r.Update(ctx, &att); err != nil {
				return ctrl.Result{}, err
			}
		}
		r.clearTransitions(att.UID)
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&att, vmgpuAttachmentFinalizer) {
		controllerutil.AddFinalizer(&att, vmgpuAttachmentFinalizer)
		if err := r.Update(ctx, &att); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	switch att.Spec.DesiredState {
	case gnwesv1alpha1.VMGPUDesiredAttached:
		return r.reconcileAttach(ctx, &att, poolKey, attachKey, ch)
	case gnwesv1alpha1.VMGPUDesiredDetached:
		return r.reconcileDetach(ctx, &att, poolKey, attachKey, ch)
	default:
		att.Status.Phase = gnwesv1alpha1.VMGPUPhaseFailed
		att.Status.Message = fmt.Sprintf("unknown desiredState %q", att.Spec.DesiredState)
		return ctrl.Result{}, r.Status().Update(ctx, &att)
	}
}

func (r *VMGPUAttachmentReconciler) reconcileAttach(ctx context.Context, att *gnwesv1alpha1.VMGPUAttachment, poolKey, attachKey string, ch CHDevicePlugger) (ctrl.Result, error) {
	if att.Status.Phase == gnwesv1alpha1.VMGPUPhaseAttached && att.Status.ObservedDesiredState == gnwesv1alpha1.VMGPUDesiredAttached {
		return ctrl.Result{}, nil // already attached for this desired state; nothing to do
	}

	requestedAt := r.markTransitionStart(att.UID, gnwesv1alpha1.VMGPUDesiredAttached)

	p := r.VFIOManager.GetPool(poolKey)
	if p == nil {
		att.Status.Phase = gnwesv1alpha1.VMGPUPhasePending
		att.Status.Message = fmt.Sprintf("waiting for VFIOGPUPool %q to exist", att.Spec.PoolRef)
		return ctrl.Result{RequeueAfter: 250 * time.Millisecond}, r.Status().Update(ctx, att)
	}

	var device vfiopool.Device
	var ok bool
	if att.Spec.DeviceRef != "" {
		device, ok = p.AcquireSpecific(att.Spec.DeviceRef, attachKey)
	} else {
		device, ok = p.Acquire(attachKey)
	}
	if !ok {
		att.Status.Phase = gnwesv1alpha1.VMGPUPhasePending
		att.Status.Message = "no VFIO device available yet; waiting for one to free up"
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, r.Status().Update(ctx, att)
	}

	info, err := ch.AddDevice(ctx, chclient.DeviceConfig{
		Path: device.PCIPath,
		ID:   device.ID,
	})
	if err != nil {
		// Deliberately 1 retry/sec, not tighter: confirmed live (2026-11-13)
		// that hammering a real cloud-hypervisor VMM with a failing
		// vm.add-device call for hours (one actor's VM took thousands of
		// attempts across this investigation, both before and after this
		// specific device-config bug was fixed) can exhaust something in
		// its device-manager's own PCI slot/ID bookkeeping -- a *later*,
		// otherwise-correct add-device call on the very same VM started
		// failing with a genuine cloud-hypervisor error ("Could not find an
		// available device slot on the PCI bus") despite that VM's PCI bus
		// having only ~6 of its ~32 slots in use (confirmed via vm.info).
		// If this attachment ends up in that state, the only known fix is
		// to resume the actor fresh (a new VM, a clean PCI bus) rather than
		// keep retrying against the wedged one.
		p.Release(device.ID, attachKey)
		att.Status.Phase = gnwesv1alpha1.VMGPUPhaseFailed
		att.Status.Message = fmt.Sprintf("vm.add-device failed for device %q: %v", device.ID, err)
		return ctrl.Result{RequeueAfter: time.Second}, r.Status().Update(ctx, att)
	}
	if info.BDF != "" {
		p.SetGuestBDF(device.ID, attachKey, info.BDF)
	}

	now := metav1.Now()
	att.Status.Phase = gnwesv1alpha1.VMGPUPhaseAttached
	att.Status.DeviceID = device.ID
	att.Status.GuestBDF = info.BDF
	att.Status.ObservedDesiredState = gnwesv1alpha1.VMGPUDesiredAttached
	att.Status.LastTransitionAt = &now
	att.Status.AttachLatencyMillis = time.Since(requestedAt).Milliseconds()
	att.Status.Message = "attached"
	r.clearTransition(att.UID, gnwesv1alpha1.VMGPUDesiredAttached)

	log.FromContext(ctx).Info("vmgpuattachment attached", "attachment", attachKey, "device", device.ID, "guestBDF", info.BDF, "attachMs", att.Status.AttachLatencyMillis)
	return ctrl.Result{}, r.Status().Update(ctx, att)
}

func (r *VMGPUAttachmentReconciler) reconcileDetach(ctx context.Context, att *gnwesv1alpha1.VMGPUAttachment, poolKey, attachKey string, ch CHDevicePlugger) (ctrl.Result, error) {
	if att.Status.Phase != gnwesv1alpha1.VMGPUPhaseAttached {
		// Nothing attached (fresh object, or already detached): just record
		// the desired state as observed.
		if att.Status.Phase == "" {
			att.Status.Phase = gnwesv1alpha1.VMGPUPhaseDetached
		}
		att.Status.ObservedDesiredState = gnwesv1alpha1.VMGPUDesiredDetached
		return ctrl.Result{}, r.Status().Update(ctx, att)
	}

	requestedAt := r.markTransitionStart(att.UID, gnwesv1alpha1.VMGPUDesiredDetached)

	deviceID := att.Status.DeviceID
	if err := ch.RemoveDevice(ctx, deviceID); err != nil {
		att.Status.Phase = gnwesv1alpha1.VMGPUPhaseFailed
		att.Status.Message = fmt.Sprintf("vm.remove-device failed for device %q: %v", deviceID, err)
		return ctrl.Result{RequeueAfter: time.Second}, r.Status().Update(ctx, att)
	}

	if p := r.VFIOManager.GetPool(poolKey); p != nil {
		p.Release(deviceID, attachKey)
	}

	now := metav1.Now()
	att.Status.Phase = gnwesv1alpha1.VMGPUPhaseDetached
	att.Status.DeviceID = ""
	att.Status.GuestBDF = ""
	att.Status.ObservedDesiredState = gnwesv1alpha1.VMGPUDesiredDetached
	att.Status.LastTransitionAt = &now
	att.Status.AttachLatencyMillis = time.Since(requestedAt).Milliseconds()
	att.Status.Message = "detached"
	r.clearTransition(att.UID, gnwesv1alpha1.VMGPUDesiredDetached)

	log.FromContext(ctx).Info("vmgpuattachment detached", "attachment", attachKey, "device", deviceID, "detachMs", att.Status.AttachLatencyMillis)
	return ctrl.Result{}, r.Status().Update(ctx, att)
}

func (r *VMGPUAttachmentReconciler) chClient(apiSocket string) CHDevicePlugger {
	if r.NewCHClient != nil {
		return r.NewCHClient(apiSocket)
	}
	return chclient.NewClient(apiSocket)
}

func (r *VMGPUAttachmentReconciler) markTransitionStart(uid types.UID, desired gnwesv1alpha1.VMGPUDesiredState) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.transitionStarted == nil {
		r.transitionStarted = map[string]time.Time{}
	}
	key := string(uid) + "/" + string(desired)
	if t, ok := r.transitionStarted[key]; ok {
		return t
	}
	now := time.Now()
	r.transitionStarted[key] = now
	return now
}

func (r *VMGPUAttachmentReconciler) clearTransition(uid types.UID, desired gnwesv1alpha1.VMGPUDesiredState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.transitionStarted, string(uid)+"/"+string(desired))
}

func (r *VMGPUAttachmentReconciler) clearTransitions(uid types.UID) {
	r.clearTransition(uid, gnwesv1alpha1.VMGPUDesiredAttached)
	r.clearTransition(uid, gnwesv1alpha1.VMGPUDesiredDetached)
}

// ignoreStatusOnlyUpdates skips watch-triggered reconciles for an update
// that changed only Status -- generation (bumped only by a real Spec
// change, since this CRD has +kubebuilder:subresource:status) is unchanged
// and the object isn't newly entering/leaving deletion. Without this,
// *every* Status().Update() this reconciler makes (including the Failed
// phase/message it writes on every failed retry -- see reconcileAttach)
// fires its own watch event and gets added back to the queue immediately
// via the controller's plain (non-rate-limited) watch-event path, racing
// ahead of and effectively erasing the RequeueAfter/backoff those retry
// paths asked for. Confirmed live (2026-11-13): with no predicate, this
// object's own resourceVersion was climbing ~20-30/sec -- a tight
// self-sustaining loop around one Reconcile call, not the real retry
// cadence (RequeueAfter: time.Second on a failed vm.add-device) the code
// actually intends, and frequent enough to produce concurrent Get+Update
// pairs from overlapping reconciles, which is what the recurring "the
// object has been modified" conflict errors in cmd/manager's log turned
// out to be. DeletionTimestamp is checked separately because setting it
// (a finalized object's `kubectl delete`) is a metadata-only change that,
// like a status update, never bumps generation either -- a plain
// GenerationChangedPredicate would have silently broken the
// deletion/finalizer-cleanup path in Reconcile above.
func ignoreStatusOnlyUpdates() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
				return true
			}
			return e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero()
		},
	}
}

func (r *VMGPUAttachmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gnwesv1alpha1.VMGPUAttachment{}, builder.WithPredicates(ignoreStatusOnlyUpdates())).
		Named("vmgpuattachment").
		Complete(r)
}
