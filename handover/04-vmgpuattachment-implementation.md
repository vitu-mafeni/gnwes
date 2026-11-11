# Handover 4 — VMGPUAttachment: a real, working WorkerPool-for-GPUs analog

**Status: DONE and verified live, within this environment's limits.**
Full detail lives in `config/vmgpu/README.md` — read that first; this is
the continuity summary.

## What this is

The user asked for "an actual implementation solving the problem in
Substrate similar to how workerpool works but for GPUs" — i.e., not another
research writeup, a real mechanism. Built on exactly the path Stage 3
(`research/live-attach-poc/README.md`) identified as structurally solvable:
cloud-hypervisor (which Substrate's microVM worker, `ateworker-microvm`,
already runs via Kata) ships a live VFIO device hotplug/hot-unplug REST API
today. gVisor does not and was confirmed architecturally blocked (its
`nvproxy` device registry is one-time, creation-only) — so this targets
cloud-hypervisor specifically, not "GPUs in general."

Two new CRDs, mirroring `GPUWorkerPool`/`GPULease`'s shape exactly:

- **`VFIOGPUPool`** — a node's real, physical VFIO-bound GPU inventory
  (fixed list, not elastic like `GPUWorkerPool.spec.poolSize`).
- **`VMGPUAttachment`** — hot-attaches/detaches one device into/out of an
  already-running cloud-hypervisor guest. The key design choice: driven by
  flipping `spec.desiredState` (`Attached`/`Detached`) on the SAME object,
  not by create/delete — this is what lets one object track a device across
  many attach/detach cycles as a worker is reused across many actors, which
  is the actual property Substrate's model needs and `GPULease` never had
  to solve (`GPULease` creates a new lease object per actor-pod anyway).

New packages: `internal/chclient` (a real cloud-hypervisor REST client, wire
format from its own OpenAPI spec), `internal/vfiopool` (real-device
bookkeeping, same shape/safety property as `internal/pool`), and
`cmd/fakevmm` (a stand-in VMM for testing without real cloud-hypervisor/KVM
hardware, which this environment doesn't have).

## What's actually verified, concretely

1. **Unit level**: `internal/chclient`'s tests run a real HTTP server on a
   real unix socket (not mocked at the transport layer) and exercise
   add-device (200/204/409), remove-device (204/404), ping, and the
   readiness retry loop.
2. **Reconciler level**: `internal/controller/vmgpuattachment_controller_test.go`
   covers attach, idempotent re-reconcile, detach-releases-to-pool, a full
   attach→detach→re-attach cycle, a failed `AddDevice` correctly releasing
   the device rather than leaking it as stuck, and delete-while-attached
   detaching first.
3. **Live, end-to-end, against a real cluster**: ran a real `cmd/manager`
   process against this repo's `kind-kind` cluster, created real
   `VFIOGPUPool`/`VMGPUAttachment` objects, and watched the real reconcile
   loop make real HTTP-over-unix-socket calls to `cmd/fakevmm` — confirmed
   `status.phase` reaching `Attached` (with a guest BDF) in ~7ms, then
   `Detached` after flipping `spec.desiredState`, with the device correctly
   released back to the pool both times (confirmed via the pool's own
   status once its own object re-reconciled — see the note below about
   that lag).

One real operational lesson from that run, now in
`config/vmgpu/README.md`'s Troubleshooting section: stopping `cmd/manager`
before deleting a namespace containing an Attached-or-was-Attached
`VMGPUAttachment` leaves its finalizer stuck forever (hit this first-hand;
fixed by stripping the finalizer directly since the object was already
`Detached` with nothing left to clean up).

## What's NOT verified — the honest limits

- **No real cloud-hypervisor instance.** This dev environment has no
  `/dev/kvm` (confirmed in `research/live-attach-poc/README.md` — checked
  inside the `kind-kind` node, which runs under Docker Desktop's WSL2 VM on
  Windows). Nested virtualization isn't available, so a real
  cloud-hypervisor VM can't be booted here at all. Every test run so far
  used `cmd/fakevmm` standing in for it.
- **No real Substrate `ateworker-microvm` deployment.** The existing
  `config/substrate-integration` work used Substrate's gVisor worker, not
  its microVM one.
- **No bridge from Substrate's actual actor lifecycle to
  `spec.desiredState`.** Substrate's `Actor`/`WorkerAssignment` aren't
  Kubernetes CRDs (confirmed: grepped the real `agent-substrate/substrate`
  clone at `D:\codes\agent-substrate` for `DeepCopyObject`/
  `kubebuilder:object:root` — only `WorkerPool` has it). A real bridge would
  poll `kubectl-ate get actor -o json` (confirmed that command exists and
  supports JSON output via `cmd/kubectl-ate/internal/cmd/actor.go`'s
  `getActorsCmd`) rather than watch anything natively. Not built — flipping
  `desiredState` today is a manual `kubectl patch` or whatever external
  driver you write against it.

## Concrete next steps, in order of how much new infrastructure they need

1. **Write the actor-lifecycle bridge** (`cmd/substrate-gpu-bridge` or
   similar): poll `kubectl-ate get actor <name> -o json`, flip the matching
   `VMGPUAttachment.spec.desiredState` on resume/suspend transitions. Needs
   no new hardware, just Substrate's gVisor-or-microVM control plane already
   running (which `config/substrate-integration` already stands up) — the
   smallest next step.
2. **Get a real cloud-hypervisor instance reachable** — either a machine
   with `/dev/kvm` (a bare-metal Linux box, or a cloud VM with nested virt
   enabled), or the user's real GPU node (`dcn-gpu-0` from earlier in this
   session) if it supports KVM natively, which a bare-metal GPU research
   node very plausibly does (not confirmed — would need the same
   "You run commands, paste output" collaboration this session already
   established for that node). Point `VMGPUAttachment.spec.apiSocket` at
   its real api-socket instead of `fakevmm`'s.
3. **Stand up Substrate's `ateworker-microvm` worker type** against that
   same environment, and confirm the guest's NVIDIA driver actually
   re-probes a hot-added PCI device without a reboot (flagged as unchecked
   in `research/live-attach-poc/README.md` too) — the one real technical
   unknown left that this implementation can't resolve by itself.

None of this is blocked on more code from this mechanism's side — it's
blocked on hardware/environment access this session doesn't have.
