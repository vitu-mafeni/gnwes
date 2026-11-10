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
- **`cmd/manager`** — runs both controllers against whatever cluster your
  kubeconfig points at (no in-cluster Deployment needed for this demo).
- **`cmd/demo`** — the demo driver: cycles ephemeral actor pods through
  leases and prints attach-latency numbers, in `warm`, `cold`, or `both` mode.

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
