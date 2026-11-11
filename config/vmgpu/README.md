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

## How this differs from GPUWorkerPool/GPULease, concretely

| | GPUWorkerPool / GPULease | VFIOGPUPool / VMGPUAttachment |
|---|---|---|
| Device inventory | Simulated slots, conjured by `Resize` | Real, physical VFIO devices, a fixed list you declare (`spec.devices`) |
| "Attach" | `time.Sleep(attachLatencyMillis)` + pod annotation | Real `PUT /api/v1/vm.add-device` against a real cloud-hypervisor api-socket |
| What drives attach/detach | Object create/delete (and a TTL) | **Flipping `spec.desiredState`** between `Attached`/`Detached` on the SAME object — this is the part that actually matters: it's what lets one object track a device across many attach/detach cycles as Substrate reuses one worker pod across many actors, instead of needing a new object (and a new pod) each time |
| Backing package | `internal/pool` | `internal/vfiopool` (same Acquire/Release race-safety property, proven the same way — see its own `TestConcurrentAcquireReleaseNeverDoubleBooks`) |
| REST client | n/a | `internal/chclient`, wire format taken directly from cloud-hypervisor's own OpenAPI spec and `docs/hotplug.md`, not guessed |

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

## What's NOT verified

- **No real cloud-hypervisor instance, no real VFIO GPU, no real guest.**
  This dev environment has no `/dev/kvm` (confirmed in
  `research/live-attach-poc/README.md`) — nested virtualization isn't
  available, so a real cloud-hypervisor VM couldn't be booted here at all.
  Everything above the VMM boundary is real and tested; the VMM itself, in
  every test run so far, is `cmd/fakevmm`, not cloud-hypervisor.
- **No bridge to Substrate's actual actor lifecycle.** Flipping
  `spec.desiredState` is what a bridge watching Substrate actor resume/
  suspend transitions (e.g. polling `kubectl-ate get actor -o json`, since
  Substrate's `Actor`/`WorkerAssignment` aren't Kubernetes CRDs you can
  `kubectl get` or watch directly — only `WorkerPool` is) would do. That
  bridge is not built. This mechanism is the real, working "how do I
  actually hot-attach a device" half; "when do I decide to" is still a
  manual `kubectl patch` or a future, separate component.
- **No real Substrate `ateworker-microvm` deployment exercised.** The
  `config/substrate-integration` work so far used Substrate's gVisor
  worker, not its microVM one. Wiring this against a real one needs a
  machine with nested virtualization, standing that worker type up, and
  confirming the guest's NVIDIA driver actually re-probes a hot-added PCI
  device without a reboot (also unconfirmed — see
  `research/live-attach-poc/README.md`'s "Next steps").

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
