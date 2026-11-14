// Package substratebridge connects Agent Substrate's actor lifecycle to
// VMGPUAttachment's spec.desiredState: when an actor resumes, the device it
// needs should be attached; when it suspends (or crashes, or is deleted),
// the device should come back off. See config/vmgpu/README.md's "What's NOT
// verified" section -- this is that bridge.
//
// Substrate's Actor/WorkerAssignment are not Kubernetes CRDs (only
// WorkerPool is -- confirmed by grepping agent-substrate/substrate for
// kubebuilder:object:root, which only WorkerPool's type carries), so there
// is nothing to `kubectl get --watch`. This package polls instead, driving
// whatever actually fetches actor state through the ActorStateFetcher
// interface -- in production, KubectlAteFetcher (kubectlate.go), which
// shells out to `kubectl-ate get actor -a <atespace> -o json` and finds
// whichever actor (if any) is currently assigned to this bridge's worker
// pod (the same tool and invocation pattern config/substrate-integration/README.md
// already established for this repo, rather than importing agent-substrate's
// internal Go packages across module boundaries).
package substratebridge

import (
	"context"
	"fmt"
	"time"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
)

// ActorObservation is what this bridge needs to know about whichever actor
// (if any) currently occupies the one worker pod it watches. A worker pod
// hosts actors sequentially, not just one for its whole lifetime (suspend
// one, resume another), so both fields describe "right now", not "the actor
// this bridge was deployed for" -- there is no such fixed actor.
type ActorObservation struct {
	// State is the raw ActorState enum name (e.g. "ACTOR_STATE_RUNNING"), or
	// "" if no actor is currently assigned to this worker pod.
	State string
	// UID is the current actor's ResourceMetadata.uid, or "" if no actor is
	// currently assigned.
	UID string
	// APISocket is the HOST-filesystem path to this actor's cloud-hypervisor
	// api-socket (see HostAPISocketPath), or "" if no actor is currently
	// assigned. Precomputed by the fetcher, which is the only thing that
	// knows its own worker pod's name (needed to build the path) -- kept
	// off ReconcileOnce so it stays a pure function of whatever the fetcher
	// and driver report, testable with fakes that just return a string.
	APISocket string
}

// ActorStateFetcher returns what Agent Substrate currently reports for
// whichever actor (if any) is assigned to the one worker pod this bridge
// watches.
type ActorStateFetcher interface {
	FetchActorState(ctx context.Context) (ActorObservation, error)
}

// AttachmentDriver reads and writes the VMGPUAttachment this bridge drives.
// Both spec fields are read and written together: cloud-hypervisor's
// api-socket path and the desired attach state must land on the object in
// the same update, not two separate ones, so VMGPUAttachmentReconciler never
// observes DesiredState=Attached paired with a socket path from a different
// (possibly already-torn-down) actor's VM.
type AttachmentDriver interface {
	GetSpec(ctx context.Context) (state gnwesv1alpha1.VMGPUDesiredState, apiSocket string, err error)
	SetSpec(ctx context.Context, state gnwesv1alpha1.VMGPUDesiredState, apiSocket string) error
}

// GNWESHostVCVMDir is the fixed host-filesystem base directory
// podgate-webhook hostPath-mounts into every Backend=Exclusive worker pod's
// /run/vc/vm (one subdirectory per pod, via a volumeMount with a literal
// SubPath -- see vcVMID below) -- not an emptyDir, deliberately:
// cmd/manager's VMGPUAttachmentReconciler runs as its own process, outside
// every worker pod, and can only dial a unix socket it can see on its own
// host filesystem. cmd/manager must therefore run on the same node as the
// worker pods it serves (same constraint config/substrate-integration's
// TESTING-WITH-REAL-AGENTS.md already called out for the earlier,
// VM-on-bare-host version of this test).
const GNWESHostVCVMDir = "/var/lib/gnwes/vcvm"

// clhAPISocketNameRestore/clhAPISocketNameFresh are the two possible
// cloud-hypervisor api-socket filenames within one actor's kata.VMDir,
// which one actually applies depending on how ateworker-microvm launched
// THIS SPECIFIC VM -- not a universal constant, corrected 2026-11-15.
//
// The original version of this comment (2026-11-13) concluded Substrate
// "always restores from a baseline/golden snapshot" and that
// "clh-api.sock" never applies in practice, because every resume tested
// until then reused a worker pod that already held its own prior local
// checkpoint -- restore.go's restoreWorkload ran every time, naming the
// socket "clh-api-restore.sock" permanently (restore.go:393/527).
//
// That "always" was disproven live (2026-11-15): deleting a worker pod
// (forcing Substrate to recreate it from scratch -- e.g. to pick up a
// new SandboxConfig) and then resuming an actor onto the fresh pod made
// ateworker-microvm cold-`run` the VM instead (run.go:514,
// "clh-api.sock") -- even though that SAME actor had prior checkpoint
// history from its life on the old pod. The deciding factor is whether
// THIS WORKER POD has its own local checkpoint to restore from, not
// whether the actor has ever run before.
const (
	clhAPISocketNameRestore = "clh-api-restore.sock"
	clhAPISocketNameFresh   = "clh-api.sock"
)

