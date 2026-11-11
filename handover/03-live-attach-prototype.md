# Handover 3/3 — Live GPU attach/detach prototype attempt

**Status: DONE, for what this session could reach.** The deeper problem
(gVisor side) is genuinely blocked without upstream engineering; the
microVM side is a real, concrete, actionable opportunity for whoever picks
this up next. Full write-up with sources: `research/live-attach-poc/README.md`.

## TL;DR

The user asked for a real attempt at the actual open problem (attaching a
real GPU to a Substrate worker pod *while it's running and reused across
many actors* — not at creation time). This session:

1. **Built and verified, live, on this machine's `kind-kind` cluster**: a
   device node can be hot-attached to and hot-detached from an
   already-running container with zero pod recreation
   (`research/live-attach-poc/hotattach.sh` + `test-pod.yaml`). Confirmed
   via `kubectl exec` (the real API path) and by watching `restartCount`/
   `containerID` stay identical throughout. This refutes the idea that the
   underlying Linux mechanism is the blocker — it's specifically DRA's API
   that only resolves at pod creation, not the kernel.
2. **Researched (via a sourced fork, not guesswork) whether that primitive
   reaches Substrate's actual sandboxes**, since Substrate runs actors in
   gVisor or microVM, never a plain container:
   - **gVisor: genuinely blocked.** `nvproxy`'s device registry is built
     once at sandbox creation (`pkg/sentry/devices/nvproxy/nvproxy.go`,
     `Register()`/`DeviceInfoFromVFS`) and there's no RPC or other
     mechanism to add a device afterward — confirmed from gVisor's own
     source, reinforced by
     [gVisor#13621](https://github.com/google/gvisor/issues/13621) (a
     maintainer-acknowledged bug showing nvproxy is brittle even to
     *creation-time* CDI variation). Fixing this needs new gVisor
     engineering this project can't do from outside.
   - **MicroVM: genuinely promising.** Substrate's `ateworker-microvm` runs
     Kata Containers on **Cloud Hypervisor** (confirmed directly from
     `cmd/ateworker-microvm/internal/ch/ch.go` in the real
     `agent-substrate/substrate` repo). Cloud Hypervisor already ships a
     live VFIO PCI device hotplug *and hot-unplug* REST API
     (`docs/hotplug.md`). The capability Substrate's model needs already
     exists in the VMM it already uses — what's missing is orchestration
     code, not new low-level engineering.

## Why this is the right place to stop, for this session

Actually prototyping the microVM path means standing up Substrate's
`ateworker-microvm` deployment (a different worker type than the gVisor one
already verified in `config/substrate-integration`), which likely needs
nested virtualization — not confirmed available under this machine's
Docker-Desktop-on-Windows `kind` setup. That's a real, separate, nontrivial
setup task, not a quick follow-on. Flagged honestly as the next concrete
step rather than attempted under time/environment pressure.

## Files this stage added

- `research/live-attach-poc/test-pod.yaml` — the plain (non-GNWES, non-DRA)
  pod used to isolate and test the hot-attach primitive.
- `research/live-attach-poc/hotattach.sh` — the reusable attach/detach tool
  (nsenter + crictl, ~50 lines), tested working end-to-end.
- `research/live-attach-poc/README.md` — the full write-up: what was
  proven, what was researched, exact caveats, and concrete next steps. Read
  this first — it has the real depth, this handover is just the summary.
- `.gitattributes` — forces `*.sh` to check out with LF; needed because
  `hotattach.sh` is shebang-executed on a Linux node and Windows'
  CRLF-on-checkout would otherwise silently break it on a fresh clone.

Committed as `de1a03d`.

## Concrete next steps (not started here)

1. Stand up `ateworker-microvm` somewhere nested virtualization is
   available (not necessarily this machine) and confirm Cloud Hypervisor's
   `clh-api.sock` is reachable the way Substrate's own `internal/ch`
   expects.
2. Prototype actual orchestration: call Cloud Hypervisor's hotplug API on
   actor resume (attach the VFIO GPU device to that exact running VM) and
   hot-unplug on suspend. This is where a genuine, hardware-measured
   "attach a real GPU to an already-running, reused sandbox" latency number
   would finally come from — the thing Tier 2/3 in `RUNBOOK.md` still can't
   produce.
3. Confirm the guest kernel actually re-probes the NVIDIA driver on a
   hot-added PCI device without a guest reboot — not yet checked, and a
   real risk to this plan if it doesn't.
4. If it works, this is also a concrete, evidence-backed thing worth
   bringing to `agent-substrate/substrate#752` or `#779` — nobody in that
   thread has discussed the microVM worker's Cloud Hypervisor hotplug
   capability specifically; the whole Tier-1/Tier-2 discussion there is
   gVisor/device-plugin framed. Same posting rule as Stage 2: draft first,
   get the user's explicit sign-off on exact text, confirm GitHub identity,
   before anything goes public.

## All three stages, in one place

- `handover/01-reposition-demo-claims.md` — docs no longer imply solved
  problems that aren't solved (commit `72e53bb`).
- `handover/02-upstream-issue-752-draft.md` — a ready-to-review, NOT-posted
  draft comment with a real, narrow, hardware-backed data point for the
  upstream Tier-1 CDI-spec-reuse debate (commit `38b1b80`).
- `handover/03-live-attach-prototype.md` (this file) — a real attempt at
  the harder problem, with a verified partial result and a precise,
  sourced map of where the remaining wall actually is (commit `de1a03d`).

Nothing in this three-stage arc claims Substrate's core problem is solved.
What changed is precision: the gap is now documented as upstream's own
problem too (Stage 1), there's a concrete, scoped technical contribution
ready for upstream's attention pending the user's approval (Stage 2), and
the remaining unknown has been narrowed from "can this be done at all" to
"build the microVM/Cloud-Hypervisor integration — here's exactly why that's
the path and here's where to start" (Stage 3).
