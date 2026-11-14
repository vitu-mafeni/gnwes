# Kernel upgrade plan: server1 / dcn-gpu-0 need Linux 6.7+ for Substrate actors

**Status: planned, NOT executed.** Nothing in this document has been run.
This is deliberately a separate task from the rest of the real-cluster
install (`INSTALL-ON-REAL-CLUSTER.md`), because it reboots shared
infrastructure and touches GPU driver compatibility.

## Why this is needed

Confirmed by reading `internal/imagecache/bundle_linux.go:156-187` and
`internal/imagecache/README.md`: every Substrate actor's rootfs is composed
as an overlay mount using the **new** Linux mount API (`fsopen`/`fsconfig`
with incremental `lowerdir+`), with **no fallback** to the old `mount(2)`
path. This specific overlayfs option requires:

> **Minimum supported kernel: Linux 6.7, or 6.6.23** (`lowerdir+`, backported
> to 6.6.y)

Confirmed the hard way on `server1` (2026-11-12): deploying the Counter demo
got all the way to actually running an actor, and failed with:

```
while composing "counter" rootfs: while mounting overlay rootfs at
".../rootfs": while creating overlay superblock: invalid argument
```

— the exact symptom of the kernel rejecting the `lowerdir+` fsconfig option
it doesn't recognize. Both nodes run **5.15.0** (Ubuntu 22.04.5's GA kernel).
This is not specific to the "golden snapshot" demo feature: it is the
rootfs-composition mechanism every actor uses, on both sandbox classes
(gVisor and microVM). No actor can run on this cluster until this is fixed.

## The fix: Ubuntu 22.04's own HWE kernel track

Confirmed available and already resolves to a kernel well above the minimum,
on both nodes (`apt-cache policy`, 2026-11-12):

- `server1`: `linux-image-generic-hwe-22.04` candidate = **6.8.0-149.149~22.04.1**
- `dcn-gpu-0`: `linux-image-generic-hwe-22.04` candidate = **6.8.0-138.138~22.04.1**

This is the lowest-risk upgrade path available: it's Ubuntu's own supported
hardware-enablement kernel for the *same* 22.04.5 release (not a different
distro, not a mainline/unsupported kernel, not a major version jump to a
newer Ubuntu release) — a well-trodden path for Ubuntu LTS + NVIDIA servers.

## The real risk: `dcn-gpu-0`'s GPU driver

Confirmed on `dcn-gpu-0` (2026-11-12):

- NVIDIA driver: `nvidia-driver-580-server` 580.178.04, confirmed **DKMS-based**
  (`dkms status` → `nvidia-srv/580.178.04, 5.15.0-186-generic, x86_64: installed`).
  DKMS means the driver's kernel module gets rebuilt automatically against
  any new kernel that gets installed — the best-case scenario for a kernel
  upgrade. This is not guaranteed to succeed, but it is the standard,
  well-supported mechanism for exactly this situation.
- `server1` has no GPU, so it carries none of this risk — just the normal
  "new kernel boots and the node is healthy" risk any kernel upgrade has.

## Recommended order and precautions

1. **Check for other users/running jobs on `dcn-gpu-0` first.** This is a
   shared research cluster node; a reboot evicts whatever is running on it.
   Coordinate a maintenance window before touching it. (Not checked yet —
   do this before anything else below.)
2. **Upgrade and reboot `server1` first**, not `dcn-gpu-0`. It has no GPU
   driver risk, and it's the fastest way to confirm the HWE kernel actually
   resolves the overlay mount issue before touching the node that also has
   GPU driver risk. It does mean a brief control-plane outage (same
   trade-off as the kubelet restarts earlier today) — expected and
   recoverable, not a concern specific to this upgrade.