// HostAPISocketPath returns the HOST-filesystem path to one actor's real
// cloud-hypervisor api-socket -- NOT the in-pod path ateworker-microvm
// itself writes to (/run/vc/vm/<actor-uid>/<clh-api(-restore)?.sock>, see
// the two names above). Because podgate-webhook hostPath-mounts
// GNWESHostVCVMDir/<vcVMID> onto that exact container path, the same file
// is reachable from the host (and hence by cmd/manager) at this path
// instead.
//
// Deliberately a PURE function again -- an earlier version of this fix
// (2026-11-15) tried to check which of the two names actually exists
// right here, via os.Stat. That is architecturally impossible from
// where this runs: KubectlAteFetcher.FetchActorState, inside
// cmd/substrate-gpu-bridge, the worker pod's own sidecar container --
// which was deliberately given NO hostPath mount into GNWESHostVCVMDir
// at all (this function only ever needs to produce a path STRING for
// cmd/manager, running separately on the host, to dial; the sidecar
// itself never opens the socket). Confirmed live: that version always
// saw ENOENT for both candidates regardless of what actually existed on
// the host, so the attachment stayed permanently Detached. The real
// fix, which actually has filesystem access to check,
// is internal/controller/vmgpuattachment_controller.go's
// resolveRealAPISocket -- this function goes back to always returning
// one guess (clhAPISocketNameRestore); cmd/manager resolves which name
// is actually real right before dialing, same place vmGone's own
// os.Stat check already lives.
//
// vcVMID is a short random token podgate-webhook generates once per pod at
// admission time (cmd/podgate-webhook's exclusiveSidecarPatches) -- NOT the
// pod's own name. It must be short: this path is dialed as a real AF_UNIX
// socket address, whose sun_path buffer Linux caps at 108 bytes total, and
// a worker pod's real Kubernetes name (GenerateName plus the apiserver's
// random suffix, e.g. "gnwes-counter-exclusive-64cfccc6b6-6j4mb") routinely
// burns through most of that budget on its own once combined with
// GNWESHostVCVMDir and the actor UID below -- confirmed the hard way
// (2026-11-13): a 110-byte path using the real pod name failed
// cloud-hypervisor's dial with "connect: invalid argument" (Go's net
// package rejecting an over-length UnixAddr), even though every RBAC and
// control-loop piece upstream of it was by then already working. Using the
// pod's own name here was never required anyway -- all this ID has to do
// is keep one worker pod's subdirectory from colliding with another's, the
// same job a short random token does just as well in far fewer bytes.
// Keyed by both vcVMID and actorUID: by vcVMID because GNWESHostVCVMDir
// holds one subdirectory per pod, and by actorUID because a reused worker
// pod's VM is torn down and rebuilt fresh for each actor that resumes onto
// it.
func HostAPISocketPath(vcVMID, actorUID string) string {
	return fmt.Sprintf("%s/%s/%s/%s", GNWESHostVCVMDir, vcVMID, actorUID, clhAPISocketNameRestore)
}

// DesiredStateForActorState maps a Substrate ActorState onto what
// VMGPUAttachment.spec.desiredState should be. Deliberately conservative:
// only the fully-RUNNING state attaches a device; every other state
// (including the transitional RESUMING/SUSPENDING, and anything this
// bridge doesn't recognize) detaches or stays detached. Attaching earlier,
// during RESUMING, could shave more latency off a real resume -- but
// whether cloud-hypervisor's vm.add-device behaves the same against a
// guest that's still mid-restore (as opposed to fully Running) is not
// something this repo has verified against real hardware, so it isn't
// assumed here. See config/vmgpu/README.md.
func DesiredStateForActorState(actorState string) gnwesv1alpha1.VMGPUDesiredState {
	if actorState == "ACTOR_STATE_RUNNING" {
		return gnwesv1alpha1.VMGPUDesiredAttached
	}
	return gnwesv1alpha1.VMGPUDesiredDetached
}

// Transition is one applied state change, for logging/observability.
type Transition struct {
	ActorState   string
	DesiredState gnwesv1alpha1.VMGPUDesiredState
	APISocket    string
}

