package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
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

// updateStatusWithRetry re-GETs att and reapplies mutate's Status changes on
// a write conflict, instead of returning "the object has been modified" up
// to controller-runtime. Confirmed live (2026-11-16): every reconcileAttach
// retry path pairs a non-nil error with an explicit RequeueAfter, and
// controller-runtime's own doc comment on Reconciler -- borne out by its
// "RequeueAfter and Requeue will always be ignored if the error is
// non-nil" warning, logged on every one of these conflicts -- discards that
// RequeueAfter whenever the error is non-nil, falling back to its default
// backoff instead. A real-hardware VMGPUAttachment is exactly the kind of
// object multiple reconciles (this controller's own explicit RequeueAfter
// retries, cmd/substrate-gpu-bridge's spec patches, informer-cache replay
// after a restart) touch around the same few seconds, so a plain
// Status().Update() here conflicted on essentially every attempt once a
// real actor's VM survived long enough to reach vm.add-device (the pmem
// bug fixed in agent-substrate made that unreachable before 2026-11-16) --
// att.Status never reached Attached, Failed, or any of this method's other
// terminal states at all; it just sat on whatever reconcileAttach/
// reconcileDetach's last clean write had been, forever. att itself is
// updated in place with whatever this wrote, so a caller's log line right
// after this call (e.g. reconcileAttach's own "vmgpuattachment attached")
// reports what's actually now in etcd.
func (r *VMGPUAttachmentReconciler) updateStatusWithRetry(ctx context.Context, att *gnwesv1alpha1.VMGPUAttachment, mutate func(*gnwesv1alpha1.VMGPUAttachment)) error {
	key := client.ObjectKeyFromObject(att)
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var latest gnwesv1alpha1.VMGPUAttachment
		if err := r.Get(ctx, key, &latest); err != nil {
			return err
		}
		mutate(&latest)
		if err := r.Status().Update(ctx, &latest); err != nil {
			return err
		}
		*att = latest
		return nil
	})
}

// updateWithRetry is updateStatusWithRetry's counterpart for a (rare, only
// at finalizer add/remove) non-status update: same conflict hazard, same
// re-GET-and-reapply fix. mutate's finalizer calls are idempotent
// (controllerutil.AddFinalizer/RemoveFinalizer are no-ops if already in the
// wanted state), so reapplying them against a freshly-read copy is safe.
func (r *VMGPUAttachmentReconciler) updateWithRetry(ctx context.Context, att *gnwesv1alpha1.VMGPUAttachment, mutate func(*gnwesv1alpha1.VMGPUAttachment)) error {
	key := client.ObjectKeyFromObject(att)
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var latest gnwesv1alpha1.VMGPUAttachment
		if err := r.Get(ctx, key, &latest); err != nil {
			return err
		}
		mutate(&latest)
		if err := r.Update(ctx, &latest); err != nil {
			return err
		}
		*att = latest
		return nil
	})
}

