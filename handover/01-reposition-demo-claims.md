# Handover 1/3 — Reposition demo claims honestly

**Status: DONE.** This file is a handover note for whoever (human or another
AI session, e.g. Codex) picks up the next stage — it assumes no prior
context beyond this repo.

## Why this stage happened

The user pointed out that nothing built in this repo so far actually solves
Agent Substrate's real problems — it was flagged by reading NVIDIA's own
Nov 2026 announcement
([blog](https://developer.nvidia.com/blog/agent-substrate-advancing-kubernetes-for-agents-at-scale/)).
That post confirms the core thing Substrate is for: reusing a small pool of
warm worker Pods across many sequential agent "actors" via suspend/restore,
*without* going through pod create/schedule/kubelet each time. GPU device
support on that model is explicitly **not solved yet upstream either** —
tracked only as an open design issue
([agent-substrate/substrate#752](https://github.com/agent-substrate/substrate/issues/752)),
with GPU suspend/resume across actors described as "a design" they propose,
not shipped code.

This repo (GNWES) had built two real things before this stage:
1. `config/dra` — a genuine Kubernetes DRA + CDI kubelet plugin
   (`cmd/mock-gpu-driver`), verified end-to-end on real NVIDIA hardware
   (3x TITAN RTX, GPU-Operator-managed node). Real, but only proves DRA's
   vanilla model: a device bound once, at pod-creation time.
2. `config/substrate-integration` — GNWES's scheduling-gate + lease
   mechanism wired onto a real, upstream `agent-substrate/substrate`
   `WorkerPool`, with a real Substrate actor really resuming onto the exact
   pod GNWES gated. Real, but the GPU "attach" step there is still GNWES's
   own sleep + annotation stand-in — never rewired to real CDI injection.

Neither of these closes the actual gap: attaching/detaching a real GPU to a
worker pod that Substrate **reuses across many actors**, which vanilla K8s
DRA fundamentally can't do (it only resolves `ResourceClaim`s at pod
creation). That mismatch is real, not a shortcut either project glossed
over — it's the same thing upstream's own roadmap hasn't solved.

## What changed in this stage

Pure documentation — no code changes. Four files edited to add explicit,
specific cross-references to the upstream blog post and issue #752, so the
"gap" reads as a confirmed, still-open upstream problem rather than
something only this build noticed:

- [`README.md`](../README.md) (top-level) — "Real Agent Substrate
  integration" section now has a "What this does *not* claim" paragraph.
- [`config/dra/README.md`](../config/dra/README.md) — "A real gap this
  surfaced in Agent Substrate" section expanded with the upstream citation
  and an explanation of *why* it's hard (pod-creation-time-only binding vs.
  pod-reuse model), not just *that* a field is missing.
- [`config/substrate-integration/README.md`](../config/substrate-integration/README.md)
  — "What this is honest about NOT proving" section, same treatment.
- [`RUNBOOK.md`](../RUNBOOK.md) — Tier 3 section gets a short
  "what this does and doesn't prove" paragraph so someone following just the
  runbook (not the sub-READMEs) still sees this.

Committed as `72e53bb` ("Stop implying real DRA/Substrate integration solves
live GPU attach").

`DEMO.md` was checked and needed no changes — it already clearly labels
every simulated number as structural, not a hardware benchmark.

## What did NOT change

- No code. `cmd/mock-gpu-driver`, `internal/controller`, `internal/pool`,
  all manifests under `config/` — untouched.
- The underlying claims that *are* true were kept: the DRA+CDI mechanism
  really is verified on real hardware (for a freshly-created pod); the real
  Substrate `WorkerPool` gating/leasing really is verified against upstream's
  real control plane. Nothing here walks those back — it only adds the
  "but this specific harder thing remains open" context next to them.

## Next stages (not started by this file)

- **Stage 2** (`handover/02-upstream-issue-752-draft.md` once written): draft
  a technical comment for `agent-substrate/substrate#752`, based on what
  building `cmd/mock-gpu-driver/nvidia.go` taught us (the GPU-Operator
  CDI-spec-reuse trick / `NVIDIA_CDI_MODE=existing`, the backend-interface
  split that works the same for gVisor or microVM sandboxes). **Draft only
  — do not post to GitHub without the user's explicit go-ahead**; posting to
  a public upstream issue is a "publish public content" action that needs
  confirmation first.
- **Stage 3** (`handover/03-live-attach-prototype.md` once written): attempt
  an actual prototype of live GPU attach/detach on an already-running
  Substrate worker pod (gVisor/microVM), bypassing vanilla DRA's
  pod-creation-time binding. Flagged to the user as genuine, higher-risk
  research scope, not a demo tweak — likely the most open-ended of the
  three stages.