3. **Only after confirming `server1` boots healthy and the overlay mount
   issue is actually fixed** (retry the Counter demo's golden-actor step),
   do the same on `dcn-gpu-0`, with the extra GPU-specific verification
   below.
4. **Never remove the old kernel packages** (`linux-image-5.15.0-187-generic`
   on server1, `-185`/`-186` on dcn-gpu-0) until the new kernel is confirmed
   good on that node. Ubuntu's GRUB menu keeps every installed kernel
   selectable at boot — if the new kernel fails to boot cleanly or breaks
   something, reboot and pick the old 5.15 kernel from GRUB's "Advanced
   options for Ubuntu" submenu. This is the rollback plan; no other rollback
   mechanism is needed as long as the old kernel package stays installed.

## Step-by-step (per node, run one at a time)

```bash
# 1. Install the HWE kernel + matching headers (needed for DKMS to rebuild
#    the NVIDIA module against it, on dcn-gpu-0 specifically)
sudo apt-get update
sudo apt-get install -y linux-image-generic-hwe-22.04 linux-headers-generic-hwe-22.04

# 2. Confirm it installed as a NEW, additional kernel (not a replacement) --
#    the old one should still be listed
dpkg -l | grep linux-image

# 3. Reboot
sudo reboot
```

```bash
# 4. After reboot, confirm the new kernel is actually running
uname -r
# expect: 6.8.0-149-generic (server1) or 6.8.0-138-generic (dcn-gpu-0),
# not 5.15.0-...
```

On `dcn-gpu-0` specifically, also confirm the GPU driver survived:

```bash
nvidia-smi
# expect: the same driver version (580.178.04) and GPU list as before
dkms status
# expect: nvidia-srv/580.178.04, <new kernel version>, x86_64: installed
```

On `server1`, also confirm the control plane came back healthy:

```bash
kubectl get nodes
kubectl -n ate-system get pods
```

## Verifying the actual fix (after server1's reboot)

Retry the Counter demo's golden-actor step -- this is the step that failed
with the overlay error:

```bash
export KUBECTL_CONTEXT=kubernetes-admin@ml-cluster
export KO_DOCKER_REPO=10.9.0.13:30728/substrate-test
go run ./cmd/ate-setup deploy demo counter --context="${KUBECTL_CONTEXT}" --ko-docker-repo="${KO_DOCKER_REPO}"
```

If it completes without the "while creating overlay superblock: invalid
argument" error, the kernel upgrade fixed it. If the golden actor pod lands
on `dcn-gpu-0` specifically (not guaranteed -- depends on scheduling), that
node also needs the same upgrade before this fully succeeds cluster-wide.

## Real gotcha hit on dcn-gpu-0, now fixed (2026-11-12)

After the HWE kernel reboot, `dcn-gpu-0` came back with **no network
connectivity at all** (SSH and ping both failed for ~20 minutes, requiring
physical console access to diagnose). Root cause, confirmed via console:

- `lspci -k` confirmed the Intel X722 1GbE NICs still had the correct
  in-tree driver (`i40e`) bound correctly -- **not a driver problem**.
- `/etc/netplan/00-installer-config.yaml` matched interfaces by the name
  `enp96s0f0`..`enp96s0f3` (no suffix). Under the new 6.8 kernel, the
  in-tree `i40e` driver exposes each port's `phys_port_name` via sysfs
  (something the 5.15-bundled `i40e` version apparently didn't expose the
  same way), so `systemd-udevd` now names the same physical ports
  `enp96s0f0np0`..`enp96s0f3np3` -- an extra `npN` suffix. Netplan's name
  match found nothing, so it silently left all four interfaces
  unconfigured (`state DOWN`, `qdisc noop`) -- not an error, just nothing
  to apply the config to.
- Fixed by updating `/etc/netplan/00-installer-config.yaml` to use the new
  interface names and re-running `netplan apply` (original backed up as
  `00-installer-config.yaml.bak-5.15-names`). Confirmed permanent: netplan
  regenerates `systemd-networkd`'s config from this file and it's
  re-applied on every boot, so it holds across reboots on this kernel.
- **If `server1` or any other node's reboot also loses network
  connectivity after this same kernel upgrade, check this exact thing
  first**: `ip link` for the real interface names vs. what
  `/etc/netplan/*.yaml` matches by name, before assuming a driver or
  hardware problem.
- GPU driver survived cleanly: `dkms status` shows the `nvidia-srv` module
  built for both the old (5.15.0-186) and new (6.8.0-149) kernels;
  `nvidia-smi` shows both TITAN RTX GPUs correctly.

## Still not done after this

- `dcn-gpu-0` itself needs the same upgrade, verified separately with its
  GPU-specific checks above, before any GPU-scheduled actor can run.
- Not yet checked: whether `cnlab-runtime`'s patched CRI-O/CRIU build has
  its own kernel-version assumptions that a jump to 6.8 could violate (the
  FluidCR checkpoint/restore work earlier in this project depends on a
  specific patched CRIU build). Worth a quick sanity check against
  `cnlab-runtime`'s own documentation or whoever maintains it before
  committing to this on `dcn-gpu-0`, given FluidCR is core to the
  Shared-tier checkpoint/restore work this whole effort is building toward.
