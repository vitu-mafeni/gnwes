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
// interface -- in production, cmd/substrate-gpu-bridge's kubectlAteFetcher,
// which shells out to `kubectl-ate get actor -o json` (the same tool and
// invocation pattern config/substrate-integration/README.md already
// established for this repo, rather than importing agent-substrate's
// internal Go packages across module boundaries).
package substratebridge

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
)

// ActorStateFetcher returns the raw ActorState enum name (e.g.
// "ACTOR_STATE_RUNNING") Agent Substrate currently reports for one actor.
type ActorStateFetcher interface {
	FetchActorState(ctx context.Context) (string, error)
}

// AttachmentDriver reads and writes the VMGPUAttachment this bridge drives.
type AttachmentDriver interface {
	GetDesiredState(ctx context.Context) (gnwesv1alpha1.VMGPUDesiredState, error)
	SetDesiredState(ctx context.Context, state gnwesv1alpha1.VMGPUDesiredState) error
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

// actorStatusJSON is the minimal shape this package needs from
// `kubectl-ate get actor -o json`'s protojson output (ateapi.v1.Actor):
// just status.state. Protojson serializes proto field names to
// lowerCamelCase and enum values to their full name string, confirmed
// against api/ateapi/v1/ateapi.proto's ActorStatus/ActorState definitions
// in the real agent-substrate/substrate repo.
type actorStatusJSON struct {
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

// ParseActorState extracts status.state from one actor's JSON
// representation, as `kubectl-ate get actor <name> -o json` prints it.
func ParseActorState(data []byte) (string, error) {
	var a actorStatusJSON
	if err := json.Unmarshal(data, &a); err != nil {
		return "", fmt.Errorf("parsing actor JSON: %w", err)
	}
	if a.Status.State == "" {
		return "", fmt.Errorf("actor JSON has no status.state field (got: %s)", string(data))
	}
	return a.Status.State, nil
}

// Transition is one applied state change, for logging/observability.
type Transition struct {
	ActorState   string
	DesiredState gnwesv1alpha1.VMGPUDesiredState
}

// ReconcileOnce fetches the actor's current state, computes what
// VMGPUAttachment.spec.desiredState should be, and applies it only if that
// differs from the attachment's current desiredState -- idempotent, so
// polling at a fixed interval doesn't turn into a storm of needless API
// writes once the actor settles into a steady state. Returns the applied
// Transition and changed=true only when a write actually happened.
func ReconcileOnce(ctx context.Context, fetcher ActorStateFetcher, driver AttachmentDriver) (changed bool, t Transition, err error) {
	actorState, err := fetcher.FetchActorState(ctx)
	if err != nil {
		return false, Transition{}, fmt.Errorf("fetching actor state: %w", err)
	}

	desired := DesiredStateForActorState(actorState)
	t = Transition{ActorState: actorState, DesiredState: desired}

	current, err := driver.GetDesiredState(ctx)
	if err != nil {
		return false, Transition{}, fmt.Errorf("reading current desiredState: %w", err)
	}
	if current == desired {
		return false, t, nil
	}

	if err := driver.SetDesiredState(ctx, desired); err != nil {
		return false, Transition{}, fmt.Errorf("setting desiredState to %q: %w", desired, err)
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
	changed, t, err := ReconcileOnce(ctx, fetcher, driver)
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
