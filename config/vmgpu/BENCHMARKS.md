# GPU hot-attach/detach latency, measured on real hardware

Every number below was measured on this project's own real bare-metal
cluster (`server1` control-plane + `dcn-gpu-0`, 1x real NVIDIA TITAN RTX),
driving real `kubectl-ate resume/suspend actor` calls against the real
`gnwes-counter-exclusive` WorkerPool (Exclusive tier, `microvm-nvidia-gpu`
SandboxConfig, the virtio-blk rootfs fix from 2026-11-16) — not a
simulation, and not `cmd/demo`'s simulated `GPUWorkerPool`/`GPULease`
slots (see `DEMO.md` for that one; it's a *structural* illustration, by
its own admission, not a hardware benchmark). This doc only covers the
real `VFIOGPUPool`/`VMGPUAttachment` path.

**Reproduce it yourself:** `bash config/vmgpu/bench-gpu-attach.sh
<namespace> <worker-pool-label-selector> <trials> <template-name>` from
any node with `kubectl`/`kubectl-ate` access to the cluster. Defaults
match this doc's own run (`ate-demo-counter`,
`ate.dev/worker-pool=gnwes-counter-exclusive`, 5 trials,
`gnwes-nvidia-smi`). Needs the WorkerPool, SandboxConfig, and actor
template this repo's own `README.md` and `nvidia-smi-test/README.md`
already set up; it creates and deletes its own actors, and leaves nothing
behind.

## Headline numbers (2026-11-16)

| Measurement | Value | What it actually is |
|---|---|---|
| **GPU attach** (reconciler-measured) | **468ms avg** (425-503ms, n=3) | `VFIOGPUPoolReconciler` acquiring the real device + the real `vm.add-device` HTTP call to cloud-hypervisor succeeding. The actual "lease a GPU" operation. |
| **GPU detach** (reconciler-measured) | **124ms avg** (123-124ms, n=3) | The real `vm.remove-device` call + releasing the device back to the pool. |
| Guest kernel boot → kata-agent answers | **903ms avg** (887-926ms, n=6) | `agent_dial` in the actor's own "Actor boot phases" log line — the dominant cost of a cold microVM boot, measured from inside `ateworker-microvm` itself. |
| Container setup inside an already-answering guest | **86ms avg** (83-92ms, n=6) | `containers` in the same log line — creating this actor's specific container(s) once the guest agent is reachable. |
| Full cold microVM boot (`vsock_wait`+`agent_dial`+`containers`+`wakeup_probe`) | **989ms avg** (970-1018ms, n=6) | `since_boot` in the same log line: the whole cost of standing up a fresh microVM for one actor, with no GPU involved yet. |
| `kubectl-ate resume actor` wall-clock (create → `ACTOR_STATE_RUNNING`) | **2.26s avg** (2.06-3.02s, n=8) | The externally observed resume latency: VM boot + agent setup + the Substrate control-plane round trip around it. |
| Wall-clock from `RUNNING` to `VMGPUAttachment` phase=`Attached` | **3.1s avg** (2.79-3.51s, n=3) | Dominated by `cmd/substrate-gpu-bridge`'s own 1-second poll interval (`-poll-interval`), not the attach operation itself — see "Where the time actually goes" below. |

## Where the time actually goes

A cold actor resume with a GPU, end to end, breaks down roughly like this:

```
actor create+resume ----------------------------------- ~2.3s  (microVM cold boot, no GPU yet)
  agent_dial  ~0.90s  <- guest kernel boot + kata-agent answering over vsock (THE dominant cost)
  containers  ~0.09s  <- this actor's container(s) created inside the guest
  (control-plane RPC/scheduling overhead makes up the rest of the ~2.3s)
bridge notices RUNNING, writes desiredState=Attached ---- up to ~1s   (bridge's own poll interval)
reconciler acquires device + vm.add-device -------------- ~0.47s  (the real GPU attach)
```