// ReconcileOnce fetches whichever actor currently occupies the worker pod,
// computes what VMGPUAttachment.spec.desiredState and spec.apiSocket should
// be, and applies both together only if either differs from the
// attachment's current spec -- idempotent, so polling at a fixed interval
// doesn't turn into a storm of needless API writes once the actor settles
// into a steady state. Returns the applied Transition and changed=true only
// when a write actually happened.
//
// When no actor is currently assigned (ActorObservation.UID == ""), the
// attachment's last-known apiSocket is left as-is rather than cleared --
// harmless, since DesiredState already falls back to Detached in that case
// regardless of what apiSocket says.
func ReconcileOnce(ctx context.Context, fetcher ActorStateFetcher, driver AttachmentDriver) (changed bool, t Transition, err error) {
	obs, err := fetcher.FetchActorState(ctx)
	if err != nil {
		return false, Transition{}, fmt.Errorf("fetching actor state: %w", err)
	}

	desiredState := DesiredStateForActorState(obs.State)
	desiredSocket := obs.APISocket
	t = Transition{ActorState: obs.State, DesiredState: desiredState, APISocket: desiredSocket}

	currentState, currentSocket, err := driver.GetSpec(ctx)
	if err != nil {
		return false, Transition{}, fmt.Errorf("reading current spec: %w", err)
	}

	nextSocket := currentSocket
	if desiredSocket != "" {
		nextSocket = desiredSocket
	}

	// An actor can report RUNNING before this bridge has learned the
	// VM's real api-socket path -- confirmed live (2026-11-15): a
	// freshly-created (non-restore) worker pod's VM can take a few
	// seconds after resume before cloud-hypervisor creates either
	// possible socket file at all (see HostAPISocketPath's own doc
	// comment on why it can legitimately return ""). Attaching with
	// apiSocket="" would write a spec cmd/manager can only fail to dial
	// ("dial unix: missing address") -- stay Detached until a later
	// poll tick finds the real socket, rather than flipping to Attached
	// on actor state alone.
	if desiredState == gnwesv1alpha1.VMGPUDesiredAttached && nextSocket == "" {
		desiredState = gnwesv1alpha1.VMGPUDesiredDetached
		t.DesiredState = desiredState
	}

	if currentState == desiredState && nextSocket == currentSocket {
		return false, t, nil
	}

	if err := driver.SetSpec(ctx, desiredState, nextSocket); err != nil {
		return false, Transition{}, fmt.Errorf("setting desiredState=%q apiSocket=%q: %w", desiredState, nextSocket, err)
	}
	return true, t, nil
}

// OnTransition is called after every ReconcileOnce call that actually
// changed desiredState (never for a no-op poll) -- the caller's hook for
// logging.
type OnTransition func(Transition)

// OnError is called whenever one ReconcileOnce call fails -- the loop
// itself never stops on an error (a transient kubectl-ate or API-server
// hiccup shouldn't kill the bridge), it just logs and retries next tick.
type OnError func(error)

// tickTimeout bounds a single ReconcileOnce call -- in production that
// means one `kubectl-ate get actor` invocation, which itself auto-port-
// forwards to ate-api-server (dialing a websocket with no deadline of its
// own, see agent-substrate's internal/portforward package). Run's loop is a
// single goroutine processing one tick at a time with no per-call
// deadline; without this, one hung dial (a stalled handshake, a backend
// pod that stopped responding mid-connection) blocks that tick's
// exec.CommandContext forever and silently wedges the entire reconcile
// loop -- no crash, no restart, just a bridge that stops polling and
// never logs again. Confirmed live on 2026-11-13: the sidecar's log went
// completely silent for 3+ minutes (should log every ~1s) with restartCount
// still 0, after a kubectl-ate invocation apparently hung mid-dial.
const tickTimeout = 15 * time.Second

// Run polls ReconcileOnce every interval until ctx is cancelled.
func Run(ctx context.Context, fetcher ActorStateFetcher, driver AttachmentDriver, interval time.Duration, onTransition OnTransition, onError OnError) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Reconcile once immediately rather than waiting a full interval for
	// the first observation.
	tick(ctx, fetcher, driver, onTransition, onError)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick(ctx, fetcher, driver, onTransition, onError)
		}
	}
}

func tick(ctx context.Context, fetcher ActorStateFetcher, driver AttachmentDriver, onTransition OnTransition, onError OnError) {
	tickCtx, cancel := context.WithTimeout(ctx, tickTimeout)
	defer cancel()

	changed, t, err := ReconcileOnce(tickCtx, fetcher, driver)
	if err != nil {
		if onError != nil {
			onError(err)
		}
		return
	}
	if changed && onTransition != nil {
		onTransition(t)
	}
}
