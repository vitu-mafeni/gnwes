// Package hamilifecycle drives the Shared (HAMi) tier's actor-triggered
// checkpoint/restore cycle: when a real Agent Substrate actor stops running,
// its HAMi-shared GPU pod is checkpointed (via FluidCR, already deployed and
// proven on this project's real cluster) and deleted, freeing the GPU slice
// for other agents; when the actor runs again, the pod is restored from that
// checkpoint, continuing its GPU work from where it was saved -- or created
// fresh, the first time there is nothing to restore yet.
//
// This reuses internal/substratebridge's own ActorStateFetcher interface and
// DesiredStateForActorState mapping (same actor-state semantics as the
// Exclusive/VFIO tier's bridge: Attached means "should be running", Detached
// means "should be freed") rather than inventing a parallel one -- the two
// tiers answer the same question ("is this actor supposed to be using its
// GPU right now?") with different mechanisms underneath.
//
// Checkpoint/restore is a multi-second, multi-phase operation (confirmed:
// every real tool surveyed for this project -- FluidCR itself, NVIDIA
// Snapshot, FAST'26's GCR -- takes seconds, bounded by how much GPU memory
// has to move, never milliseconds), unlike the Exclusive tier's near-instant
// device hotplug. This package's Run loop accounts for that: it polls actor
// state on the same cadence as the VFIO bridge, but a single
// checkpoint/restore cycle runs to completion (or failure) before the next
// state is considered.
package hamilifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
	"gnwes/internal/fluidcrclient"
	"gnwes/internal/substratebridge"
)

// Driver owns one HAMi-shared pod's full lifecycle. PodSpec is used both to
// create the pod fresh (the first run, before any checkpoint exists) and as
// the restore Template's spec (a CRIU restore rebuilds the process from the
// archive, so PodSpec's command/args/env are never actually executed on
// restore -- only the sandbox shape, e.g. resourceClaims/runtimeClassName/
// volumes, matters then).
type Driver struct {
	Client        client.Client
	Namespace     string
	PodName       string
	ContainerName string
	PodSpec       corev1.PodSpec

	// PollInterval governs how often EnsureRunning/EnsureSuspended re-check
	// a FluidCRCheckpoint/FluidCRRestore's phase while waiting for it to
	// finish. Defaults to 2s if zero.
	PollInterval time.Duration

	mu         sync.Mutex
	checkpoint *savedCheckpoint // nil until the first checkpoint completes
}

type savedCheckpoint struct {
	path string
}

func (d *Driver) pollInterval() time.Duration {
	if d.PollInterval <= 0 {
		return 2 * time.Second
	}
	return d.PollInterval
}

// EnsureRunning makes the actor's HAMi pod exist and be Running: a no-op if
// it already is, a FluidCRRestore if a prior checkpoint exists, or a fresh
// pod create if this is the first run.
func (d *Driver) EnsureRunning(ctx context.Context) error {
	var pod corev1.Pod
	err := d.Client.Get(ctx, types.NamespacedName{Namespace: d.Namespace, Name: d.PodName}, &pod)
	if err == nil {
		return nil // already exists; nothing to do
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("checking for existing pod %s/%s: %w", d.Namespace, d.PodName, err)
	}

	d.mu.Lock()
	cp := d.checkpoint
	d.mu.Unlock()

	if cp == nil {
		return d.createFresh(ctx)
	}
	return d.restoreFrom(ctx, cp)
}

// EnsureSuspended checkpoints the actor's pod (if it exists) via FluidCR and
// waits for FluidCR to free its GPU -- a no-op if the pod is already gone.
func (d *Driver) EnsureSuspended(ctx context.Context) error {
	var pod corev1.Pod
	err := d.Client.Get(ctx, types.NamespacedName{Namespace: d.Namespace, Name: d.PodName}, &pod)
	if apierrors.IsNotFound(err) {
		return nil // already suspended
	}
	if err != nil {
		return fmt.Errorf("checking for existing pod %s/%s: %w", d.Namespace, d.PodName, err)
	}

	ckptName := d.PodName + "-ckpt"
	ckpt := &fluidcrclient.FluidCRCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: ckptName, Namespace: d.Namespace},
		Spec: fluidcrclient.FluidCRCheckpointSpec{
			WorkloadRef: fluidcrclient.WorkloadReference{
				APIVersion: "v1",
				Kind:       "Pod",
				Name:       d.PodName,
				Namespace:  d.Namespace,
			},
		},
	}
	// Idempotent: delete any stale checkpoint object from a previous cycle
	// under the same name before creating this round's.
	_ = d.Client.Delete(ctx, &fluidcrclient.FluidCRCheckpoint{ObjectMeta: metav1.ObjectMeta{Name: ckptName, Namespace: d.Namespace}})
	if err := d.Client.Create(ctx, ckpt); err != nil {
		return fmt.Errorf("creating FluidCRCheckpoint %s/%s: %w", d.Namespace, ckptName, err)
	}

	for {
		var got fluidcrclient.FluidCRCheckpoint
		if err := d.Client.Get(ctx, types.NamespacedName{Namespace: d.Namespace, Name: ckptName}, &got); err != nil {
			return fmt.Errorf("polling FluidCRCheckpoint %s/%s: %w", d.Namespace, ckptName, err)
		}
		switch got.Status.Phase {
		case fluidcrclient.FCPhaseCompleted:
			if len(got.Status.Pods) == 0 || len(got.Status.Pods[0].CheckpointFiles) == 0 {
				return fmt.Errorf("FluidCRCheckpoint %s/%s completed with no checkpoint file recorded", d.Namespace, ckptName)
			}
			path := got.Status.Pods[0].CheckpointFiles[0].FilePath
			d.mu.Lock()
			d.checkpoint = &savedCheckpoint{path: path}
			d.mu.Unlock()
			return nil
		case fluidcrclient.FCPhaseFailed:
			return fmt.Errorf("FluidCRCheckpoint %s/%s failed: %s", d.Namespace, ckptName, got.Status.Message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.pollInterval()):
		}
	}
}

