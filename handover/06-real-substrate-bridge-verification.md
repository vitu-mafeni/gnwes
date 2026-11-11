# Handover 6 — the bridge verified against the real Substrate control plane

**Status: DONE.** This is the strongest verification this mechanism can get
without real cloud-hypervisor/KVM hardware. Full detail in
`config/vmgpu/README.md`'s "What's verified" and "Driving this from a real
Substrate actor instead of a fake one" sections; this is the continuity
summary.

## What changed since handover 5

Handover 5 built the bridge and verified it against `cmd/fakekubectlate` — a
simulated actor. This stage ran it against the **real** thing:

- The real Substrate control plane from `config/substrate-integration`
  (`ate-system` namespace) was still up from an earlier session —
  confirmed `kubectl --context kind-kind get pods -n ate-system` showed
  everything `Running`, 17h old.
- The real cross-compiled `kubectl-ate-linux` binary and
  `kubeconfig-internal.yaml` were still present at
  `D:\codes\agent-substrate\bin\`.
- The real `gnwes-actor-1` actor (from that same earlier session) was still
  there, `ACTOR_STATE_SUSPENDED`.

`kubectl-ate` doesn't build or run natively on this Windows box, so
`cmd/substrate-gpu-bridge`'s `-kubectl-ate-bin` needed something it could
`exec.Command` directly that would itself invoke the real binary inside a
container on the `kind` docker network. Wrote a small (~20 line), **not
checked into the repo** Go wrapper for this (machine-specific mount path:
`D:\codes\agent-substrate\bin`) at
`<scratchpad>/kubectlatedocker/kubectl-ate-docker.exe` — forwards its own
args straight through to `docker run --rm --network kind -v
D:/codes/agent-substrate/bin:/bin2 -w /bin2 debian:bookworm-slim
./kubectl-ate-linux --kubeconfig kubeconfig-internal.yaml <args>`. Verified
first that the real binary's JSON (`status.state`) parses exactly as
`internal/substratebridge.ParseActorState` expects — it does, confirming
the earlier "source read, not tested" caveat from handover 5.

## The actual test

1. Deployed a fresh `VFIOGPUPool`/`VMGPUAttachment` (desiredState:
   Detached) on `kind-kind`, `cmd/fakevmm` as the VMM stand-in (still no
   KVM here), `cmd/manager` pointed at the right context (**tripped on this
   once**: `go run ./cmd/manager` picks up whatever `kubectl config
   current-context` is at process start, not a flag — had it pointed at
   `kind-gnwes-demo` briefly, caught and restarted with `kind-kind`).
2. Started `cmd/substrate-gpu-bridge` with `-kubectl-ate-bin
   <the wrapper>`, `-atespace ate-demo-counter -actor gnwes-actor-1`,
   3s poll interval. Confirmed it correctly observed `ACTOR_STATE_SUSPENDED`
   and left the attachment `Detached`.
3. Ran the REAL `kubectl-ate resume actor gnwes-actor-1 --atespace
   ate-demo-counter` (through the wrapper) — a genuine Substrate control-plane
   call, the same one a real operator or Substrate's own scheduler would
   make. Within one poll (~3s), the bridge logged
   `actor state ACTOR_STATE_RUNNING -> desiredState Attached`, and
   `status.phase` reached `Attached` with a real guest BDF 3ms later.
4. Ran the REAL `kubectl-ate suspend actor` the same way — bridge logged
   the matching `Detached` transition, reconciler released the device.
5. Cleaned up in the order that avoids the known finalizer-hang gotcha
   (stop bridge → delete namespace while `cmd/manager` still running →
   stop manager → stop `fakevmm` → remove local binaries) and confirmed
   `gnwes-actor-1` was left `SUSPENDED` (its state before this test
   started) and nothing else in `ate-system`/`ate-demo-counter` was
   touched.

## What this proves, concretely

Every hop in the chain is now real on both sides except one: a real
Substrate actor → a real `kubectl-ate` call → a real `ate-api-server`
response → real `internal/substratebridge` parsing/decision logic → a real
Kubernetes patch → the real `VMGPUAttachmentReconciler` → a real
HTTP-over-unix-socket `vm.add-device`/`vm.remove-device` call. The only
thing still not real is what's on the other end of that last call: a real
cloud-hypervisor VMM with a real VFIO GPU and a real guest. That is the one
remaining gap, and it's a hardware/environment gap (no `/dev/kvm` here), not
a code gap.

## Concrete next step

The only thing left: get this pointed at a real cloud-hypervisor instance.
Either a machine with `/dev/kvm` (bare-metal Linux, or a cloud VM with
nested virt enabled), or the user's real GPU node (`dcn-gpu-0` from much
earlier in this session) if it turns out to support KVM natively — not
confirmed, would need the same "you run commands, paste output"
collaboration this session already established for that node. Once
reachable, point `VMGPUAttachment.spec.apiSocket` at its real api-socket and
re-run exactly this same test — the bridge and reconciler code need no
changes for that; only the VMM at the far end does.
