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

## Prerequisites and node configuration

This is the clean, working recipe — not the debugging journey it took to find
it (that's in `README.md`'s "What's verified" section and the commit history,
for the historical record). Follow this start to finish on a fresh node and
you should reach a working Cloud Hypervisor + real-GPU setup without hitting
any of today's wedges.

### Hardware/OS requirements

- Bare metal (or a VM with nested virtualization already enabled) — this
  mechanism needs real `/dev/kvm`. Confirmed working on Ubuntu 22.04.5 LTS,
  kernel 5.15.0-186-generic, with real Intel VT-x.
- At least one NVIDIA GPU you can dedicate to VFIO passthrough. **If the node
  has multiple identical GPU models, you must use the boot-time exclusion
  method below (not a live unbind) — see "Why boot-time, not live-unbind"**.
- `sudo` access, and — if the node is shared/production — a quiet window or
  explicit sign-off, since steps 2 and 4 below involve reboots.

### 1. Confirm KVM is actually available

```bash
ls -l /dev/kvm
lsmod | grep -i kvm
```

Expect `/dev/kvm` to exist and `kvm_intel` (or `kvm_amd`) to show as loaded.
If `/dev/kvm` doesn't exist at all, this is either a VM without nested
virtualization enabled, or a BIOS setting (VT-x/AMD-V) that needs enabling
first — stop here until it does.

### 2. Enable IOMMU, if it isn't already

```bash
cat /proc/cmdline   # look for intel_iommu=on or amd_iommu=on already present
```

If absent:

```bash
sudo cp /etc/default/grub /etc/default/grub.bak.$(date +%s)
grep GRUB_CMDLINE_LINUX /etc/default/grub   # see what's there before changing it
sudo sed -i -E 's/^(GRUB_CMDLINE_LINUX_DEFAULT=")([^"]*)(")$/\1\2 intel_iommu=on iommu=pt\3/' /etc/default/grub
grep GRUB_CMDLINE_LINUX /etc/default/grub   # confirm it looks right BEFORE rebuilding
sudo update-grub   # watch for errors; a missing-firmware warning for an
                    # unrelated module (e.g. "ast") is harmless and expected
sudo reboot
```

After reboot, confirm:

```bash
cat /proc/cmdline                               # should show intel_iommu=on iommu=pt
dmesg | grep -i -e DMAR -e IOMMU | head -5       # expect "DMAR: IOMMU enabled"
```

### 3. Identify your target GPU's PCI functions and IOMMU group

```bash
lspci -Dnn | grep -i nvidia   # find your target GPU's base address, e.g. 0000:18:00.0

# a consumer GPU typically has 4 functions (VGA/audio/USB/USB-C) that MUST
# move to VFIO together -- they share one IOMMU group
for d in /sys/bus/pci/devices/0000:18:00.*; do
  echo "$d -> iommu group $(basename "$(readlink -f "$d/iommu_group")")"
done
ls /sys/kernel/iommu_groups/<group-number-from-above>/devices/
```

Confirm the group contains *only* your target GPU's own functions, nothing
from another device you need to keep on the host.

### 4. Reserve that GPU for VFIO at boot (not a live unbind)

**Why boot-time, not live-unbind:** on an actively GPU-Operator-managed
node, attempting `echo <dev> > .../driver/unbind` on an already-bound,
already-in-use GPU reproducibly wedges — the process can survive even
`kill -9` — because something (a GPU-sharing scheduler's device plugin, a
DCGM/`nv-hostengine` monitoring daemon, possibly others never fully
identified) holds the device open and NVIDIA's driver waits indefinitely for
that handle to close. Chasing down every possible holder live is fragile and
not guaranteed to ever find them all. The fix that actually worked: make the
GPU belong to `vfio-pci` from the very start of boot, so `nvidia.ko` never
gets a chance to claim it in the first place — there is then nothing to
unbind, and nothing to race against.

```bash
sudo tee /etc/initramfs-tools/scripts/init-top/gnwes-vfio-gpu0.sh > /dev/null <<'HOOK'
#!/bin/sh
PREREQ=""
prereqs() { echo "$PREREQ"; }
case "$1" in
  prereqs) prereqs; exit 0 ;;
esac

for dev in 0000:18:00.0 0000:18:00.1 0000:18:00.2 0000:18:00.3; do
  if [ -e "/sys/bus/pci/devices/$dev" ]; then
    echo vfio-pci > "/sys/bus/pci/devices/$dev/driver_override" 2>/dev/null
    echo "$dev" > /sys/bus/pci/drivers_probe 2>/dev/null
  fi
done
HOOK
sudo chmod +x /etc/initramfs-tools/scripts/init-top/gnwes-vfio-gpu0.sh

cat /etc/initramfs-tools/scripts/init-top/gnwes-vfio-gpu0.sh   # confirm it looks right
sudo update-initramfs -u -k $(uname -r)   # watch for errors before rebooting
sudo reboot
```

Replace the 4 PCI addresses with your target GPU's own (from step 3). This
is **initramfs-tools** syntax (Debian/Ubuntu); a dracut-based distro needs
the equivalent dracut module convention instead.

After reboot, confirm:

```bash
for dev in 0000:18:00.0 0000:18:00.1 0000:18:00.2 0000:18:00.3; do
  echo "$dev -> $(readlink -f /sys/bus/pci/devices/$dev/driver 2>/dev/null | xargs -r basename)"
done
nvidia-smi   # should now list one fewer GPU than before -- the reserved one is gone from the host's view
```

**This reservation is permanent across reboots** until the hook is removed
and the initramfs rebuilt again — the GPU will not be visible to
`nvidia-smi`, GPU Operator, or any GPU-sharing scheduler on this node until
you revert it.