func (r *VMGPUAttachmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var att gnwesv1alpha1.VMGPUAttachment
	if err := r.Get(ctx, req.NamespacedName, &att); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	attachKey := req.NamespacedName.String()
	poolKey := req.Namespace + "/" + att.Spec.PoolRef
	// Resolved once, here, and reused below (never att.Spec.APISocket
	// directly) -- see resolveRealAPISocket's own doc comment for why
	// the bridge that wrote this field can't always get the filename
	// right itself.
	apiSocket := resolveRealAPISocket(att.Spec.APISocket)
	ch := r.chClient(apiSocket)

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
			if err := r.updateWithRetry(ctx, &att, func(a *gnwesv1alpha1.VMGPUAttachment) {
				controllerutil.RemoveFinalizer(a, vmgpuAttachmentFinalizer)
			}); err != nil {
				return ctrl.Result{}, err
			}
		}
		r.clearTransitions(att.UID)
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&att, vmgpuAttachmentFinalizer) {
		if err := r.updateWithRetry(ctx, &att, func(a *gnwesv1alpha1.VMGPUAttachment) {
			controllerutil.AddFinalizer(a, vmgpuAttachmentFinalizer)
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	switch att.Spec.DesiredState {
	case gnwesv1alpha1.VMGPUDesiredAttached:
		return r.reconcileAttach(ctx, &att, poolKey, attachKey, ch)
	case gnwesv1alpha1.VMGPUDesiredDetached:
		return r.reconcileDetach(ctx, &att, poolKey, attachKey, apiSocket, ch)
	default:
		desiredState := att.Spec.DesiredState
		err := r.updateStatusWithRetry(ctx, &att, func(a *gnwesv1alpha1.VMGPUAttachment) {
			a.Status.Phase = gnwesv1alpha1.VMGPUPhaseFailed
			a.Status.Message = fmt.Sprintf("unknown desiredState %q", desiredState)
		})
		return ctrl.Result{}, err
	}
}

func (r *VMGPUAttachmentReconciler) reconcileAttach(ctx context.Context, att *gnwesv1alpha1.VMGPUAttachment, poolKey, attachKey string, ch CHDevicePlugger) (ctrl.Result, error) {
	if att.Status.Phase == gnwesv1alpha1.VMGPUPhaseAttached && att.Status.ObservedDesiredState == gnwesv1alpha1.VMGPUDesiredAttached {
		return ctrl.Result{}, nil // already attached for this desired state; nothing to do
	}

	requestedAt := r.markTransitionStart(att.UID, gnwesv1alpha1.VMGPUDesiredAttached)

	p := r.VFIOManager.GetPool(poolKey)
	if p == nil {
		poolRef := att.Spec.PoolRef
		err := r.updateStatusWithRetry(ctx, att, func(a *gnwesv1alpha1.VMGPUAttachment) {
			a.Status.Phase = gnwesv1alpha1.VMGPUPhasePending
			a.Status.Message = fmt.Sprintf("waiting for VFIOGPUPool %q to exist", poolRef)
		})
		return ctrl.Result{RequeueAfter: 250 * time.Millisecond}, err
	}

	var device vfiopool.Device
	var ok bool
	if att.Spec.DeviceRef != "" {
		device, ok = p.AcquireSpecific(att.Spec.DeviceRef, attachKey)
	} else {
		device, ok = p.Acquire(attachKey)
	}
	if !ok {
		err := r.updateStatusWithRetry(ctx, att, func(a *gnwesv1alpha1.VMGPUAttachment) {
			a.Status.Phase = gnwesv1alpha1.VMGPUPhasePending
			a.Status.Message = "no VFIO device available yet; waiting for one to free up"
		})
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, err
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
		addErr := err
		err = r.updateStatusWithRetry(ctx, att, func(a *gnwesv1alpha1.VMGPUAttachment) {
			a.Status.Phase = gnwesv1alpha1.VMGPUPhaseFailed
			a.Status.Message = fmt.Sprintf("vm.add-device failed for device %q: %v", device.ID, addErr)
		})
		return ctrl.Result{RequeueAfter: time.Second}, err
	}
	if info.BDF != "" {
		p.SetGuestBDF(device.ID, attachKey, info.BDF)
	}

	now := metav1.Now()
	attachLatencyMillis := time.Since(requestedAt).Milliseconds()
	err = r.updateStatusWithRetry(ctx, att, func(a *gnwesv1alpha1.VMGPUAttachment) {
		a.Status.Phase = gnwesv1alpha1.VMGPUPhaseAttached
		a.Status.DeviceID = device.ID
		a.Status.GuestBDF = info.BDF
		a.Status.ObservedDesiredState = gnwesv1alpha1.VMGPUDesiredAttached
		a.Status.LastTransitionAt = &now
		a.Status.AttachLatencyMillis = attachLatencyMillis
		a.Status.Message = "attached"
	})
	r.clearTransition(att.UID, gnwesv1alpha1.VMGPUDesiredAttached)
	if err != nil {
		return ctrl.Result{}, err
	}

	log.FromContext(ctx).Info("vmgpuattachment attached", "attachment", attachKey, "device", device.ID, "guestBDF", info.BDF, "attachMs", attachLatencyMillis)
	return ctrl.Result{}, nil
}

func (r *VMGPUAttachmentReconciler) reconcileDetach(ctx context.Context, att *gnwesv1alpha1.VMGPUAttachment, poolKey, attachKey, apiSocket string, ch CHDevicePlugger) (ctrl.Result, error) {
	// Keyed on DeviceID, not Phase: Phase can be Failed while a device is
	// still genuinely held (a previous detach attempt's vm.remove-device
	// call failed for a real reason, see vmGone below) -- DeviceID is the
	// actual source of truth for "is a device currently recorded as held by
	// this attachment." Confirmed live (2026-11-14): checking Phase !=
	// Attached here meant an attachment already sitting in Phase=Failed
	// from an earlier failed detach attempt short-circuited straight to
	// "nothing to do" on every subsequent reconcile, forever, without ever
	// retrying RemoveDevice or reaching vmGone's recovery path below --
	// stuck permanently, even after the fix that added vmGone itself.
	if att.Status.DeviceID == "" {
		// Nothing attached (fresh object, or already detached): just record
		// the desired state as observed.
		err := r.updateStatusWithRetry(ctx, att, func(a *gnwesv1alpha1.VMGPUAttachment) {
			if a.Status.Phase == "" {
				a.Status.Phase = gnwesv1alpha1.VMGPUPhaseDetached
			}
			a.Status.ObservedDesiredState = gnwesv1alpha1.VMGPUDesiredDetached
		})
		return ctrl.Result{}, err
	}

	requestedAt := r.markTransitionStart(att.UID, gnwesv1alpha1.VMGPUDesiredDetached)

	deviceID := att.Status.DeviceID
	if removeErr := ch.RemoveDevice(ctx, deviceID); removeErr != nil && !r.vmGone(apiSocket) {
		err := r.updateStatusWithRetry(ctx, att, func(a *gnwesv1alpha1.VMGPUAttachment) {
			a.Status.Phase = gnwesv1alpha1.VMGPUPhaseFailed
			a.Status.Message = fmt.Sprintf("vm.remove-device failed for device %q: %v", deviceID, removeErr)
		})
		return ctrl.Result{RequeueAfter: time.Second}, err
	} else if removeErr != nil {
		log.FromContext(ctx).Info("vm.remove-device failed but the VM's own api-socket is gone; treating the device as already freed",
			"attachment", attachKey, "device", deviceID, "apiSocket", apiSocket, "err", removeErr)
	}

	if p := r.VFIOManager.GetPool(poolKey); p != nil {
		p.Release(deviceID, attachKey)
	}

	now := metav1.Now()
	detachLatencyMillis := time.Since(requestedAt).Milliseconds()
	err := r.updateStatusWithRetry(ctx, att, func(a *gnwesv1alpha1.VMGPUAttachment) {
		a.Status.Phase = gnwesv1alpha1.VMGPUPhaseDetached
		a.Status.DeviceID = ""
		a.Status.GuestBDF = ""
		a.Status.ObservedDesiredState = gnwesv1alpha1.VMGPUDesiredDetached
		a.Status.LastTransitionAt = &now
		a.Status.AttachLatencyMillis = detachLatencyMillis
		a.Status.Message = "detached"
	})
	r.clearTransition(att.UID, gnwesv1alpha1.VMGPUDesiredDetached)
	if err != nil {
		return ctrl.Result{}, err
	}

	log.FromContext(ctx).Info("vmgpuattachment detached", "attachment", attachKey, "device", deviceID, "detachMs", detachLatencyMillis)
	return ctrl.Result{}, nil
}

// vmGone reports whether apiSocket's path no longer exists on this node's
// filesystem -- a definitive signal, not a transient dial hiccup (chclient's
// own doWithDialRetry already absorbs those before RemoveDevice ever
// returns an error here), that the cloud-hypervisor VM which owned it is
// gone. Confirmed live (2026-11-14): Agent Substrate's actor suspend fully
// tears down the microVM's entire working directory (socket, console.log,
// everything), not just pauses the VM -- so a hot vm.remove-device call
// this reconciler's detach path assumed would still work can never
// succeed, and it was retrying a permanently-dead socket once a second
// forever, with the device stuck "held" in the pool (VFIOGPUPool's
// availableDevices never recovering) since Release() only ran on success.
// When the VM process itself is gone, whatever VFIO device it held is
// already back with the host kernel regardless (closing a process's VFIO
// container file descriptor -- which happens unconditionally on exit --
// is what actually releases a VFIO device, not any API call cloud-
// hypervisor exposes), so there is nothing left to hot-remove, and this
// lets reconcileDetach correctly recognize that instead of leaking the
// device out of the pool's own bookkeeping permanently.
func (r *VMGPUAttachmentReconciler) vmGone(apiSocket string) bool {
	_, err := os.Stat(apiSocket)
	return os.IsNotExist(err)
}

// resolveRealAPISocket returns the real, existing cloud-hypervisor
// api-socket path for apiSocket, swapping its filename for the sibling
// candidate if the one cmd/substrate-gpu-bridge wrote doesn't actually
// exist on this node. ateworker-microvm names the socket
// "clh-api-restore.sock" when it restores a VM from this worker pod's
// own local checkpoint, and "clh-api.sock" instead for a cold `run`
// (no local checkpoint for THIS pod) -- the bridge genuinely cannot
// tell which happened: it runs inside the worker pod's own sidecar
// container, which (deliberately, by design -- it only ever needs to
// write a path string, never open the socket itself) has no hostPath
// mount into GNWESHostVCVMDir at all, so it cannot os.Stat either
// candidate to check. This reconciler runs as cmd/manager, directly on
// the host, so it can. An earlier version of this fix tried the stat
// check inside the bridge instead and could never work for exactly
// that reason -- confirmed live (2026-11-15): the attachment stayed
// permanently Detached, since the bridge's check always saw ENOENT
// for a path it has no visibility into at all, regardless of what
// actually existed on the host.
//
// Returns apiSocket unchanged if neither candidate exists (lets the
// subsequent dial fail with its own clear error instead of guessing).
func resolveRealAPISocket(apiSocket string) string {
	if _, err := os.Stat(apiSocket); err == nil {
		return apiSocket
	}
	dir := filepath.Dir(apiSocket)
	for _, name := range []string{"clh-api-restore.sock", "clh-api.sock"} {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return apiSocket
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