func (d *Driver) createFresh(ctx context.Context) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: d.PodName, Namespace: d.Namespace},
		Spec:       d.PodSpec,
	}
	if err := d.Client.Create(ctx, pod); err != nil {
		return fmt.Errorf("creating fresh pod %s/%s: %w", d.Namespace, d.PodName, err)
	}
	return nil
}

func (d *Driver) restoreFrom(ctx context.Context, cp *savedCheckpoint) error {
	restoreName := d.PodName + "-restore"
	resume := true
	restore := &fluidcrclient.FluidCRRestore{
		ObjectMeta: metav1.ObjectMeta{Name: restoreName, Namespace: d.Namespace},
		Spec: fluidcrclient.FluidCRRestoreSpec{
			Archive: fluidcrclient.ArchiveSource{
				Path:      cp.path,
				Container: d.ContainerName,
			},
			PodName: d.PodName,
			Template: fluidcrclient.RestorePodTemplate{
				Spec: d.PodSpec,
			},
			Resume: &resume,
		},
	}
	_ = d.Client.Delete(ctx, &fluidcrclient.FluidCRRestore{ObjectMeta: metav1.ObjectMeta{Name: restoreName, Namespace: d.Namespace}})
	if err := d.Client.Create(ctx, restore); err != nil {
		return fmt.Errorf("creating FluidCRRestore %s/%s: %w", d.Namespace, restoreName, err)
	}

	for {
		var got fluidcrclient.FluidCRRestore
		if err := d.Client.Get(ctx, types.NamespacedName{Namespace: d.Namespace, Name: restoreName}, &got); err != nil {
			return fmt.Errorf("polling FluidCRRestore %s/%s: %w", d.Namespace, restoreName, err)
		}
		switch got.Status.Phase {
		case fluidcrclient.FRPhaseCompleted:
			return nil
		case fluidcrclient.FRPhaseFailed:
			return fmt.Errorf("FluidCRRestore %s/%s failed: %s", d.Namespace, restoreName, got.Status.Message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.pollInterval()):
		}
	}
}

// fetchActorStateTimeout bounds one FetchActorState call within Run's tick
// -- see substratebridge.tickTimeout's doc comment for why this is needed.
const fetchActorStateTimeout = 15 * time.Second

// Transition is one applied state change, for logging.
type Transition struct {
	ActorState string
	NowRunning bool
}

// Run polls fetcher on interval and drives driver's EnsureRunning/
// EnsureSuspended on each real transition, the same idempotent-diff shape
// substratebridge.Run uses for the Exclusive tier.
func Run(ctx context.Context, fetcher substratebridge.ActorStateFetcher, driver *Driver, interval time.Duration, onTransition func(Transition), onError func(error)) {
	var lastRunning *bool

	tick := func() {
		// Same hang this package's Run shares with substratebridge.Run: a
		// stuck kubectl-ate invocation (e.g. a stalled port-forward dial)
		// has no deadline of its own, so it would otherwise block this
		// whole tick -- and this loop's single goroutine -- forever.
		// EnsureRunning/EnsureSuspended below intentionally get no such
		// timeout: a checkpoint/restore cycle is expected to take seconds
		// and must run to completion, not be cut off mid-operation.
		fetchCtx, cancel := context.WithTimeout(ctx, fetchActorStateTimeout)
		obs, err := fetcher.FetchActorState(fetchCtx)
		cancel()
		if err != nil {
			if onError != nil {
				onError(fmt.Errorf("fetching actor state: %w", err))
			}
			return
		}
		actorState := obs.State
		wantRunning := substratebridge.DesiredStateForActorState(actorState) == gnwesv1alpha1.VMGPUDesiredAttached

		if lastRunning != nil && *lastRunning == wantRunning {
			return
		}

		var opErr error
		if wantRunning {
			opErr = driver.EnsureRunning(ctx)
		} else {
			opErr = driver.EnsureSuspended(ctx)
		}
		if opErr != nil {
			if onError != nil {
				onError(fmt.Errorf("applying desired running=%v: %w", wantRunning, opErr))
			}
			return
		}

		lastRunning = &wantRunning
		if onTransition != nil {
			onTransition(Transition{ActorState: actorState, NowRunning: wantRunning})
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}
