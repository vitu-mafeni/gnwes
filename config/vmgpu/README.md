# VMGPUAttachment — real, live GPU hot-attach/detach (not simulated)

This is GPULease's real counterpart: where GPULease simulates the attach
step (`time.Sleep` + a pod annotation) because vanilla Kubernetes DRA cannot
attach a device to a pod after it's already running (see
`config/dra/README.md` and `research/live-attach-poc/README.md`),
`VMGPUAttachment` does the real thing — a genuine `PUT /api/v1/vm.add-device`
/ `vm.remove-device` call against a real **cloud-hypervisor** VMM's REST API,
hot-plugging/unplugging a real VFIO (GPU) device into/out of an
already-running guest, with the guest never recreated.

This targets cloud-hypervisor specifically, not gVisor, because Stage 3's
research (`research/live-attach-poc/README.md`) found gVisor's `nvproxy` has
no mechanism to add a device to an already-running sandbox at all (a static,
creation-time-only device registry, confirmed from source), while
Agent Substrate's microVM worker (`ateworker-microvm`) already runs Kata on
cloud-hypervisor, which has shipped exactly this hotplug/hot-unplug
capability for a while.

**Verified on real hardware (2026-11-11):** this mechanism's actual
`chclient` code hot-attached a real NVIDIA TITAN RTX GPU into an
already-running real Cloud Hypervisor guest, and hot-detached it again,
confirmed from inside the guest both times (`lspci` showed the real device
appear, then disappear, with no guest reboot). See
`TESTING-WITH-REAL-AGENTS.md` for the staged plan to combine this with a
real Agent Substrate actor and a real WorkerPool, and for the honest limits
(checkpoint/restore across nodes is not solved, and isn't a GNWES gap to
close — it's a hardware limitation).

## How this differs from GPUWorkerPool/GPULease, concretely

| | GPUWorkerPool / GPULease | VFIOGPUPool / VMGPUAttachment |
|---|---|---|
| Device inventory | Simulated slots, conjured by `Resize` | Real, physical VFIO devices, a fixed list you declare (`spec.devices`) |
| "Attach" | `time.Sleep(attachLatencyMillis)` + pod annotation | Real `PUT /api/v1/vm.add-device` against a real cloud-hypervisor api-socket |
| What drives attach/detach | Object create/delete (and a TTL) | **Flipping `spec.desiredState`** between `Attached`/`Detached` on the SAME object — this is the part that actually matters: it's what lets one object track a device across many attach/detach cycles as Substrate reuses one worker pod across many actors, instead of needing a new object (and a new pod) each time |
| Backing package | `internal/pool` | `internal/vfiopool` (same Acquire/Release race-safety property, proven the same way — see its own `TestConcurrentAcquireReleaseNeverDoubleBooks`) |
| REST client | n/a | `internal/chclient`, wire format taken directly from cloud-hypervisor's own OpenAPI spec and `docs/hotplug.md`, not guessed |

## The bridge: deciding WHEN to flip `desiredState`

`internal/substratebridge` + `cmd/substrate-gpu-bridge` close the gap this
README used to list under "No bridge to Substrate's actual actor lifecycle"
(see git history if you want the exact old wording). It polls one actor's
state via `kubectl-ate get actor -o json` (Substrate's `Actor` isn't a
Kubernetes CRD — only `WorkerPool` is, confirmed by grepping the real
`agent-substrate/substrate` clone for `kubebuilder:object:root` — so there's
nothing to `kubectl get --watch` instead) and flips one `VMGPUAttachment`'s
`spec.desiredState` to match: `Attached` while `ACTOR_STATE_RUNNING`,
`Detached` for every other state (including the transitional
`RESUMING`/`SUSPENDING` — see `DesiredStateForActorState`'s doc comment for
why that's the conservative choice, not an oversight).

```bash
substrate-gpu-bridge \
  -kubectl-ate-bin kubectl-ate \
  -kubectl-ate-kubeconfig /path/to/substrate-control-plane-kubeconfig.yaml \
  -atespace ate-demo-counter -actor gnwes-actor-1 \
  -attachment-namespace gnwes-vmgpu -attachment-name actor-worker-1-gpu \
  -poll-interval 1s
```

## What's verified, and how

- **`internal/chclient`** — verified against a *real* HTTP server listening
  on a *real* unix socket (not mocked at the transport level): add-device
  (200 w/ `PciDeviceInfo`, 204 cold-add, 409 conflict), remove-device (204,
  404), ping, and the "wait until the socket answers" retry loop. See
  `internal/chclient/client_test.go`.
- **The reconciler logic** (`internal/controller/vmgpuattachment_controller.go`)
  — verified with a fake `CHDevicePlugger` standing in for the real client:
  attach, idempotent re-reconcile, detach-releases-device-back-to-pool,
  repeated attach→detach→re-attach cycles (proving the "one object, many
  cycles" property works), a failed `AddDevice` correctly releasing the
  device rather than leaking it as stuck-Attached, and delete-while-attached
  correctly detaching first. See `internal/controller/vmgpuattachment_controller_test.go`.
- **The whole thing wired together, live, against a real Kubernetes
  cluster** — run end-to-end on this repo's local `kind-kind` cluster: a
  real `cmd/manager` process, real `VFIOGPUPool`/`VMGPUAttachment` objects,
  reconciling for real, making a real HTTP-over-unix-socket call to
  **`cmd/fakevmm`** (a tiny stand-in for cloud-hypervisor's API --- see
  below) standing in for the real VMM. Confirmed: `kubectl patch
  ...desiredState=Attached` produced a real `vm.add-device` call and a
  `status.phase=Attached` with a real (fake-VMM-assigned) guest BDF in under
  10ms; flipping to `Detached` produced a real `vm.remove-device` call and
  released the device back to the pool (confirmed via the pool's own status
  immediately reflecting `availableDevices` again once the `VFIOGPUPool`
  object was re-reconciled).
- **The bridge, end-to-end, live** — same `kind-kind` cluster, same real
  `cmd/manager`/`cmd/fakevmm`, plus **`cmd/fakekubectlate`** (a stand-in for
  `kubectl-ate get actor -o json` driven by a plain text file, so a test can
  flip the "actor's" state without a real Substrate control plane) and the
  real `cmd/substrate-gpu-bridge` binary polling it. Confirmed: writing
  `ACTOR_STATE_RUNNING` to the state file produced a real `desiredState`
  patch within one poll interval, which the real reconciler then turned
  into a real `vm.add-device` call and `status.phase=Attached`; writing
  `ACTOR_STATE_SUSPENDED` reversed both steps. The bridge's own log showed
  exactly two transitions, matching the two writes, with no spurious
  extra patches in between polls where nothing had changed.
- **`internal/substratebridge`'s decision logic itself** — unit-tested with
  fakes for both the actor-state fetcher and the attachment driver: applies
  a change only when the computed desired state differs from the current
  one (no needless API writes once settled), propagates fetch/write errors
  without guessing, and a `Run`-loop test drives a fake actor through
  `SUSPENDED → RUNNING → SUSPENDED` and confirms exactly the two real
  transitions land, in order. See `internal/substratebridge/bridge_test.go`.
- **The real `kubectl-ate` against the real Substrate control plane,
  driving a real `VMGPUAttachment`, live.** Ran the real
  `cmd/substrate-gpu-bridge` binary against the real cross-compiled
  `kubectl-ate-linux` (wrapped in a throwaway container on the `kind`
  docker network, same pattern `config/substrate-integration` already
  established — not `cmd/fakekubectlate` this time), pointed at the actual
  `gnwes-actor-1` actor from that integration's own `ate-demo-counter`
  atespace, still running from an earlier session. Confirmed, with real
  `kubectl-ate resume actor`/`suspend actor` commands, not a simulated
  state file: resuming the real actor produced
  `ACTOR_STATE_RUNNING -> desiredState Attached` in the bridge's log within
  one 3s poll, and the real reconciler reached `status.phase=Attached` with
  a real guest BDF 3ms later; suspending the real actor again produced the
  matching `Detached` transition and release. This is the strongest
  verification available in this environment — the only boundary still not
  crossed is cloud-hypervisor itself (next item).

**Updated 2026-11-13:** the real run above used the bridge's *old*
interface, which watched one actor by a fixed name passed at deploy time.
That assumption was always going to break the moment a worker pod hosted a
second actor after the first (exactly what "a worker pod is reused across
many actors" means) — the bridge now resolves whichever actor is *currently*
assigned to its worker pod instead (`internal/substratebridge`, see
`config/substrate-integration/REPRODUCTION-GUIDE.md`'s "Known limitations"
for the precise gap this closed and what's still unverified about it). The
CLI example right below reflects the new flags; it has not itself been
re-run against real hardware since the change — only the fetch/parse logic
behind it is unit-tested (see `internal/substratebridge/kubectlate_test.go`).

## What's NOT verified

- ~~No real cloud-hypervisor instance, no real VFIO GPU, no real guest~~ —
  **resolved 2026-11-11** on a real bare-metal node (`dcn-gpu-0`, 3x TITAN
  RTX, confirmed to have real `/dev/kvm`, unlike this dev environment).
  Real `chclient` code hot-attached/detached a real GPU into/out of a real
  Cloud Hypervisor guest, confirmed from inside the guest via `lspci` both
  times. See `TESTING-WITH-REAL-AGENTS.md` for the full account, including
  the real obstacles hit (NVIDIA driver live-unbind reproducibly wedging
  while actively managed, fixed by excluding the GPU from `nvidia.ko` at
  boot via an initramfs hook instead of live-unbinding it).
- ~~No real end-to-end hardware run of the microVM worker path has been
  performed in this repo yet~~ — **resolved 2026-11-14.** A real
  `gnwes-counter-exclusive-*` worker pod (`sandboxClasses: [microvm]`)
  reached `2/2 Running` on `dcn-gpu-0`, a real actor resumed onto it, and
  its `VMGPUAttachment` reached `phase: Attached` with a real `deviceID`
  (`gpu-exclusive-0`) and `guestBDF` (`"0000:00:08.0"`) —
  cloud-hypervisor's own `vm.add-device` call succeeding against its real
  nested-VM guest. The remaining blocker when this was last written —
  getting the worker container real VFIO device access without
  `privileged: true` (incompatible with `ateworker-microvm`'s own startup
  check, see below) — turned out to need node-level CRI-O config, not just
  webhook changes:
  1. The node's CRI-O config (`/etc/crio/crio.conf.d/`, **not** automated
     by anything in this repo — a one-time per-node setup step) needs
     `allowed_annotations = ["io.kubernetes.cri-o.Devices"]` and
     `allowed_devices = ["/dev/vfio/vfio", "/dev/vfio/<iommu-group>"]`
     added to whichever runtime handler is active for worker pods (here,
     `runc`, confirmed via `crio config`'s resolved `default_runtime` and
     no pod on the cluster setting a `runtimeClassName`), followed by a
     `systemctl restart crio`.
  2. `podgate-webhook` now sets the `io.kubernetes.cri-o.Devices` pod
     annotation (`cmd/podgate-webhook/main.go`'s
     `vfioDevicesAnnotationValue`) from a new `VFIO_IOMMU_GROUPS` env var
     (comma-separated IOMMU group numbers; this cluster's single TITAN RTX
     is group 8) — in addition to the `/dev/vfio` hostPath mount it already
     added, which makes the device path resolve but, on its own, still
     leaves the kernel's device cgroup blocking `open()` with `EPERM`.
  See `config/substrate-integration/REPRODUCTION-GUIDE.md`'s "Known
  limitations" (Exclusive tier) for the full account, including an earlier
  bug in `internal/chclient`'s `Iommu` field (string vs. the real
  cloud-hypervisor API's boolean) and a missing watch predicate on
  `VMGPUAttachmentReconciler` that both had to be fixed first to even see
  this EPERM clearly.

  **Updated 2026-11-14 — the full attach → detach → re-attach cycle is now
  confirmed end-to-end, including the guest-kernel side:**
  - ~~Still not confirmed either way: whether the guest's NVIDIA driver
    actually re-probes the hot-added PCI device without a reboot~~ —
    resolved. The actor's cloud-hypervisor **console log**
    (`/var/lib/gnwes/vcvm/<vcVMID>/<actorUID>/console.log` on the node)
    shows the guest kernel live-enumerating the device the moment it was
    added: `pci 0000:00:08.0: [10de:1e02] type 00 class 0x030000 PCIe
    Legacy Endpoint`, full BAR assignment, and a `kata-agent` uevent
    (`action: "add"`) timestamped to the millisecond against the
    attachment's own `lastTransitionAt`. This confirms real, live
    kernel-level PCI hotplug enumeration — the prerequisite any driver
    binds to — not just cloud-hypervisor's own API call succeeding. (This
    test guest has no NVIDIA driver installed, so `nvidia.ko` itself
    binding to the device specifically is still unconfirmed — a separate,
    narrower claim than kernel-level enumeration.)
  - Detach found and fixed two real bugs in
    `VMGPUAttachmentReconciler.reconcileDetach`: Agent Substrate's actor
    `suspend` fully tears down the microVM's whole working directory (not
    just pauses it), so a live `vm.remove-device` call can never succeed
    once that happens, and the device was staying stuck "held" in the pool
    forever as a result (`VFIOGPUPool.status.availableDevices` stuck at
    `0`) — see `vmGone`'s doc comment in
    `internal/controller/vmgpuattachment_controller.go`. A second, related
    bug meant an attachment already sitting in `Phase: Failed` from an
    earlier failed detach attempt was *never retried on any later
    reconcile* (the guard checked `Phase != Attached`, true for `Failed`
    too) — fixed by keying the guard on `DeviceID` instead, the real source
    of truth for "is a device currently held."
  - **`nvidia-smi` on the host will *never* show this GPU, by design, and
    that's not related to attach/detach state at all** — it's permanently
    excluded from `nvidia.ko` via the initramfs hook (see
    `TESTING-WITH-REAL-AGENTS.md`'s step 4), handed to `vfio-pci`
    unconditionally at boot. The correct way to confirm a device is really
    "back" after detach: `kubectl get vfiogpupool -o yaml`'s
    `status.availableDevices`, and `readlink -f
    /sys/bus/pci/devices/<BDF>/driver` on the host staying `vfio-pci`.
  - **Two real operational requirements for `cmd/manager` on the GPU node,
    neither obvious, both now documented in `scripts/start-manager.sh`:**
    it must run as **root** (`ateworker-microvm` creates each actor's VM
    directory `root:root`, mode `0700` — a non-root manager process gets a
    distinct "permission denied" dialing the socket, not the `vmGone`
    path's ENOENT), and its `KUBECONFIG` must point at a kubeconfig whose
    CA matches the cluster's *current* CA (`sudo`'s `HOME` is `/root`, not
    the invoking user's — it will NOT pick up `~ubuntu/.kube/config` on its
    own, and a stale CA produces "x509: certificate signed by unknown
    authority").
  - **VFIO device passthrough needs a raised `memlock` ulimit** —
    confirmed live via `vfio_pin_pages_remote: RLIMIT_MEMLOCK (8388608)
    exceeded` in the host's `dmesg`: pinning a VM's entire guest memory for
    DMA (required on `vm.restore`, i.e. every `kubectl-ate resume`) needs
    far more than the Linux default 8 MiB. CRI-O applies ulimits at
    container-create time, so an already-running worker pod needs
    recreating to pick this up, same as the annotation fix.

  All three CRI-O-side fixes above (`allowed_annotations`,
  `allowed_devices`, `default_ulimits`) are one committed drop-in file,
  not just prose: **`crio-vfio-devices.conf`** in this directory. Install
  it at `/etc/crio/crio.conf.d/99999-vfio-annotations.conf` on each
  Exclusive-tier GPU node and `sudo systemctl restart crio`.
  - **Two more real bugs found via a repeated attach/detach cycle
    benchmark (`scripts/bench-attach-cycles.sh`), both fixed in
    `internal/chclient/client.go` (2026-11-14):** a successful
    `vm.remove-device` response doesn't reliably mean cloud-hypervisor has
    actually finished removing the device internally (`RemoveDevice` now
    polls `vm.info` and only returns once the device is confirmed gone
    from `config.devices`); and a freshly-resumed VM can report
    `"Duplicated device path"` on `vm.add-device` because Agent
    Substrate's restore-from-checkpoint path boots it with the GPU
    already baked into its config (`AddDevice` now recognizes this via
    `vm.info` and recovers the real BDF instead of failing). A 5-cycle
    suspend/resume run went from wedging after the first cycle to 5/5
    clean after both fixes.
  - **Checked 2026-11-15: an actor's own code had no way to see its GPU
    pool, or how much capacity was available, at all — fixed.**
    `podgate-webhook` now injects `GNWES_GPU_POOL`/`GNWES_GPU_BACKEND`
    (Downward API, from the pod's own labels) into the `worker`
    container of every GNWES-gated pod, Shared and Exclusive tier alike,
    and `config/webhook/bridge-rbac.yaml`'s Role now also grants
    read-only `get`/`list`/`watch` on `gpuworkerpools`, `gpuleases`, and
    `vfiogpupools` to the worker pod's shared ServiceAccount — confirmed
    live via `kubectl auth can-i ... --as=system:serviceaccount:...`, all
    `yes`. An actor's own code can now use its own mounted ServiceAccount
    token (the standard Kubernetes self-discovery pattern, no new API) to
    `GET` the pool named by `GNWES_GPU_POOL` and read real total/available
    counts off its `.status`. **Not yet confirmed from *inside* a real
    guest** — same access limitation as the NVIDIA-driver item above (no
    shell, no exec); this was verified from outside via RBAC checks and
    the deployed pod's resolved env vars, not by an actor actually calling
    the Kubernetes API itself.
  - **Checked 2026-11-15: the Shared tier gives out a whole physical GPU
    per claim, not a HAMi slice.** `config/dra/README.md` confirms
    `nvidia-smi` inside the real-hardware test pod showed exactly the one
    GPU claimed; no HAMi slicing library is deployed anywhere in this
    project's config. `GPUWorkerPoolSpec.GPUResourceName` can in
    principle point at a HAMi virtual-slice resource key, but nothing
    here installs or configures HAMi — so today both tiers give out
    whole, exclusive GPUs, differing in *when* (pod creation only vs. any
    time) and *how* (Kubernetes scheduling vs. live VFIO hot-attach), not
    in sharing. Real GPU slicing is a real next step, not something
    built yet. Relatedly: no real concurrent multi-actor GPU workload has
    ever been run on either tier — the one real-hardware DRA+CDI test was
    a single pod, once, and `cmd/loadtest`'s pods use a plain `pause`
    image with no GPU resource request at all, so it only exercises
    GNWES's own admission bookkeeping, never real device binding under
    concurrency.
  **What IS now done (2026-11-13):** `ateworker-microvm` turned out to
  already exist and ship upstream in agent-substrate (Kata on
  cloud-hypervisor, confirmed via `pkg/sandboxclass.All()` and
  `docs/reference/sandbox-classes.md` in the real repo) — it was never
  actually missing infrastructure, just never exercised by this
  integration. Three things changed to actually exercise it:
  1. `GPUWorkerPoolReconciler.reconcileExclusive`
     (`internal/controller/gpuworkerpool_controller.go`) creates one
     `VMGPUAttachment` per real Exclusive-tier worker pod immediately
     (`Detached`, empty `apiSocket`) — it no longer waits on a static
     `gnwes.io/ch-api-socket` pod annotation, which was always going to be
     wrong anyway: cloud-hypervisor's api-socket path is keyed by actor UID
     (`/run/vc/vm/<actor-uid>/clh-api.sock`), not by pod, since a worker pod
     reuses its VM lifecycle across many sequential actors.
  2. `cmd/substrate-gpu-bridge` (`internal/substratebridge`) now resolves
     whichever actor is *currently* assigned to its worker pod (there is no
     fixed actor to watch) and writes `apiSocket` and `desiredState`
     together from that actor's real UID, instead of the old fixed
     `-actor` flag that would have paired `Attached` with a stale socket
     path the moment a second actor ever reused the pod.
  3. `podgate-webhook` now injects the bridge as a sidecar (plus the shared
     `/run/vc/vm` emptyDir volume it needs) into every pod labeled
     `gnwes.io/gpu-backend: Exclusive`, reading `-atespace` off a
     `gnwes.io/atespace` pod label and the pod's own name via the Downward
     API — the one deployment step nothing had automated before.

  See `config/substrate-integration/REPRODUCTION-GUIDE.md`'s Phase 4 for
  the actual manifests and apply order. The ENTIRE chain from "a real
  worker pod exists" to "a real GPU gets hot-attached when its actor
  resumes" is now confirmed real, wired, and run end-to-end on real
  hardware (2026-11-14, see above) — not just unit-tested.
- **Whether attaching earlier (during `ACTOR_STATE_RESUMING`, rather than
  waiting for fully `RUNNING`) would be worth doing for latency** is
  unexplored — `DesiredStateForActorState`'s conservative mapping was a
  deliberate choice given no real cloud-hypervisor to test the transitional
  case against, not a settled answer.

## Testing without real hardware

`cmd/fakevmm` is a small stand-in VMM server (not part of the production
path) that speaks exactly the subset of cloud-hypervisor's REST API this
mechanism needs, over a real unix socket, so the whole reconcile loop above
the VMM boundary can be exercised on a machine with no KVM/cloud-hypervisor
available:

```bash
go build -o fakevmm ./cmd/fakevmm
./fakevmm -socket ./fvmm.sock &      # keep this path SHORT -- AF_UNIX has a
                                      # ~100-byte path limit; a long temp-dir
                                      # path fails with "bind: invalid argument"

kubectl apply -f config/crd/bases/gnwes.io_vfiogpupools.yaml
kubectl apply -f config/crd/bases/gnwes.io_vmgpuattachments.yaml
kubectl create namespace gnwes-vmgpu
# edit config/vmgpu/vmgpuattachment-sample.yaml's apiSocket to the absolute
# path of fvmm.sock, then:
kubectl apply -f config/vmgpu/vfiogpupool-sample.yaml
kubectl apply -f config/vmgpu/vmgpuattachment-sample.yaml

go run ./cmd/manager   # against whatever context your cluster is on

kubectl get vmgpuattachment -n gnwes-vmgpu -o wide   # watch Phase -> Attached
kubectl patch vmgpuattachment actor-worker-1-gpu -n gnwes-vmgpu \
  --type=merge -p '{"spec":{"desiredState":"Detached"}}'
kubectl get vmgpuattachment -n gnwes-vmgpu -o wide   # watch Phase -> Detached
```

**Pointing this at a real cloud-hypervisor instance instead** needs nothing
different code-wise: set `apiSocket` to that VMM's real api-socket path
(e.g. Kata's `/run/vc/vm/<id>/clh-api.sock`) instead of `fakevmm`'s. That
path has simply not been exercised yet in this repo (see "What's NOT
verified" above).

**Testing the bridge too, without a real Substrate control plane:** add
`cmd/fakekubectlate` as a stand-in for `kubectl-ate` on top of the setup
above (same cluster, same `fakevmm`, `VMGPUAttachment.spec.desiredState`
can start at either value since the bridge will correct it within one poll):

```bash
go build -o fakekubectlate ./cmd/fakekubectlate
go build -o substrate-gpu-bridge ./cmd/substrate-gpu-bridge

echo -n "ACTOR_STATE_SUSPENDED" > actor-state.txt
# FAKE_WORKER_POD must match -worker-pod below -- fakekubectlate reports its
# one canned actor as assigned to exactly this worker pod name, the same way
# ParseActorForWorkerPod filters the real kubectl-ate's actor list.
FAKE_ACTOR_STATE_FILE="$(pwd)/actor-state.txt" FAKE_WORKER_POD=actor-worker-1-gpu ./substrate-gpu-bridge \
  -kubectl-ate-bin ./fakekubectlate \
  -atespace fake-atespace -worker-pod actor-worker-1-gpu \
  -attachment-namespace gnwes-vmgpu \
  -poll-interval 1s &

kubectl get vmgpuattachment -n gnwes-vmgpu -o wide   # Detached

echo -n "ACTOR_STATE_RUNNING" > actor-state.txt
sleep 2
kubectl get vmgpuattachment -n gnwes-vmgpu -o wide   # -> Attached, for real

echo -n "ACTOR_STATE_SUSPENDED" > actor-state.txt
sleep 2
kubectl get vmgpuattachment -n gnwes-vmgpu -o wide   # -> Detached again
```

**Cleanup order matters here too:** stop the bridge first (it'll otherwise
immediately re-attach whatever `cmd/manager` just detached on the next
poll), then make sure the attachment is `Detached` before stopping
`cmd/manager` — see Troubleshooting below.

## Driving this from a real Substrate actor instead of a fake one

With `config/substrate-integration`'s real control plane already up (its own
README covers standing it up) and `cmd/substrate-gpu-bridge` pointed at a
real `kubectl-ate`, this is the same picture above but for real. The one
wrinkle: `kubectl-ate` doesn't build or run natively on this Windows dev box
(`internal/imagecache` uses `syscall.Stat_t`), so `-kubectl-ate-bin` needs to
point at something that runs the real cross-compiled `kubectl-ate-linux`
inside a container on the `kind` docker network and forwards its own
arguments straight through — the same pattern `config/substrate-integration/README.md`
already uses for `resume actor`/`suspend actor` by hand, just wrapped so
`cmd/substrate-gpu-bridge`'s `exec.Command` can call it directly as one
binary. A ~20-line Go program doing exactly that (build `docker run --rm
--network kind -v <bin dir>:/bin2 -w /bin2 debian:bookworm-slim
./kubectl-ate-linux --kubeconfig kubeconfig-internal.yaml <args...>` and run
it) is enough; it's not checked into this repo since the mount path is
specific to wherever your `agent-substrate` checkout's `bin/` lives.

```bash
# attachment-name is implicit now: it's always the worker pod's own name
# (-worker-pod), since GPUWorkerPoolReconciler names every VMGPUAttachment
# after the pod it belongs to.
substrate-gpu-bridge \
  -kubectl-ate-bin /path/to/your/kubectl-ate-docker-wrapper \
  -atespace ate-demo-counter -worker-pod actor-worker-1 \
  -attachment-namespace gnwes-vmgpu \
  -poll-interval 3s &

# from another terminal, using the real kubectl-ate the same wrapper calls:
kubectl-ate-docker-wrapper resume actor gnwes-actor-1 --atespace ate-demo-counter
kubectl get vmgpuattachment -n gnwes-vmgpu -o wide   # -> Attached, driven by a REAL actor resume

kubectl-ate-docker-wrapper suspend actor gnwes-actor-1 --atespace ate-demo-counter
kubectl get vmgpuattachment -n gnwes-vmgpu -o wide   # -> Detached again
```

Confirmed exactly this sequence against this repo's own `gnwes-actor-1` —
see "What's verified" above.

## Troubleshooting

- **A namespace containing a `VMGPUAttachment` hangs `Terminating`
  forever** — this happens if `cmd/manager` is stopped before the deletion
  is processed; the finalizer (`gnwes.io/vmgpu-attachment-cleanup`) never
  gets removed. Hit this during the smoke test above. Fix: if the object's
  `status.phase` was already `Detached` (nothing left to clean up), it's
  safe to strip the finalizer directly:
  `kubectl patch vmgpuattachment <name> -n <ns> --type=json -p '[{"op":"remove","path":"/metadata/finalizers"}]'`.
  If it was still `Attached`, restart `cmd/manager` first instead, so the
  real detach actually runs before the finalizer is removed -- stripping it
  directly in that case would leak the device as stuck-Attached in the
  pool's bookkeeping with nothing left to release it.
