# Testing fast, real GPU give/take with agents, notebooks, and WorkerPools

This is a staged, step-by-step plan, not something already executed — it
builds on what's proven today (2026-11-11, on `dcn-gpu-0`): real
`chclient` code hot-attaching/detaching a real NVIDIA TITAN RTX into/out of
a real, already-running Cloud Hypervisor VM, and (separately) the real
`cmd/substrate-gpu-bridge` driving a real `VMGPUAttachment` from a real
Agent Substrate actor's real resume/suspend cycle. **Those two proofs have
not yet been run together, in the same test, with the same GPU.** That's
Phase 1 below.

Read this whole document before running anything — it's written so you can
review the plan first, given how much live debugging today's GPU test
needed.

## What each phase actually proves, at a glance

| Phase | Proves | New setup needed |
|---|---|---|
| 0 | Nothing new — gets the code onto `dcn-gpu-0` | A way to get gnwes's source there |
| 1 | Real actor lifecycle → real GPU attach/detach, combined for the first time | None beyond Phase 0 — reuses today's tools |
| 2 | The same thing, but through the *real* GNWES CRDs/controllers, not standalone test binaries | CRDs applied to the real cluster, `cmd/manager` running on `dcn-gpu-0` |
| 3 | The GPU attaches into the **agent's own sandbox**, not a side VM — the actual WorkerPool integration | Agent Substrate's microVM worker (Kata + Cloud Hypervisor) deployed |
| Notebook | Same fast attach/detach, driven by a Jupyter kernel's busy/idle state instead of an actor's resume/suspend state | A new (unbuilt) `jupyter-gpu-bridge`, same shape as `substrate-gpu-bridge` |

## Phase 0 — getting gnwes onto `dcn-gpu-0`

Everything from here on needs `cmd/manager`, `cmd/substrate-gpu-bridge`, or
both running on `dcn-gpu-0` itself (the unix socket constraint: whichever
process calls `chclient` must be on the same host as the VMM's api-socket).
Go 1.27.4 is already there, confirmed. Two ways to get the source there:

- **A — push `gnwes` to a git remote you control** (this repo currently has
  none — `git remote -v` is empty) and `git clone` it on `dcn-gpu-0`
  (confirmed it has outbound internet access: it already pulled Cloud
  Hypervisor releases and Ubuntu cloud images today). Fastest if you're
  willing to make the repo reachable from there (a private GitHub repo
  works fine).
- **B — reconstruct the needed files via heredoc**, the same way
  `gnwesrealtest`/`fakekubectlate` got there today. Needs `api/v1alpha1`,
  `internal/pool`, `internal/vfiopool`, `internal/chclient`,
  `internal/controller`, `internal/substratebridge`, `cmd/manager`,
  `cmd/substrate-gpu-bridge`, `go.mod`, `go.sum` — roughly 20 files. Slower
  to set up, but no external git hosting needed.

Decide which before starting Phase 2 (Phase 1 doesn't need either — see
below).

## Phase 1 — combine today's two proofs (no new deployment)

This reuses exactly what's already on `dcn-gpu-0`: `gnwesrealtest` (the
standalone `chclient` copy) and the real `kubectl-ate` Docker wrapper. No
GNWES CRDs, no `cmd/manager`, nothing new to deploy. **Honest caveat**: the
GPU still attaches into the standalone test VM from today, not into
`gnwes-actor-1`'s own (gVisor) sandbox — this phase proves the *automation
loop* end to end against real hardware, not yet "the GPU lands inside the
agent's own sandbox."

