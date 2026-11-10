# GNWES NeoCloudCon demo run-book

## The one sentence

> "Standing up a GPU from scratch takes seconds. Attaching an already-warm
> one to a running agent should take milliseconds — GNWES makes that the
> normal path."

## What's real vs simulated in this build

Be upfront about this on stage — it's a control-plane proof of concept, not a
claim of measured performance on real GPUs.

| Layer | This demo | Full research design |
|---|---|---|
| CRDs, controllers, reconcile loop, finalizer-based cleanup | **Real** — actual Kubernetes API objects and controller-runtime reconcilers | Same |
| Warm pool bookkeeping (acquire/release, race-free under concurrent leases) | **Real** — in-memory, mutex-protected | Same idea, same concurrency property |
| "Standing up a GPU slot" cost (`slotInitMillis`) | **Simulated** — a sleep | Real device/driver init, VRAM clear, possibly microVM boot |
| "Attaching a slot to a running actor" cost (`attachLatencyMillis`) | **Simulated** — a sleep, then a pod annotation patch | Real DRA ResourceClaim resolution + CDI device injection into a running sandbox |
| Scheduler gating, fairness arbiter, predictive autotuner | **Not built** in this demo | Designed in the research docs (Area 1 Phase B / Area 2) |

The number the demo produces — "leasing from a warm pool is ~Nx faster than
provisioning a dedicated slot per actor" — is a **structural** result (it
falls directly out of paying `slotInitMillis` once vs. every time), not a
hardware benchmark. Say that plainly if asked.

## Run-book

1. Make sure Docker Desktop is running.
2. `bash hack/up.sh` — creates the kind cluster, installs the two CRDs,
   creates the `gnwes-demo` namespace.
3. Terminal A: `go run ./cmd/manager` — leave this running; it prints a log
   line every time a pool reconciles or a lease binds.
4. Terminal B: `go run ./cmd/demo -mode=both -actors=8 -pool-size=3`

### What you'll see

**Warm path** — one pool created once, 8 actors cycle through it:

```
pool warm-up (one-time, paid ahead of any actor): 4021ms for 3 slots
  actor 1   slot=mock-gpu-0   attach=151ms   (controller-measured=151ms)
  actor 2   slot=mock-gpu-1   attach=152ms   (controller-measured=152ms)
  ...
--- warm (lease attach): n=8  min=150ms  avg=153ms  max=158ms ---
```

**Cold path** — every actor pays full slot-init on its own critical path:

```
  actor 1   slot=mock-gpu-0   total=4172ms   (slot-init+attach, no reuse)
  actor 2   slot=mock-gpu-1   total=4169ms   (slot-init+attach, no reuse)
  ...
--- cold (dedicated slot): n=8  min=4150ms  avg=4171ms  max=4190ms ---
```

**Comparison** — the slide-worthy line:

```
 warm (lease attach, pool reused):   avg 153ms per actor
 cold (dedicated slot per actor):    avg 4171ms per actor
 => leasing from a warm GNWES pool is ~27x faster than provisioning a dedicated GPU slot per actor
```

Tune `-slot-init-ms` / `-attach-ms` to match whatever real published
GPU-attach numbers you want the simulation to track for the talk.

### While it's running, worth pointing at

- `kubectl get gpuworkerpool -n gnwes-demo` — watch `AVAILABLE`/`LEASED`
  columns move as `cmd/demo` cycles actors.
- `kubectl get gpulease -n gnwes-demo -o wide` — `ATTACHMS` column is the
  controller's own measurement, independent of the demo driver's client-side
  timer — the two should agree.
- `kubectl get pod actor-1 -n gnwes-demo -o jsonpath='{.metadata.annotations}'`
  (while actor 1's lease is still bound) — shows the `gnwes.io/gpu-slot`
  annotation, standing in for real CDI device injection.

## Tear down

```bash
bash hack/down.sh
```
