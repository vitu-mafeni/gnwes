# GNWES — GPU-Native Warm Ephemeral Substrate (demo build)

A runnable proof-of-concept of the core GNWES idea from the research docs:
**lease a GPU by the second out of a pre-warmed pool, instead of provisioning
one from scratch for every ephemeral agent.**

**New to this repo? Start with [`RUNBOOK.md`](RUNBOOK.md)** — every command
to run and test every part of this, organized fastest-first, each one
actually run against a live cluster while writing it.

This is a conference-demo build, not the full research design. The core
headline flow — warm pool → sub-second lease attach → detach and reuse — runs
anywhere `kind` + Docker, using a **simulated** GPU device (`cmd/manager` +
`internal/controller`/`internal/pool`), documented honestly in `DEMO.md`.

**`config/dra` adds a second, real mechanism alongside it**: a genuine
Kubernetes DRA (Dynamic Resource Allocation) kubelet plugin
(`cmd/mock-gpu-driver`) that publishes real devices via a real `ResourceSlice`,
gets real `ResourceClaim`s allocated by the real scheduler, and writes a real
CDI (Container Device Interface) spec that containerd genuinely injects into
the container — verified by `kubectl exec`-ing into the running container and
seeing the injection take effect. Only the device identity is mocked (no real
NVIDIA hardware exists in this environment); everything else in that path —
the API objects, the scheduling, the kubelet plugin protocol, the CDI
file and its injection — is real. `cmd/mock-gpu-driver` now also has a real
NVIDIA backend for running this same mechanism against an actual GPU node
instead of the mock device, **verified end-to-end on a real 3x NVIDIA TITAN
RTX node** (GPU Operator-managed, CRI-O runtime): a genuine `ResourceClaim`
allocation followed by `nvidia-smi` inside the container showing exactly the
one physical GPU that claim was allocated, not the node's other GPUs. See
`config/dra/README.md`.

The predictive/adaptive autoscaling loop and fairness arbiter from the
research docs are still out of scope for this build.

## What's here

- **`api/v1alpha1`** — four CRDs:
  - `GPUWorkerPool` — a pool of N warm, simulated GPU slots. Standing a slot
    up costs `slotInitMillis` (simulates real device/driver init) — paid once,
    ahead of any actor needing a GPU.
  - `GPULease` — a request to attach one warm slot to a running actor pod.
    Binding costs only `attachLatencyMillis` (simulates CDI injection into an
    already-running sandbox) — the fast path this research argues for.
  - `VFIOGPUPool` — the real counterpart to `GPUWorkerPool`: a fixed
    inventory of actual, physical VFIO-bound GPUs on one node.
  - `VMGPUAttachment` — the real counterpart to `GPULease`: a request to
    hot-attach (or hot-detach) one real device into/out of an
    already-running cloud-hypervisor guest, driven by flipping
    `spec.desiredState` rather than by creating/deleting the object.
- **`internal/pool`** / **`internal/vfiopool`** — the in-memory,
  mutex-protected device bookkeeping (`Acquire`/`Release`) each pair of
  controllers shares — simulated slots and real VFIO devices respectively.
  This is the actual source of truth; CR status mirrors it for visibility.
- **`internal/chclient`** — a Go client for the subset of cloud-hypervisor's
  REST VMM API needed to hot-attach/detach a real device (`vm.add-device`,
  `vm.remove-device`), wire format taken from cloud-hypervisor's own OpenAPI
  spec, not guessed. `cmd/fakevmm` is a stand-in VMM for testing this
  without real cloud-hypervisor/KVM hardware available.