```bash
# On dcn-gpu-0, with the test VM from today still running (or re-launched
# the same way as before) and gnwesrealtest already built:

cd ~/ch-test
cat > agent-gpu-loop.sh <<'EOF'
#!/bin/sh
# Minimal poll loop: real actor state -> real GPU attach/detach, no K8s involved.
WRAPPER=/path/to/your/kubectl-ate-docker-wrapper   # from earlier today
SOCKET=/tmp/ch-api.sock
GPU_PATH=/sys/bus/pci/devices/0000:18:00.0/
DEVICE_ID=gnwes-real-gpu-0
ATESPACE=ate-demo-counter
ACTOR=gnwes-actor-1

attached=0
while true; do
  state=$("$WRAPPER" get actor "$ACTOR" --atespace "$ATESPACE" -o json 2>/dev/null \
    | grep -o '"state": *"[^"]*"' | head -1 | cut -d'"' -f4)
  if [ "$state" = "ACTOR_STATE_RUNNING" ] && [ "$attached" = "0" ]; then
    echo "$(date): actor RUNNING, attaching GPU"
    ./gnwes-realtest/gnwesrealtest attach "$SOCKET" "$GPU_PATH" && attached=1
  elif [ "$state" != "ACTOR_STATE_RUNNING" ] && [ "$attached" = "1" ]; then
    echo "$(date): actor $state, detaching GPU"
    ./gnwes-realtest/gnwesrealtest detach "$SOCKET" "$DEVICE_ID" && attached=0
  fi
  sleep 3
done
EOF
chmod +x agent-gpu-loop.sh
./agent-gpu-loop.sh &
```

Then, from `server1`, drive the real actor exactly as before:

```bash
kubectl-ate-docker-wrapper resume actor gnwes-actor-1 --atespace ate-demo-counter
# watch dcn-gpu-0's loop output -- expect "attaching GPU" within ~3s, then
# (optionally) confirm via tmux attach -t chvm -> lspci | grep -i nvidia

kubectl-ate-docker-wrapper suspend actor gnwes-actor-1 --atespace ate-demo-counter
# expect "detaching GPU" within ~3s
```

This is genuinely testable today, with everything already in place.

## Phase 2 — the real GNWES CRDs and controllers, on the real cluster

This is Phase 1's same proof, but through the actual production code path
(`VFIOGPUPool`, `VMGPUAttachment`, `internal/substratebridge`) instead of a
shell-script stand-in — the thing that would actually ship.

1. Finish Phase 0 (get the source or a built binary onto `dcn-gpu-0`).
2. Apply the CRDs to the **real** cluster (they've only ever been applied
   to the local `kind-kind` cluster so far):
   ```bash
   kubectl apply -f config/crd/bases/gnwes.io_vfiogpupools.yaml
   kubectl apply -f config/crd/bases/gnwes.io_vmgpuattachments.yaml
   ```
3. Create a `VFIOGPUPool` naming GPU 0's real path and a `VMGPUAttachment`
   pointing at `/tmp/ch-api.sock` (same shape as
   `config/vmgpu/vfiogpupool-sample.yaml` / `vmgpuattachment-sample.yaml`),
   in a namespace on the real cluster.
4. Run `cmd/manager` **on `dcn-gpu-0`** (not `server1` — it needs local
   filesystem access to the api-socket), pointed at the real cluster's
   kubeconfig.
5. Run the real `cmd/substrate-gpu-bridge` (not the Phase-1 shell script),
   with `-kubectl-ate-bin` set to the Docker wrapper, watching
   `gnwes-actor-1`, driving the real `VMGPUAttachment` object.
6. Repeat the same resume/suspend test as Phase 1, now watching
   `kubectl get vmgpuattachment -o wide` for `status.phase` transitions
   instead of shell output.

## Phase 3 — the GPU lands in the agent's own sandbox (real WorkerPool integration)

