# Handover 5 — the Substrate actor-lifecycle bridge

**Status: DONE and verified live, within this environment's limits.**
Supersedes handover 4's "No bridge..." item (left in place there for
history; this is the continuity summary for what closed it).

## What this is

Handover 4 built `VMGPUAttachment` (real hot-attach/detach against
cloud-hypervisor) but named one explicit gap: nothing decided *when* to
flip `spec.desiredState` — that was a manual `kubectl patch`. This stage
built that decision-maker:

- **`internal/substratebridge`** — the pure logic. `DesiredStateForActorState`
  maps a Substrate `ActorState` string to `Attached`/`Detached`
  (conservative: only `ACTOR_STATE_RUNNING` attaches; every other state,
  including the transitional `RESUMING`/`SUSPENDING`, detaches — see the
  function's doc comment for why). `ReconcileOnce`/`Run` poll an injected
  `ActorStateFetcher`, diff against an injected `AttachmentDriver`'s current
  state, and only write when something actually changed.
- **`cmd/substrate-gpu-bridge`** — the real wiring: a `kubectlAteFetcher`
  that shells out to `kubectl-ate get actor <name> --atespace <space> -o
  json` (same invocation pattern `config/substrate-integration` already
  established, not a new one), and a `k8sAttachmentDriver` using a plain
  `controller-runtime` client (`client.New`, the same lightweight pattern
  `cmd/loadtest` already uses — no full manager/leader-election needed for a
  one-shot poller).
- **`cmd/fakekubectlate`** — a stand-in for `kubectl-ate get actor -o json`,
  reading its answer from a plain text file (`FAKE_ACTOR_STATE_FILE`) so a
  test can flip "the actor's state" without a real Substrate control plane,
  the same spirit as `cmd/fakevmm` standing in for a real VMM.

## What's actually verified, concretely

1. **Unit level** (`internal/substratebridge/bridge_test.go`): the state
   mapping for every `ActorState` value in the real proto enum plus unknown
   ones; JSON parsing against both a full and minimal real-shaped actor
   JSON and two failure shapes; `ReconcileOnce` applying a change only when
   needed, propagating fetch/write errors without guessing a state; and a
   `Run`-loop test that drives a fake actor through
   `SUSPENDED → RUNNING → SUSPENDED` over real wall-clock polls and confirms
   exactly those two transitions land, in order, with the loop stopping
   cleanly on context cancellation.
2. **Live, end-to-end, against the real `kind-kind` cluster**: ran the real
   `substrate-gpu-bridge` binary against the real `fakekubectlate` binary,
   driving a real `VMGPUAttachment` that the real `VMGPUAttachmentReconciler`
   (from handover 4) was reconciling against real `cmd/fakevmm`. Sequence,
   confirmed via `kubectl get vmgpuattachment -o wide` and the bridge's own
   log after each step:
   - Started with the fake actor `ACTOR_STATE_SUSPENDED` → attachment
     stayed `Detached` (no spurious attach).
   - Flipped the actor-state file to `ACTOR_STATE_RUNNING` → within ~2s
     (one poll interval), `status.phase` reached `Attached` with a real
     (fake-VMM-assigned) guest BDF.
   - Flipped back to `ACTOR_STATE_SUSPENDED` → `status.phase` reached
     `Detached`, device released back to the pool.
   - Bridge log showed exactly two `actor state X -> desiredState Y` lines
     for the whole run — no extra writes while the state was unchanged
     between flips.

## What's NOT verified

- **No real `kubectl-ate` binary, no real Substrate control plane.** The
  real `ateapi.v1.Actor` JSON shape (`status.state`, protojson's camelCase
  field naming and full-enum-name string values) was confirmed by reading
  `api/ateapi/v1/ateapi.proto` and
  `cmd/kubectl-ate/internal/cmd/actor.go`/`printer.go` directly in the real
  `agent-substrate/substrate` clone at `D:\codes\agent-substrate` — a source
  read, not a test against the real binary and a real running actor.
- Everything handover 4 already flagged as unverified (real
  cloud-hypervisor/KVM, real `ateworker-microvm` deployment) is still
  unverified — this stage didn't touch that boundary.

## Concrete next steps

1. **Run this against a real `kubectl-ate` and a real Substrate control
   plane** — `config/substrate-integration` already has one stood up
   (gVisor worker); point `-kubectl-ate-bin`/`-kubectl-ate-kubeconfig` at
   the real cross-compiled binary and real cluster from that setup, watch a
   real `kubectl-ate resume actor`/`suspend actor` cycle drive a
   `VMGPUAttachment` for real. This is the cheapest remaining step — no new
   hardware needed, just wiring two already-built things together.
2. Everything handover 4 already listed (real cloud-hypervisor/KVM, real
   `ateworker-microvm`) remains the harder, hardware-gated remainder.