### 5. Install Cloud Hypervisor, `ch-remote`, and boot firmware

```bash
mkdir -p ~/ch-test && cd ~/ch-test
wget -q https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v54.1/cloud-hypervisor-static -O cloud-hypervisor
wget -q https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v54.1/ch-remote-static -O ch-remote
chmod +x cloud-hypervisor ch-remote
./cloud-hypervisor --version

wget -q https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.4.2/hypervisor-fw
sudo setcap cap_net_admin+ep ./cloud-hypervisor
```

### 6. Build a test guest image

```bash
sudo apt-get update -qq && sudo apt-get install -y -qq qemu-utils cloud-image-utils tmux
wget -q https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img
qemu-img convert -p -f qcow2 -O raw jammy-server-cloudimg-amd64.img jammy-server-cloudimg-amd64.raw
qemu-img resize jammy-server-cloudimg-amd64.raw +4G

cat > user-data <<'EOF'
#cloud-config
password: cloud123
chpasswd: { expire: False }
ssh_pwauth: True
EOF
cat > meta-data <<'EOF'
instance-id: chtest01
local-hostname: chtest
EOF
cloud-localds seed.img user-data meta-data
```

Jammy (22.04), not an older cloud image — its kernel (5.15+) is already past
the "needs ACPI GED, 5.5-rc1 or later" requirement `docs/hotplug.md` states
for PCI hotplug, without relying on an older kernel's config flag. **Login
is the distro's default user (`ubuntu`), not a separate `cloud` user** — the
cloud-config above only sets that user's password.

### 7. Boot the test VM and confirm its REST API answers

```bash
tmux new-session -d -s chvm \
  "sudo ./cloud-hypervisor \
     --api-socket /tmp/ch-api.sock \
     --firmware ./hypervisor-fw \
     --disk path=jammy-server-cloudimg-amd64.raw,image_type=raw path=seed.img,image_type=raw \
     --cpus boot=2 \
     --memory size=2048M \
     --serial tty --console off"
sleep 5
sudo ./ch-remote --api-socket=/tmp/ch-api.sock info
```

Expect a JSON config with `"state":"Running"`. If you instead get
`Connection refused`, the VM didn't start — check
`tmux capture-pane -t chvm -p` for the actual boot error before continuing.

At this point the node matches exactly the state Phase 0/1 below assume.

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

**Done, confirmed on real hardware (2026-11-13/14) — this section originally
described Phase 3 as future work requiring a separate Kata Containers
install. That assumption was wrong, corrected below once the real
mechanism was understood by actually running it.**

A Substrate `WorkerPool` with `sandboxClasses: [microvm]` runs the
**microVM** worker type, `ateworker-microvm` (Kata on Cloud Hypervisor,
confirmed from Substrate's own source). The api-socket `VMGPUAttachment`
targets is the *agent's own* sandbox, not a side VM — exactly the real end
state this phase set out to reach.

What actually made this work, corrected against the plan above:

- **No separate Kata Containers install needed on `dcn-gpu-0` at all.**
  `ateworker-microvm` runs as a perfectly ordinary container under CRI-O's
  own plain default runtime (confirmed: no pod on this cluster sets
  `runtimeClassName`, so CRI-O's resolved `default_runtime` — `runc` on
  this cluster — applies; see `config/substrate-integration/
  REPRODUCTION-GUIDE.md`). It is not itself run under a Kata-shimmed CRI-O
  runtime handler; it manages its own nested cloud-hypervisor VM directly
  via `/dev/kvm` from inside that ordinary container. Step 1 and step 2's
  Kata/CH smoke test above were never actually necessary.
- **The real api-socket path is `/run/vc/vm/<actor-uid>/clh-api-restore.sock`,
  not `clh-api.sock`.** Every `kubectl-ate resume`, including an actor's
  very first one, goes through Substrate's restore-from-checkpoint path,
  which launches cloud-hypervisor with this name permanently — confirmed
  the hard way (2026-11-13), see `internal/substratebridge/bridge.go`'s
  `clhAPISocketName` doc comment.
- **Per-worker `VMGPUAttachment` creation is built, not a needed
  enhancement.** `GPUWorkerPoolReconciler.reconcileExclusive`
  (`internal/controller/gpuworkerpool_controller.go`) creates one
  `VMGPUAttachment` per real Exclusive-tier worker pod automatically as
  pods come and go.
- **`cmd/substrate-gpu-bridge` resolving the socket path itself is built**
  (the second option this section originally listed as unbuilt): it
  resolves whichever actor is *currently* assigned to its worker pod
  (`internal/substratebridge`) and writes `apiSocket`/`desiredState`
  together from that actor's real UID, rather than watching one fixed
  actor name.
- **Confirming the GPU inside the guest doesn't need `kubectl-ate exec`**
  (which doesn't exist — `kubectl-ate --help` lists no `exec` subcommand,
  and the worker container itself has no shell to exec into). The real
  confirmation is the guest's own cloud-hypervisor **console log**
  (`/var/lib/gnwes/vcvm/<vcVMID>/<actorUID>/console.log` on the node): it
  shows the guest kernel live-enumerating the device
  (`pci 0000:00:08.0: [10de:1e02] ...`) and a `kata-agent` uevent
  (`action: "add"`), timestamped to the millisecond against the
  attachment's own `lastTransitionAt`.

See `config/substrate-integration/REPRODUCTION-GUIDE.md` and
`config/vmgpu/README.md` for the full real-hardware run this phase
produced, including the real bugs found and fixed along the way.

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
