# VMGPUAttachment — real, live GPU hot-attach/detach (not simulated)

## In plain English (updated 2026-11-15)

This machine has **3 physical GPUs**. They're split into two groups that
work completely differently, on purpose:

- **2 of them (`af:00.0`, `d8:00.0`) are "shared."** They stay on the
  GPU's normal driver the whole time, which is exactly why the host's
  own `nvidia-smi` lists them. Several small jobs can use one of these
  GPUs at the same time, each getting a slice of it (a fraction of its
  memory and compute power) — like splitting a pizza, except the
  "knife" is a software layer that loads into every job automatically
  and enforces the limit (not a hardware boundary — a real break-out
  test against it hasn't been done yet, see Limitations). This is the
  **Shared tier**, built on this cluster's real Volcano scheduler.
  Confirmed live: 3 jobs ran at once, 2 of them genuinely sharing the
  same physical card, each one able to see and prove it only has its own
  small slice (`nvidia-smi` run *inside* one shows `0MiB / 768MiB`, not
  the full 24576 MiB). A job's slice is handed out once, when it starts
  — there's no "moving" a slice later. **A slice does get handed to a
  *different* job later, though** — confirmed live (2026-11-15): when a
  job is paused (suspended), it gives up its spot entirely (`WORKER POD`
  shows `<none>`), and that same spot — including its slice — was then
  claimed by a genuinely new, just-created job, which got the exact same
  slice size and the correct enforced limit. This is the same mechanism
  already used for ordinary CPU/memory — nothing GPU-specific had to be
  built for it.
- **1 of them (`18:00.*`) is "exclusive."** It's handed entirely to one
  virtual machine at a time — no sharing, no slicing, the VM gets the
  real whole card with real hardware-level isolation, the same as if it
  were plugged directly into that one machine and no other. This is the
  **Exclusive tier**. The special part: that GPU can be unplugged from
  one VM and plugged into a *different* VM while that second VM is
  already running, without rebooting anything — confirmed working,
  repeatedly (5 out of 5 cycles in the latest test). A job here never
  gets just a slice — it's the whole card or nothing.
- **These two groups never overlap.** A job is either Shared (gets a
  slice of one of the 2 shared cards, software-enforced) or Exclusive
  (gets the one dedicated card, whole, hardware-isolated) — never a mix,
  and the 3 physical GPUs are permanently split between the two groups,
  not reassigned between them.
- **This is exactly why that 3rd GPU never shows up when you type
  `nvidia-smi` on the host machine itself, and that's not a bug.**
  Giving a virtual machine true, direct hardware access (the technique
  is called VFIO passthrough) requires the host's own GPU driver to
  never touch that card at all — it's handed over at boot time to a
  different, lower-level mechanism instead. So the "missing" GPU
  *is* the one being used — it's just invisible to the host on
  purpose, the whole time, regardless of whether a VM is currently
  using it or not. Confirmed directly on real hardware: that GPU's PCI
  driver stays the same (`vfio-pci`) before and after a VM lets go of
  it; only `kubectl get vmgpuattachment` and `kubectl get vfiogpupool`
  can tell you whether it's currently in use.
- **GNWES did not invent the Shared tier's slicing — be precise about
  that.** Volcano's scheduler and its own `volcano-vgpu-device-plugin`
  (HAMi-lineage code) already do the real slicing and enforcement on
  this cluster, with or without GNWES. What GNWES actually adds: Agent
  Substrate's `WorkerPool` API has no field at all for requesting a
  vGPU slice (no way to set the scheduler name, the runtime class, the
  three resource keys, or the env vars the hijack library needs to not
  break the driver mount) — `podgate-webhook`'s `volcanoVGPUPatches`
  turns two simple pod labels into that whole correct request, Agent
  Substrate's own actor-reuse-one-pod lifecycle stays correctly tracked
  against it, and an operator picks this tier with the same one field
  (`GPUWorkerPoolSpec.Backend`) used for the completely unrelated
  Exclusive/VFIO mechanism. That's integration onto an existing
  capability, not a new slicing mechanism.
- **Is this reproducible?** The code for both mechanisms, the real bug
  fixes found along the way, and the exact commands to repeat every
  test are all committed to this repo (see `REPRODUCTION-GUIDE.md`) —
  nothing was hand-patched outside of version control. A handful of
  one-time, per-machine setup steps (labeling the GPU node, installing
  a CRI-O config file, loading kernel modules) are still manual, not
  automated yet — documented honestly as that, not glossed over.

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

    **Built and uploaded 2026-11-15 — a custom, driver-included Kata
    kernel+rootfs now exist and are staged on this cluster; actor-level
    testing is the next step.** `config/vmgpu/build-nvidia-kata-image.sh`
    is the actual, runnable build script (not just prose) capturing every
    step worked out live on `dcn-gpu-0`:
    - **Considered and rejected: `kata-deploy`'s official NVIDIA GPU
      shims** (`qemu-nvidia-gpu` and friends, installed via
      `helm install ... -f try-kata-nvidia-gpu.values.yaml`). Real,
      NVIDIA-maintained, driver/rootfs fully handled — but QEMU-only,
      and they assign the GPU statically at VM creation, the same
      bind-once limitation every other mechanism already has. That's a
      different problem than this tier exists to solve (cloud-hypervisor
      + live hot-attach into an already-running VM), so it would mean
      giving up this project's actual differentiator, not shortcutting
      the work.
    - **Kernel**: Kata's own kernel-builder has a built-in `-g nvidia`
      flag (`tools/packaging/kernel/build-kernel.sh`) that merges the
      right config fragment (`CONFIG_MODULES=y`, `CONFIG_FW_LOADER=y`,
      plus a few driver-required memory/MTRR configs — no
      `MODULE_SIG_FORCE`, so an unsigned custom module still loads) *and*
      automatically fetches NVIDIA's open-source kernel module source
      (`NVIDIA/open-gpu-kernel-modules`) to build against it. That
      source's pinned version (`versions.yaml`'s
      `externals.nvidia.driver.version`) defaults to whatever upstream
      last set — confirmed live this was `615.71.09`, newer than any
      driver this host has. **NVIDIA kernel modules and userspace
      libraries must be the exact same version** (a private, versioned
      ioctl interface) — pin it to this host's own real driver version
      first (`580.178.04` — confirmed as a real tag in that repo) or the
      module builds cleanly and then fails at init. `-t cloud-hypervisor`
      is a first-class supported hypervisor target for the same builder
      (confirmed from the script's own validation logic) — this was
      never QEMU-specific.
    - **Rootfs**: NOT a from-scratch build, and NOT NVIDIA's own
      `tools/osbuilder/rootfs-builder/nvidia` variant (that one is real
      and maintained, but pulls a whole separate driver install via
      NVIDIA's own apt repos and the `NVRC` custom init, built for the
      static-assignment QEMU shims above) — simpler and more reliable
      given this host already has the exact right driver installed:
      took the *stock* rootfs this cluster already deploys (same
      sha256 as the live `microvm` SandboxConfig) and added only: `kmod`
      + `libkmod` (genuinely absent from the stock image — confirmed via
      `ldd`/`find`, matching why `nvidia-smi` needed the same fix on the
      Shared tier), fetched for the *guest's* exact Ubuntu release
      (26.04 "resolute"), not the host's (22.04) — an ABI mismatch risk
      worth being deliberate about; the 4 real `.ko` files this build
      produced (`nvidia`, `nvidia-uvm`, `nvidia-modeset`, `nvidia-drm` —
      `nvidia-peermem` is GPUDirect-RDMA-only, skipped); and one
      `/etc/modules-load.d/nvidia.conf`.
    - **Real finding that corrects an earlier wrong assumption made
      while building this same rootfs**: it does have
      `/usr/sbin/init -> systemd` and a real, enabled
      `systemd-modules-load.service`, but **this real deployment's
      kernel never runs systemd at all.** Its boot parameters set
      `init=/usr/bin/kata-agent` directly, and the console log proves
      it: `[ 0.205443] check access for rdinit=/init failed: -2,
      ignoring` immediately followed by `[ 0.208384] Run
      /usr/bin/kata-agent as init process` — the kernel execs
      `kata-agent` as PID 1 itself, so `systemd-modules-load.service`
      (and every other systemd unit) never starts, never mind runs.
      `/etc/modules-load.d/nvidia.conf` alone is therefore inert here.
      This was only discovered by forcing a genuine cold boot (a
      brand-new actor, not a restored one — a restored actor's console
      log starts too late, mid-boot, to show this) and reading the
      full `[0.xxx]`-timestamped segment from power-on; it showed zero
      NVIDIA-related output, which is what first exposed that the
      original fix (built around `systemd-modules-load.service`) never
      actually ran.
    - **The fix**: the same init-wrapper technique NVIDIA's own NVRC
      project uses for exactly this situation. On the mounted rootfs:
      rename the real init binary aside
      (`/usr/bin/kata-agent -> /usr/bin/kata-agent.real`) and install a
      tiny shell script at the exact path the kernel execs as init
      (`/usr/bin/kata-agent`) that `modprobe`s the four NVIDIA modules
      and then `exec`s the real binary:
      ```sh
      #!/bin/sh
      for m in nvidia nvidia-uvm nvidia-modeset nvidia-drm; do
        /usr/sbin/modprobe "$m" 2>/dev/null
      done
      exec /usr/bin/kata-agent.real "$@"
      ```
      `exec` replaces the process image in place, so PID 1 is preserved
      across the handoff. This step is captured in
      `build-nvidia-kata-image.sh`'s `install_init_wrapper` step so a
      fresh build includes it from the start, not as a manual
      post-build patch. New rootfs sha256 after adding the wrapper:
      `39830d309a72981c4f3d1868dfa8b5c6b89e46f6fb906b95fecb1b398ee17254`
      (kernel is unchanged).
    - **A real mistake made and fixed while building this, left in
      because it's a genuine footgun**: the kernel build's own
      `lib/modules/<release>/` output directory contains a `build`
      symlink back to the full kernel source tree (standard
      `modules_install` convention, for building further out-of-tree
      modules later) — a careless recursive directory merge follows that
      symlink and pulls in the *entire* kernel source (100+MB) into the
      rootfs, nearly filling the image. Copy only `kernel/` and
      `modules.*` explicitly, never the whole directory.
    - Both custom assets are uploaded to this cluster's real asset store
      (`s3://ate-snapshots/microvm-assets/amd64/`, the same rustfs bucket
      the stock assets already use) and a **second, independent**
      `SandboxConfig` (`sandboxconfig-microvm-nvidia-gpu.yaml`) is applied
      — the stock `microvm` config, and every other WorkerPool still
      using it, is completely untouched.
      Real sha256es: kernel `c3b5598732fc3b7ae79b55588e7a475134f7c422272add4bc91f8c1967f3559a`,
      rootfs `39830d309a72981c4f3d1868dfa8b5c6b89e46f6fb906b95fecb1b398ee17254`
      (updated after the init-wrapper fix below; the original
      pre-wrapper upload was `78f5f498927489659ec7e6b22242f2be3d8b45ce148ebccf772838e6f649080c`).
    - **Tested live, same day — real hot-attach confirmed working end to
      end against the custom image; `nvidia.ko` binding itself is NOT
      conclusively confirmed, and that's an honest gap, not an
      oversight.** Pointed `gnwes-counter-exclusive`'s
      `sandboxClasses[0].configRef` at the new `SandboxConfig`, deleted
      its pod to force a real (not reused) recreation, resumed the
      actor. Two more real, previously-latent bugs surfaced and got
      fixed along the way (see below); once fixed, the attachment
      reached `PHASE: Attached`, `DEVICE: gpu-exclusive-0`,
      `GUESTBDF: 0000:00:07.0`, `ATTACHMS: 485` — a real
      `vm.add-device` success against the custom-kernel VM.
      `/var/lib/ate/actors/<uid>/sandbox-assets.json` on the node
      directly confirms this VM was staged with our exact custom
      `kata-kernel`/`kata-image` sha256 values, not the stock ones — the
      custom build genuinely was used, not silently bypassed.
      What's NOT confirmed: whether `nvidia.ko` actually bound inside the
      guest. The console log turned out to be `kata-agent`'s own
      structured JSON log stream (every line carries `"source":"agent"`
      — its own systemd unit sets `StandardOutput=tty` onto the shared
      console), not a raw kernel/systemd boot log; it only starts once
      `kata-agent.service` itself starts, well after
      `systemd-modules-load.service` (where module loading happens)
      would have run and printed anything. There is still no exec
      access to the guest OS itself to check `lsmod` directly (only to
      the demo app's own driver-less, shell-less container on top, the
      same limitation as always) — so this specific question stays
      open, honestly, rather than inferred from indirect evidence.
    - **Two more real bugs found and fixed getting here, both
      previously-latent, never exercised until a worker pod was
      actually recreated from scratch with a *different* SandboxConfig
      pointing at it:**
      1. `cmd/substrate-gpu-bridge`'s `HostAPISocketPath` had hardcoded
         `clh-api-restore.sock` as the one true socket name, based on
         2026-11-13 testing that concluded Substrate "always restores" —
         true only because every resume tested until then reused a
         worker pod with its own existing local checkpoint. A pod
         recreated from scratch can cold-`run` instead (`clh-api.sock`),
         even for an actor with prior checkpoint history elsewhere —
         `vm.add-device` then failed with "no such file or directory"
         (dialing a socket that was never created) — see
         `internal/substratebridge/bridge.go`'s own doc comment.
      2. The real fix for #1 — checking which socket name actually
         exists — belongs in `cmd/manager`
         (`internal/controller/vmgpuattachment_controller.go`'s new
         `resolveRealAPISocket`), **not** in the bridge sidecar: an
         earlier attempt put the check inside `HostAPISocketPath`
         itself, which runs inside the bridge's own container — one
         that was deliberately given no hostPath mount into
         `GNWESHostVCVMDir` at all (by original design, it only ever
         needed to write a path *string*, never open the socket). That
         check could only ever see ENOENT regardless of what actually
         existed on the host, so the attachment stayed permanently
         `Detached` — a real regression, caught before it shipped
         anywhere but this live test.
      Both are genuinely new findings this cluster's real hardware
      surfaced, not something a stock Kata/Substrate install would hit
      on its own — see `internal/substratebridge/bridge_test.go` and
      `internal/controller/vmgpuattachment_controller_test.go` for the
      regression tests.
    - **A third, separate issue — first suspected to be a cold
      informer-cache timing bug, then root-caused to something much
      simpler and already-seen in this project**: after a worker pod
      recreation, its new `VMGPUAttachment` was never created
      automatically at all — not just slowly. Grepping the full
      `manager.log` window confirmed zero automatic `gpuworkerpool`
      reconciles ever followed a pod create/delete, on two separate
      occasions hours apart (so "cold cache after restart" was ruled
      out — the second occurrence had no restart in between). A
      manual `kubectl annotate gpuworkerpool <name> nudge=... --overwrite`
      always made it fire immediately, proving the reconcile logic
      itself was correct once triggered — only the trigger was
      missing.
      **Real cause**: `dcn-gpu-0`'s own checkout of this repository is
      a plain `scp`'d tree, not a git clone (`git log` there fails
      with "not a git repository"), and its
      `internal/controller/gpuworkerpool_controller.go` was dated
      2026-11-12 — one day *before* the `mapPodToWorkerPool`/`Watches`
      fix was committed upstream on 2026-11-13. `strings` on the
      actual running `bin/manager` binary confirmed it directly: zero
      matches for `mapPodToWorkerPool` or `EnqueueRequestsFromMapFunc`.
      The manager's own startup log confirmed it a second way: only
      one `Starting EventSource` line for the `gpuworkerpool`
      controller (`*v1alpha1.GPUWorkerPool` itself), never a second one
      for `*v1.Pod` — unlike the separate `podgate` controller, which
      does watch Pods and logs its own `*v1.Pod` EventSource. The
      Pod-watch feature this doc already described as "added
      2026-11-13" was never actually running on real hardware at all.
      **Same lesson as the `scripts/start-manager.sh` gap below, hit a
      second time**: a commit landing in this git history is necessary
      but not sufficient — it has to actually be `scp`'d to, and the
      binary rebuilt on, every real machine that runs it. Fix applied:
      re-sync the full source tree to both `server1` and `dcn-gpu-0`
      and rebuild `cmd/manager` there (see `REPRODUCTION-GUIDE.md`'s
      sync step); verify by checking for a second `Starting
      EventSource` line for `*v1.Pod` under `"controller":
      "gpuworkerpool"` in the fresh `manager.log`, and by recreating a
      worker pod and confirming its `VMGPUAttachment` appears
      automatically with no manual annotate.
    - **Harbor registry push timeouts, hit again rebuilding
      `substrate-gpu-bridge` three times for the fixes above**: the
      same two-largest-layers timeout as 2026-11-13, same real fix
      (`docker save` + `skopeo copy docker-archive:...`, see
      `REPRODUCTION-GUIDE.md`'s Phase 4) — `skopeo copy
      docker-daemon:...` looked like a shortcut but fails on an
      unrelated Docker API version mismatch in its own vendored client
      on this cluster, confirmed live; the archive step is required.
    - **Operational note, confirmed the hard way**: `scripts/start-
      manager.sh` existing in this git history does not mean it exists
      on any real node — it had never actually been `scp`'d to either
      `server1` or `dcn-gpu-0`, despite being referenced as already
      "committed" earlier. Committing a file and *syncing* it to the
      machines that actually run it are two different steps in this
      project's workflow; this doc and `REPRODUCTION-GUIDE.md` are the
      source of truth for what a fresh machine needs, not an assumption
      that `git log` implies deployment.
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
    in sharing. **Superseded 2026-11-15 for the Shared tier — see below:**
    real GPU slicing on the Shared tier is now built and confirmed live,
    via this project's real cluster stack (Volcano + its own
    `volcano-vgpu-device-plugin`), not HAMi. Real concurrent multi-actor
    sharing has also now been run and confirmed on real hardware (see
    below) — superseding the "no real concurrent multi-actor GPU workload
    has ever been run" claim this bullet originally made. The Exclusive
    tier is unaffected by any of this: VFIO hot-attach still gives out a
    whole physical GPU per claim, which is correct for that tier (it
    exists specifically for workloads that need a whole device).
  - **Built and confirmed live 2026-11-15: real Volcano vGPU slicing for
    the Shared tier**, matching this project's actual real-cluster
    GitOps config (GPU Operator + `volcano-vgpu-device-plugin` HelmRelease
    + Volcano scheduler HelmRelease + the `nvidia-legacy` CRI-O runtime
    handler DaemonSet — NOT generic upstream Project-HAMi, which an
    earlier version of this doc incorrectly assumed before the real
    configs were provided). `podgate-webhook`'s `volcanoVGPUPatches` (see
    `cmd/podgate-webhook/main.go`) turns two new opt-in pod labels,
    `gnwes.io/vgpu-memory-units` and `gnwes.io/vgpu-cores`, into
    `schedulerName: volcano`, `runtimeClassName: nvidia-legacy`, and the
    three `volcano.sh/vgpu-number`/`-memory`/`-cores` container resource
    keys (preserving any existing `cpu`/`memory` limits/requests — see the
    function's own doc comment on why this and `workerEnvPatches` must
    each do their JSON-patch "does this path already exist" check in one
    shared pass, not two independent ones, or the second function's patch
    can clobber the first's). `vgpuDisableEnvVars` also injects
    `NVIDIA_GDS`/`NVIDIA_GDRCOPY`/`NVIDIA_MOFED=disabled`, required by the
    real plugin's NVML-hijack library or it breaks the driver mount —
    taken directly from `ui-ml-platform-postgres`'s own
    `api/gpu/gpu_backend.py` (`container_env()`), the real platform this
    project's cluster is modeled on.

    **Verified end-to-end on real hardware (2026-11-15):** a new demo
    (`config/substrate-integration/gnwes-counter-vgpu-{pool.yaml,
    workerpool.yaml.tmpl,template.yaml.tmpl}`) requests a small slice (12
    `vgpu-memory` units = 768 MiB, 30 `vgpu-cores` = 30% compute) across 3
    replicas, deliberately more than 1 so real sharing — not sequential
    reuse — gets exercised. All 3 actors (`gnwes-actor-vgpu-1/2/3`)
    reached `ACTOR_STATE_RUNNING`, each on its own worker pod on
    `dcn-gpu-0`, each pod showing `schedulerName: volcano`,
    `runtimeClassName: nvidia-legacy`, the correct combined resources and
    env vars, and the node's real `kubectl describe node` "Allocated
    resources" summing all 3 pods' requests exactly
    (`vgpu-number: 3`, `vgpu-memory: 36`, `vgpu-cores: 90`). The clinching
    proof of *real* physical-GPU co-residency, not just scheduler-level
    accounting: two of the three pods' `volcano.sh/vgpu-ids-new`
    annotations (stamped by the device plugin itself) named the exact
    same physical GPU UUID (`GPU-80e6777e-9d74-f660-8247-d0ffc309f3c0`),
    while the third named the node's other physical GPU
    (`GPU-ed7db115-...`) — two independent actors genuinely sharing one
    physical TITAN RTX, each with its own slice, confirmed by the device
    plugin's own bookkeeping, not inferred.

    **Corrected 2026-11-15, same day:** `kubectl exec ... nvidia-smi`
    inside a pod does fail (`ateworker-gvisor`'s image has no shell or
    coreutils at all, not even `ls`/`env`, so there's nothing to exec
    into), but this is NOT the same limitation as the Exclusive-tier Kata
    guest's lack of exec access, and it's fixable, not fundamental —
    root-caused via `crictl inspect` plus reading the container's own
    `/dev` and `/lib` through `/proc/<pid>/root` on the host (no
    in-container shell needed, so no guessing required):
    - GPU access itself already works. The vgpu-device-plugin's own
      `Allocate()` response injects real limits invisible to `kubectl`
      (`NVIDIA_VISIBLE_DEVICES`, `CUDA_DEVICE_MEMORY_LIMIT_0=768m`,
      `CUDA_DEVICE_SM_LIMIT=30`), an LD_PRELOAD hijack library
      (`/etc/ld.so.preload` → `libvgpu.so`, so every process gets it
      automatically, no env var needed), and the real NVIDIA driver
      userspace library stack (`libcuda.so.1`, `libnvidia-ml.so.1`, etc.,
      matching the host's 580.178.04 driver). Real `/dev/nvidia0`,
      `nvidiactl`, `nvidia-uvm`, `nvidia-uvm-tools` device nodes are
      present and accessible inside the container (confirmed via `ls
      -la /proc/<pid>/root/dev/` on the host) even though they don't
      appear in the OCI runtime spec's own `linux.devices` field — some
      other CRI-O/plugin mechanism injects them that this project hasn't
      fully traced, but their presence is directly confirmed, not
      assumed.
    - The ONLY missing piece: `ateworker-gvisor` is a statically-linked
      Go binary with no userspace libraries of its own at all (confirmed:
      no libc anywhere in the image, consistent with `ls`/`env` being
      completely absent) — so even with the driver libraries already
      mounted in, nothing in that image could run a dynamically-linked
      tool like `nvidia-smi` without glibc (`libc.so.6`,
      `ld-linux-x86-64.so.2`, and a handful of others) and the binary
      itself.
    - **Fixed**: `podgate-webhook`'s new `vgpuDebugMountPatches`
      (`cmd/podgate-webhook/main.go`) bind-mounts a configurable list of
      host paths (read-only, host path == container path, the same
      convention `devVFIOPath` already uses) into any pod that requested
      a real Volcano vGPU slice, via `VGPU_DEBUG_HOST_MOUNTS`. Not
      hardcoded paths in Go — which libc, and where the distro puts it,
      is a property of the *host*, not of GNWES (same reasoning as
      `VFIO_IOMMU_GROUPS`) — an operator sets it to `nvidia-smi`'s own
      path plus everything `ldd $(which nvidia-smi)` lists on their own
      node. See `config/webhook/podgate-webhook.yaml` for this project's
      own real, confirmed list on `dcn-gpu-0`.
    - **Verified end-to-end on real hardware (2026-11-15):** once the
      three mounts above actually land in a *freshly created* worker pod
      (the webhook only mutates pods at creation — redeploying it has no
      effect on a pod that already exists; the 3 demo pods are
      long-lived and reused across actor suspend/resume, confirmed by
      their `AGE` never resetting across many cycles, so making this
      take effect needed `kubectl delete pod -l
      ate.dev/worker-pool=gnwes-counter-vgpu` to force the WorkerPool to
      recreate them), `nvidia-smi` runs and shows the real *sliced*
      view: `0MiB / 768MiB` — exactly the 12-unit (768 MiB) request,
      never the physical card's 24576 MiB — on `00000000:D8:00.0`,
      matching `GPU-ed7db115` from the `lspci` dump above. One more
      directly-observed, not assumed, detail: the hijack library's own
      startup banner reads `[HAMI-core Msg...]` — so the real
      GPU-virtualization library here genuinely is HAMi-lineage code;
      it's Volcano's own scheduler and device plugin that *orchestrate*
      it, not generic upstream Project-HAMi's scheduler extension. Worth
      being precise about rather than over- or under-crediting HAMi
      after the 2026-11-15 correction above.
    - **Cross-actor pod/slice reclaim, confirmed live (2026-11-15), not
      just assumed from Substrate's own design docs:** suspended
      `gnwes-actor-vgpu-1`, confirmed its `WORKER POD` dropped to
      `<none>`, then created and resumed a brand-new
      `gnwes-actor-vgpu-4` against the same pool — it landed on exactly
      the pod actor-1 had just vacated
      (`gnwes-counter-vgpu-...-g4vr4`), inherited the identical
      `volcano.sh/vgpu-cores: 30` / `vgpu-memory: 12` slice, and
      `nvidia-smi` confirmed the same correctly-enforced `0MiB / 768MiB`
      limit on the same physical GPU. No GNWES code made this happen —
      it's Agent Substrate's own `WorkerPool` pod-reuse mechanism, the
      same one already proven for the Exclusive tier; the vGPU slice
      rides along with it for free because the slice is a property of
      the pod template, not of the actor. The round-trip back (suspend
      actor-4, resume actor-1) cleanly restored the original pod
      assignment, confirming this isn't one-directional either.
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