This is the actual end state: a Substrate `WorkerPool` running the
**microVM** worker type (`ateworker-microvm`, Kata on Cloud Hypervisor,
confirmed from Substrate's own source in `research/live-attach-poc/README.md`)
instead of gVisor, so the api-socket `VMGPUAttachment` targets is the
*agent's own* sandbox, not a side VM.

1. Install Kata Containers on `dcn-gpu-0`, configured to use `cloud-hypervisor`
   as its VMM (Kata supports multiple VMM backends; this needs Kata's own
   `configuration-clh.toml` selected, not the QEMU default).
2. Confirm Kata can boot a plain microVM sandbox on this node before
   touching Substrate at all (`kata-runtime`/`ctr` or a minimal
   containerd+Kata smoke test) — isolate Kata/CH problems from GNWES/Substrate
   problems.
3. Create a Substrate `ActorTemplate`/`WorkerPool` pair with
   `sandboxClass: <whatever Substrate's microVM class is named>` instead of
   `gvisor-default` (check `config/substrate-integration`'s upstream clone
   for the exact value — not yet confirmed in this repo).
4. Once a worker pod is `Running`, find its *real* Kata-assigned api-socket
   (`/run/vc/vm/<id>/clh-api.sock` per Substrate's own `internal/ch`
   package) — this path is per-worker, dynamic, and not something this repo
   currently discovers automatically.
5. **Needed, not-yet-built enhancement**: either create one
   `VMGPUAttachment` per worker pod with its discovered socket path
   (workable, matches WorkerPool's per-worker pattern, but needs something
   to create/delete these objects as workers come and go — not built), or
   extend `cmd/substrate-gpu-bridge` to resolve a worker's socket path
   itself from the actor's `WorkerAssignment` rather than a static
   `-attachment-name` flag.
6. Repeat the resume/suspend test; this time confirm the GPU appears inside
   the actor's own running sandbox (e.g. via `kubectl-ate` exec into it),
   not a separate VM.

This phase is real additional engineering, not a rerun of what exists —
call it out plainly if pursued, same as the rest of this project's
documentation standard.

## Testing with a Jupyter notebook

JupyterHub is already running on this cluster (`jhub-ns`, confirmed a live
single-user server, `jupyter-vitu1--hghh`). The honest path here is NOT to
reconfigure JupyterHub's spawner to use Kata+CH for every notebook pod (a
bigger platform change, affecting all notebook users) — it's to run the
GPU-needing **kernel** inside a Cloud Hypervisor VM we manage (same pattern
as Phases 1–3), while the notebook UI/frontend stays a normal, fast,
lightweight pod. Jupyter supports exactly this split: a notebook frontend
can connect to a remote kernel over a kernel gateway.

What this needs, not yet built:

- A small `jupyter-gpu-bridge`, the same shape as
  `internal/substratebridge`: poll the Jupyter Server's REST API
  (`GET /api/kernels/<id>`) for `execution_state` (`idle`/`busy`), map
  `busy` → `Attached`, `idle` → `Detached`, drive the same
  `VMGPUAttachment` mechanism. `DesiredStateForActorState`'s pattern in
  `internal/substratebridge/bridge.go` is directly reusable — same
  `ActorStateFetcher`-shaped interface, different concrete fetcher.
- A kernel gateway or remote-kernel setup pointing a notebook's kernel
  process at a process running inside a Cloud Hypervisor VM, rather than a
  local in-pod kernel. `enterprise-gateway` is already deployed on this
  cluster (`enterprise-gateway` namespace) — check whether it already
  supports pointing at an externally-managed kernel process before building
  something new.

Not attempted today; this is a real next step, not a verified one.

## The known, real limitation: checkpoint/restore across worker pools and nodes

Stated plainly, matching what was already found in
`research/live-attach-poc/README.md` and the real Agent Substrate
announcement read early in this project: **a VFIO-passed-through GPU is not
a live-migratable resource.** Cloud Hypervisor's own snapshot/restore
(what Substrate's `internal/ch` package uses for actor suspend/resume) works
for the VM's CPU/memory state, but a passed-through PCI device's state lives
in the physical card itself — there is no generic way to snapshot "what a
real GPU's internal state is" and restore it onto a *different* physical
GPU, let alone one on a different node. Substrate's own announcement says
the same thing: a GPU snapshot restores "only onto the same GPU model, and
sometimes only with the same driver version," with no cross-node story at
all. This is not a GNWES gap to close — it's a hardware/ecosystem
limitation (NVIDIA's own live-migration support for passthrough GPUs is
restricted to datacenter-class vGPU-licensed cards, not consumer TITAN RTX
hardware like this cluster's). If this is ever needed, the realistic path
is checkpointing the *workload* (whatever was running on the GPU) at a
layer above the hardware, not the GPU's own device state — out of scope for
this mechanism entirely.
