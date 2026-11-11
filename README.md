# GNWES — GPU-Native Warm Ephemeral Substrate (demo build)

A runnable proof-of-concept of the core GNWES idea from the research docs:
**lease a GPU by the second out of a pre-warmed pool, instead of provisioning
one from scratch for every ephemeral agent.**

This is a conference-demo build, not the full research design. It implements
the single headline flow — warm pool → sub-second lease attach → detach and
reuse — using a **simulated** GPU device (no real NVIDIA hardware, no real
DRA/CDI calls) so it runs anywhere `kind` + Docker run. The predictive/
adaptive autoscaling loop, fairness arbiter, and real DRA+CDI wiring from the
research docs are out of scope for this build; see `DEMO.md` for exactly what
is and isn't simulated and why.

## What's here

- **`api/v1alpha1`** — two CRDs:
  - `GPUWorkerPool` — a pool of N warm, simulated GPU slots. Standing a slot
    up costs `slotInitMillis` (simulates real device/driver init) — paid once,
    ahead of any actor needing a GPU.
  - `GPULease` — a request to attach one warm slot to a running actor pod.
    Binding costs only `attachLatencyMillis` (simulates CDI injection into an
    already-running sandbox) — the fast path this research argues for.
- **`internal/pool`** — the in-memory, mutex-protected slot bookkeeping
  (`Acquire`/`Release`/`Resize`) that both controllers share. This is the
  actual source of truth; CR status mirrors it for visibility.
- **`internal/controller`** — three reconcilers:
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
- **`cmd/manager`** — runs all three controllers against whatever cluster
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

This isn't just a standalone simulation anymore: **`config/substrate-integration`**
documents a verified, real integration with the upstream
[agent-substrate/substrate](https://github.com/agent-substrate/substrate)
project — GNWES gating and leasing a genuine Substrate `WorkerPool` pod, with
a real Substrate actor repeatedly resuming onto that exact, GPU-attached pod.
Read that directory's `README.md` for what's proven, what's still simulated,
and how to reproduce it.

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
