# Live GPU attach/detach on an already-running, reused worker — a real attempt

This is a genuine research attempt at the actual open problem identified in
Stage 1/2 (`handover/01-*`, `handover/02-*`): attaching/detaching a real GPU
to a Substrate worker pod *while it's running and being reused across many
sequential actors*, which vanilla Kubernetes DRA can't do (it only resolves
a `ResourceClaim` at pod creation). Flagged to the user up front as
open-ended research, not a demo tweak — this is exactly that: a real
experiment with a real, verified result, plus research-grounded limits on
how far it goes. **It does not fully solve the problem.** It narrows it
precisely, with evidence, to one sandbox type.

## Part 1 — proven: the underlying primitive is NOT blocked, only DRA's API is

Hypothesis: Kubernetes DRA's "pod-creation-time-only" binding is a kubelet
scheduling/API-resolution rule, not a kernel-level wall — the Linux
mechanisms a container actually runs on (mount namespaces, cgroup device
access) don't inherently require the container to be recreated to change
what devices it can see.

**Tested directly, live, on this machine's `kind-kind` cluster** (not
simulated, not against the shared remote GPU node — this needed no real GPU
to test the mechanism, so it ran entirely locally):

1. Deployed a plain pod (`test-pod.yaml`, `busybox:1.36 sleep infinity`,
   `privileged: true` — see caveat below), no GNWES, no DRA involved.
2. Confirmed baseline: `/dev/gnwes-fake-gpu` absent from the running
   container (`ls` → `No such file or directory`).
3. Ran [`hotattach.sh`](hotattach.sh) `attach <container-id> gnwes-fake-gpu 1 9`
   — resolves the container's host PID via `crictl inspect`, then
   `nsenter -t <pid> -m -- mknod /dev/gnwes-fake-gpu c 1 9` (char device
   1:9 = `/dev/urandom`'s major:minor, chosen so the node is actually
   readable afterward, not just a dangling special file — a real stand-in
   for a real GPU device node, not just a name).
4. **Verified via `kubectl exec`** (the real Kubernetes API path, not a
   side-channel): the device node is visible and reading from it returns
   real bytes.
5. Ran `hotattach.sh detach` — the node disappears from the container's view
   again, confirmed again via `kubectl exec`.
6. **Throughout all of this, `restartCount` stayed `0` and the
   `containerID` never changed** — the pod was never recreated, never
   restarted. The device appeared and disappeared on a container Kubernetes
   considers continuously, uninterruptedly running.

This is real, reproducible evidence that the specific mechanism Substrate's
model needs — attach device → use it → detach device, with the worker pod
itself never torn down and recreated — is achievable at the container-runtime
level. The blocker in `config/dra`'s "real gap" and the upstream `#752`
discussion isn't "Linux can't do this"; it's "native K8s DRA's *API* never
asks it to."

### Caveat this half is honest about