- **`internal/controller`** — five reconcilers:
  - `GPUWorkerPoolReconciler` grows/shrinks the pool toward `spec.poolSize`
    and reports `Warming`/`Ready` status.
  - `PodGateReconciler` — **scheduler gating / admission.** An actor pod
    created with the `gnwes.io/gpu-capacity` scheduling gate (the stable,
    GA `spec.schedulingGates` API — no scheduler plugin or kube-scheduler
    fork) and a `gnwes.io/gpu-pool` label is held un-schedulable until this
    reconciler actually reserves it a warm slot. Only then does it remove the
    gate, so the default scheduler never binds (and the cluster never spends
    node resources on) a GPU-needing actor that has nothing to attach to.
  - `GPULeaseReconciler` inherits that reservation if one exists (via
    `pool.Transfer`, race-free against another actor grabbing the slot in
    between), otherwise acquires a slot itself; simulates the attach cost;
    annotates the target pod with `gnwes.io/gpu-slot`; and releases the slot
    back to the pool on delete or TTL expiry (via a finalizer, so cleanup
    always runs).
  - `VFIOGPUPoolReconciler` / `VMGPUAttachmentReconciler` — the REAL
    counterpart to the two above: hot-attaches/detaches an actual VFIO GPU
    device into/out of an already-running cloud-hypervisor guest via its
    REST API (`internal/chclient`), driven by flipping
    `VMGPUAttachment.spec.desiredState` rather than by creating/deleting the
    object — so one object tracks a device across many attach/detach cycles
    as a worker is reused, matching how Agent Substrate's microVM worker
    actually operates. See `config/vmgpu/README.md` for what's verified
    (the whole reconcile loop, live, against a real Kubernetes cluster —
    just not yet against a real cloud-hypervisor+KVM instance, which this
    dev environment doesn't have) and what isn't yet.
- **`cmd/manager`** — runs all five controllers against whatever cluster
  your kubeconfig points at (no in-cluster Deployment needed for this demo).
- **`cmd/demo`** — the demo driver: cycles ephemeral actor pods through
  leases and prints attach-latency numbers, in `warm`, `cold`, or `both` mode.
- **`cmd/podgate-webhook`** + **`config/webhook`** — a mutating admission
  webhook that injects the `gnwes.io/gpu-capacity` scheduling gate onto any
  pod labeled `gnwes.io/gpu-pool`. Needed to gate pods GNWES doesn't create
  itself — see the Substrate integration below.
- **`cmd/loadtest`** — a concurrency correctness check against a LIVE
  cluster: many actors race a deliberately undersized pool at once, while a
  monitor polls every Bound lease's slot ID and fails immediately on any
  double-booking. See Testing below.

## Testing

- `go test ./...` — unit tests for `internal/pool` (including a dedicated
  concurrency test, `TestConcurrentAcquireReleaseNeverDoubleBooks`, best run
  with `-race`) and reconcile-level tests for all three controllers against a
  fake client (`internal/controller/*_test.go`), covering the full attach/
  release/TTL/reservation-inheritance/deletion-cleanup matrix, not just the
  happy path.
- `go run ./cmd/loadtest` — the same correctness property, but against the
  REAL controllers on a real cluster, under real concurrent contention
  (default: 40 actors, 10 at a time, racing a 3-slot pool). Verified clean:
  `succeeded=40 failed=0 double-booking violations=0`.
- `.github/workflows/ci.yaml` — build/vet/gofmt/`-race` unit tests on every
  push/PR, plus a second job that stands up a real `kind` cluster and runs
  `cmd/loadtest` against it as an integration gate.

`-race` needs cgo, which needs a C compiler; this dev machine doesn't have
one, so unit tests here run without it — CI's Linux runners do have one, so
`-race` is enforced there.

## Real Agent Substrate integration

**`config/substrate-integration`** documents a verified, real integration with
the upstream [agent-substrate/substrate](https://github.com/agent-substrate/substrate)
project — GNWES gating and leasing a genuine Substrate `WorkerPool` pod, with
a real Substrate actor repeatedly resuming onto that exact pod. Read that
directory's `README.md` for what's proven, what's still simulated, and how to
reproduce it.

**What this does *not* claim**: it does not attach a real GPU to that pod
across the resumes — the attach step there is still GNWES's own stand-in
(sleep + annotation), same as the base demo. Doing that for real would mean
attaching/detaching a device on a worker pod that Substrate *reuses* across
many sequential actors, rather than one created fresh per workload — and
that's not a GNWES shortcut, it's an open problem upstream too: Substrate's
own Nov 2026 announcement states GPU support "is being rebuilt" on Kubernetes
DRA and is tracked as an open design issue
([agent-substrate/substrate#752](https://github.com/agent-substrate/substrate/issues/752)),
with GPU suspend/resume across actors described only as "a design" they
propose, not something shipped. `config/dra` (below) proves the real DRA+CDI
mechanism works end-to-end on real hardware, but only for a device bound at
pod-creation time — the vanilla K8s DRA model. Bridging that to Substrate's
reuse-the-same-pod model is the genuine, still-open piece of work, not a
detail either project has already solved.

**`config/vmgpu` is a real attempt at closing exactly that gap** (not a
drop-in fix — see its own README for the honest scope): `VMGPUAttachment`
hot-attaches/detaches a real device on an already-running guest by flipping
a field rather than creating/deleting a pod, targeting cloud-hypervisor
specifically because Substrate's microVM worker already runs on it and it
already ships the hotplug API this needs (unlike gVisor, confirmed
architecturally blocked — see `research/live-attach-poc/README.md`). The
whole mechanism is verified live against a real Kubernetes cluster; the VMM
itself, in every run so far, is a local stand-in
(`cmd/fakevmm`), not a real cloud-hypervisor+KVM instance or a real
Substrate `ateworker-microvm` deployment — this dev environment has neither
available. See `config/vmgpu/README.md` for exactly what that does and
doesn't prove.

## Quickstart

Prerequisites: Go 1.23+, Docker Desktop running, `kind`, `kubectl` (all
already confirmed on this machine).

```bash
bash hack/up.sh
```

Then in one terminal:

```bash
go run ./cmd/manager
```

And in another:

```bash
go run ./cmd/demo -mode=both -actors=8 -pool-size=3
```

See `DEMO.md` for the full run-book, expected output, and talking points.

Tear down with `bash hack/down.sh`.

## Flags (`cmd/demo`)

| Flag | Default | Meaning |
|---|---|---|
| `-mode` | `both` | `warm`, `cold`, or `both` |
| `-actors` | `8` | number of ephemeral actors to cycle through |
| `-pool-size` | `3` | warm pool size (warm mode) |
| `-slot-init-ms` | `4000` | simulated one-time cost to stand up one slot |
| `-attach-ms` | `150` | simulated CDI-injection attach cost |
| `-work-ms` | `100` | simulated GPU work time per actor before release |
| `-namespace` | `gnwes-demo` | namespace to run in |
| `-gate` | `true` | hold each actor pod un-schedulable until a GPU slot is reserved for it (scheduler gating); `-gate=false` reverts to no admission check, for comparison |
