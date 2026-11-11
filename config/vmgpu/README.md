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

## What's NOT verified

- **No real cloud-hypervisor instance, no real VFIO GPU, no real guest.**
  This dev environment has no `/dev/kvm` (confirmed in
  `research/live-attach-poc/README.md`) — nested virtualization isn't
  available, so a real cloud-hypervisor VM couldn't be booted here at all.
  Everything above the VMM boundary is real and tested, now including a
  real Substrate actor's real resume/suspend lifecycle; the VMM itself, in
  every test run so far, is still `cmd/fakevmm`, not cloud-hypervisor.
- **No real Substrate `ateworker-microvm` deployment exercised.** The
  `config/substrate-integration` work so far (and the real-actor bridge
  test above) used Substrate's gVisor worker, not its microVM one. Wiring
  this against a real one needs a machine with nested virtualization,
  standing that worker type up, and confirming the guest's NVIDIA driver
  actually re-probes a hot-added PCI device without a reboot (also
  unconfirmed — see `research/live-attach-poc/README.md`'s "Next steps").
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
FAKE_ACTOR_STATE_FILE="$(pwd)/actor-state.txt" ./substrate-gpu-bridge \
  -kubectl-ate-bin ./fakekubectlate \
  -atespace fake-atespace -actor fake-actor \
  -attachment-namespace gnwes-vmgpu -attachment-name actor-worker-1-gpu \
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
substrate-gpu-bridge \
  -kubectl-ate-bin /path/to/your/kubectl-ate-docker-wrapper \
  -atespace ate-demo-counter -actor gnwes-actor-1 \
  -attachment-namespace gnwes-vmgpu -attachment-name actor-worker-1-gpu \
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