The test pod ran `privileged: true`, which (on cgroup v2, confirmed on this
node) means the runtime's default device cgroup eBPF filter is not
restrictive in the first place — so this proves the mount-namespace half of
the problem (making the node visible) cleanly, but does **not** by itself
prove live cgroup-v2 device-allowlist updates for a *non-privileged*
container (the realistic case — a real worker pod isn't privileged). That's
a different, well-known but separate technique (cgroup v2 replaced the old
`devices.allow` text file with a BPF_CGROUP_DEVICE program the runtime
installs; updating it for a running container needs attaching a new/updated
BPF program to that cgroup, not just a text write) — not attempted here, and
not needed to answer Part 2's question, which is the one that actually
decides whether this is usable for Substrate.

## Part 2 — researched: does this reach Substrate's actual sandboxes?

Substrate runs actors in one of two sandbox types (per the Nov 2026
announcement and the repo itself): **gVisor** or **microVM**. The container-
level primitive above only matters if one of these actually lets a
device appear inside the sandbox *after* the sandbox is already running.
Researched both against gVisor's own source and Substrate's own repo (not
guessed):

### gVisor — blocked, confirmed from source, not just undocumented

gVisor's NVIDIA passthrough (`nvproxy`) is an ioctl-forwarding proxy with its
own in-sentry device registry — not a transparent pass-through of the host's
`/dev`. Per `pkg/sentry/devices/nvproxy/nvproxy.go`,
[`Register()`](https://github.com/google/gvisor/blob/master/pkg/sentry/devices/nvproxy/nvproxy.go)
builds this registry **once**, and its own `DeviceInfoFromVFS` doc comment
confirms it's one-time setup, not re-evaluated per access. The actual device
*files* the app sees are created only during container/sub-container
creation (`runsc/boot/vfs.go`, `createDeviceFiles` →
`(*Loader).createContainerProcess` / `startSubcontainer`) — there is no RPC
or other mechanism to add a device to an already-running sentry.

This isn't just an absence of a feature — it's actively brittle even to
*creation-time* variation: [gVisor issue #13621](https://github.com/google/gvisor/issues/13621)
("nvproxy is incompatible with the NVIDIA DRA driver") shows nvproxy either
silently exposes **every** host GPU (because it keys off
`NVIDIA_VISIBLE_DEVICES`, ignoring CDI's actual per-claim device-node list)
or nil-panics, because it hardcodes device paths like `/dev/nvidiactl` and
bind-mounts them by path rather than creating nodes from major:minor the way
`runc` does. A gVisor maintainer's own diagnosis in that issue confirms this
is a real, known limitation, not a documentation gap.

**Conclusion: hot-attach into a gVisor-sandboxed actor needs real gVisor
engineering (a new sentry-side RPC to register a device into a running
nvproxy instance) that doesn't exist today, even as a proposal.** Out of
scope here — this isn't something GNWES or a thin orchestration layer can
work around from outside gVisor.

### MicroVM — structurally solvable *today*, no new low-level engineering

Checked which VMM Substrate's microVM worker actually uses, directly from
its own repo: `cmd/ateworker-microvm` runs **Kata Containers on Cloud
Hypervisor** — Substrate's own `internal/ch` package drives the Cloud
Hypervisor REST API (`clh-api.sock`) for pause/snapshot/resume.

Cloud Hypervisor's REST API already supports **live PCI device hotplug and
hot-unplug into an already-running VM**, including VFIO passthrough devices
(`docs/hotplug.md`; hotplug/unplug for block/net/pmem/virtiofs/vsock landed
in v0.7.0; a `remove-device` call specifically added hot-*un*plug of VFIO PCI
devices). This is exactly the attach/detach primitive Substrate's model
needs for a device to move in and out of a running, reused worker — and it
already exists in the VMM Substrate already runs, with no new hypervisor
engineering required.

**Conclusion: the microVM path is the genuinely promising one.** Wiring it
up would mean: (1) GNWES/Substrate orchestration calling Cloud Hypervisor's
existing hotplug API around the resume/suspend lifecycle, in place of (or
alongside) a `ResourceClaim`; (2) handling the DRA-side bookkeeping so the
device isn't double-allocated while it's "borrowed" into a running VM; (3)
confirming the guest-kernel side actually re-probes/re-attaches the NVIDIA
driver on a hot-added PCI device without a guest reboot (not yet checked —
see Next steps). None of this needs upstream gVisor/Cloud-Hypervisor changes
— it's integration engineering on top of an API that already exists.

Caveat on confidence: Firecracker's own VFIO hotplug support looked
comparatively immature in this pass (docs/PRs suggest in-progress, not
clearly merged) — irrelevant to Substrate specifically, since its microVM
worker is confirmed to run Cloud Hypervisor, not Firecracker, but worth
flagging if that ever changes.

## What this stage did NOT do

- Did not attempt the microVM path against a real Substrate microVM worker —
  that needs standing up Substrate's `ateworker-microvm` deployment (a
  different worker type than the gVisor one already verified in
  `config/substrate-integration`), likely needs nested virtualization this
  Windows/Docker-Desktop/`kind` setup may not support, and is real scope
  beyond what this session attempted.
- Did not check whether a hot-added PCI device is actually usable by the
  NVIDIA driver inside a running guest without a guest-kernel-side
  reset/reprobe — a real open question before calling the microVM path done,
  not just wired.
- Did not touch `cmd/ateworker-gvisor/runsc.go` or any other Substrate
  source — same boundary as `config/substrate-integration/README.md`
  already drew.
- Did not implement live cgroup-v2 device-allowlist updates for a
  non-privileged container (see Part 1's caveat) — moot for the gVisor path
  (blocked regardless) but would matter if this primitive were ever reused
  for a non-sandboxed, non-privileged worker.

## Reproducing Part 1

Needs a `kind` cluster with a Docker-accessible node (works with any local
`kind` cluster; used `kind-kind` here since it was already up):

```bash
kubectl --context kind-kind apply -f test-pod.yaml
kubectl --context kind-kind wait --for=condition=Ready pod/hotattach-target -n gnwes-hotattach-poc

# resolve the container ID and its host PID
cid=$(kubectl --context kind-kind get pod hotattach-target -n gnwes-hotattach-poc \
  -o jsonpath='{.status.containerStatuses[0].containerID}' | sed 's#containerd://##')

docker cp hotattach.sh kind-control-plane:/tmp/hotattach.sh
docker exec kind-control-plane chmod +x /tmp/hotattach.sh

# before: not found
kubectl --context kind-kind exec -n gnwes-hotattach-poc hotattach-target -- ls /dev/gnwes-fake-gpu

docker exec kind-control-plane /tmp/hotattach.sh attach "$cid" gnwes-fake-gpu 1 9
kubectl --context kind-kind exec -n gnwes-hotattach-poc hotattach-target -- \
  sh -c "ls -la /dev/gnwes-fake-gpu && head -c 8 /dev/gnwes-fake-gpu | od -An -tx1"

docker exec kind-control-plane /tmp/hotattach.sh detach "$cid" gnwes-fake-gpu
kubectl --context kind-kind exec -n gnwes-hotattach-poc hotattach-target -- ls /dev/gnwes-fake-gpu  # gone again

# restartCount stayed 0 and containerID never changed throughout — confirm with:
kubectl --context kind-kind get pod hotattach-target -n gnwes-hotattach-poc \
  -o jsonpath='{.status.containerStatuses[0].restartCount} {.status.containerStatuses[0].containerID}{"\n"}'

kubectl --context kind-kind delete -f test-pod.yaml   # cleanup
```

(On a Windows/Git-Bash host, prefix `docker` commands touching `/tmp/...`
paths with `MSYS_NO_PATHCONV=1` or the path gets silently rewritten to a
Windows path and the copy/exec fails.)

## Next steps, if this is picked back up

1. Stand up Substrate's microVM worker (`ateworker-microvm`) and confirm (a)
   nested virtualization is available wherever that runs, (b) Cloud
   Hypervisor's `clh-api.sock` is reachable from a controller the way
   `internal/ch` expects.
2. Prototype the orchestration: on actor resume, call Cloud Hypervisor's
   hotplug API to add the VFIO GPU device to that exact running VM; on
   suspend, call hot-unplug. Compare timing against DRA's pod-creation-time
   path — this is where a genuine "attach a real GPU to an already-running,
   reused sandbox" number would finally come from.
3. Confirm guest-kernel-side NVIDIA driver re-probe behavior on hot-added
   PCI devices (point above, not yet checked).
4. If it works, this is also a concrete, evidence-backed addition worth
   bringing back to `agent-substrate/substrate#752` / `#779` — the Tier-1
   DRA discussion there is entirely gVisor/device-plugin framed; nobody in
   that thread has discussed the microVM worker's Cloud Hypervisor hotplug
   capability specifically. That would need its own round of the user's
   explicit sign-off before posting, same as Stage 2.