**The GPU attach/detach operations themselves are already sub-second** —
424-503ms to attach, ~124ms to detach, comparable to `cmd/demo`'s
*simulated* "warm lease" figure (~150-300ms, see `DEMO.md`), except this
is real cloud-hypervisor vm.add-device/vm.remove-device against real VFIO
hardware, not a `time.Sleep`. The two real bottlenecks are the microVM's
own cold boot (dominated by `agent_dial`, ~900ms) and
`cmd/substrate-gpu-bridge`'s 1-second poll interval, neither of which is
a property of the GPU-attach mechanism itself.

## Projected: resuming onto an already-booted, idle microVM

**Nothing in this repo or in Agent Substrate currently keeps a pool of
pre-booted, idle microVMs on standby** — every actor resume cold-boots a
fresh microVM (the ~989ms `since_boot` cost above). Agent Substrate's own
"prewarm" mechanism (`cmd/atelet/runtime_prewarm.go`) only pre-stages
sandbox *runtime assets* (the cloud-hypervisor/virtiofsd/kernel/kata-image
binaries themselves) into its node-local cache ahead of time — it does
not keep a VM booted and waiting.

This projection is **not measured** — it is a structural estimate from
the real component numbers above, in the same spirit as (and clearly
labeled the same way as) `cmd/demo`'s own simulated warm-pool figure. If
a microVM were kept booted and idle, with its kata-agent already dialed
and connected, resuming an actor onto it would skip `vsock_wait` and
`agent_dial` entirely (the guest is already past that point) and pay only:

| Scenario | Projected / measured latency | Basis |
|---|---|---|
| Resume onto an idle, pre-booted VM, no GPU | **~86ms** | `containers` phase alone (measured) |
| Resume onto an idle, pre-booted VM, with GPU attach | **~554ms** | `containers` (~86ms, measured) + reconciler attach (~468ms, measured) |
| Resume today: cold VM boot, no GPU | **~989ms** (measured) | `since_boot` |
| Resume today: cold VM boot + GPU attach, bridge poll included | **~5.3s** (measured, composed from the two wall-clock rows above) | `resume_cmd_wall_ms` + `RUNNING`→`Attached` wall-clock |

So a pre-booted idle VM pool, if it existed, would plausibly cut a
GPU-backed resume from **~5.3s to ~0.5-0.6s** — roughly **9-10x** — with
most of that gain coming from skipping the cold boot's `agent_dial` wait,
not from the GPU attach step (which is already fast). Tightening
`cmd/substrate-gpu-bridge`'s poll interval would capture a meaningful
slice of the remaining gap even without any VM pre-warming at all.
Building that pool is unexplored, real engineering — this table is a
lower-bound estimate from real, already-measured components, not a
commitment to a number a real implementation would hit.

## Raw trial data

**GPU attach/detach (reconciler-measured, `attachLatencyMillis`), 3 trials:**

| trial | attach (ms) | detach (ms) | guest BDF |
|---|---|---|---|
| 1 | 503 | 124 | 0000:00:07.0 |
| 2 | 477 | 123 | 0000:00:07.0 |
| 3 | 425 | 124 | 0000:00:07.0 |

**Actor boot phases (`ateworker-microvm`'s own log, nanoseconds converted to ms), 6 trials:**

| trial | vsock_wait | agent_dial | containers | wakeup_probe | since_boot |
|---|---|---|---|---|---|
| 1 | 0.02 | 887.4 | 83.4 | ~0 | 970.2 |
| 2 | 0.04 | 905.2 | 87.7 | 0.005 | 993.0 |
| 3 | 0.02 | 895.0 | 83.1 | 0.005 | 978.2 |
| 4 | 0.01 | 925.6 | 92.3 | 0.005 | 1017.8 |
| 5 | 0.02 | 899.6 | 86.3 | 0.007 | 985.9 |
| 6 | 0.02 | 903.5 | 84.2 | 0.006 | 987.7 |

**Wall-clock `kubectl-ate resume actor` / `suspend actor`, 8 trials each:**

resume: 3017, 2202, 2064, 2068, 2258, 2289, 2110, 2061 ms (avg 2259ms)
suspend: 2351, 2269, 1002, 1407, 1259, 1217, 1322, 1292 ms (avg 1515ms)
