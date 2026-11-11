# Handover 2/3 — Draft technical input for upstream issue #752

**Scope decision (2026-11-11):** after Stage 3's research turned up a
precise, sourced root cause for why gVisor can't do the Tier 1 CDI-spec-reuse
trick (nvproxy's static, creation-time-only device registry — see
`research/live-attach-poc/README.md`), the user chose to keep *this* draft
narrowly focused on the data point it already has, rather than fold the
gVisor finding in here. The gVisor root-cause writeup went into its own
draft instead, for the issue it's actually more precisely about:
[`02b-upstream-issue-13621-draft.md`](02b-upstream-issue-13621-draft.md).

**Status: DRAFT WRITTEN, NOT POSTED.** Posting to a public GitHub issue is a
"publish public content" action — it needs the user's explicit go-ahead on
the *exact* final text before anyone posts it, and this session has not done
so. Whoever picks this up (human or AI) should treat the comment below as a
draft to review with the user, not something to paste in unilaterally.

## Background: what issue #752 actually is

Caught up in full via a fetch of the live issue thread (14 comments as of
fetch time). Correcting one imprecision from the Stage 1 docs: #752's
*literal* title is "Select devices per-container in the ActorTemplate" — a
narrower ask than "GPU support via DRA" in general. But the blog post links
it as the GPU/DRA tracking issue, and the comment thread backs that up: it
evolved (comments 11–19, Sep 2026) into exactly the architecture debate
that matters here — whether Substrate's next-gen device model should be
built on classic extended resources (`nvidia.com/gpu: "2"`) or on real
Kubernetes DRA (`resource.k8s.io`).

The specific exchange this draft responds to:

- **@mortent** (Sep 16, 2026) proposed a two-tier design: Tier 1 has
  `WorkerPool` use native DRA directly (add a `resourceClaims` field to
  `WorkerPoolPodTemplate`, mirroring `corev1.PodResourceClaim`); his key
  claim: *"Because CDI specs are generated on the host by the DRA driver,
  `ateom` only needs to consume pre-existing specs,"* which he says
  *"eliminates the need to mount host toolchains and run `nvidia-ctk`."*
- **@eliranw** (same day) pushed back: *"the GPU DRA driver generates its
  specs with the same nvcdi library as the non-DRA driver, so they carry the
  same `nvidia-cdi-hook createContainer` hooks pointing at a host path"* —
  i.e. even a DRA-driver-authored CDI spec still references a host binary
  that has to actually be reachable from wherever the container gets
  created. For Substrate's gVisor/micro-VM sandboxed workers, "wherever the
  container gets created" is not the bare host.

This is precisely the question `cmd/mock-gpu-driver/nvidia.go`'s
`NVIDIA_CDI_MODE=existing` path (built and hardware-verified in this repo
over the last several turns) has real, concrete evidence about — for half of
it, not all of it. That's the shape of the draft below: confirm what we
actually verified, be explicit about the part we didn't.

## The draft comment

Ready to post as-is if the user approves it (as a reply on
[agent-substrate/substrate#752](https://github.com/agent-substrate/substrate/issues/752)),
or to edit first:

> Not a Substrate contributor — came across this via the Nov 2026 Agent
> Substrate blog post, which pointed here for GPU/DRA tracking. The Tier 1
> CDI-spec-reuse question in @mortent's and @eliranw's exchange above looked
> worth a concrete data point, since we happened to have hardware to test
> exactly that half of it.
>
> **What we tested:** a from-scratch
> `k8s.io/dynamic-resource-allocation/kubeletplugin`-based DRA driver
> (unrelated hobby project, not Substrate) against a real 3x NVIDIA TITAN
> RTX node managed by NVIDIA GPU Operator (CDI enabled via `ClusterPolicy`,
> CRI-O 1.35 runtime). Our driver's `PrepareResourceClaims` can do either of:
> - generate its own CDI spec via `nvidia-ctk cdi generate`, or
> - just read and reuse the CDI spec GPU Operator's own device plugin
>   already writes to `/var/run/cdi/k8s.device-plugin.nvidia.com-gpu.json`,
>   and return that spec's device ID
>   (`k8s.device-plugin.nvidia.com/gpu=<uuid>`) directly.
>
> The second path needs **zero** `nvidia-ctk`/toolkit access inside the
> driver — it never calls into the toolkit at all, just an `os.ReadFile` +
> string match to confirm the claimed device's UUID is actually defined in
> that file. Verified end-to-end: a real `ResourceClaim` reached
> `allocated,reserved` against our driver, and `nvidia-smi` inside the
> claiming pod showed exactly the one physical GPU that claim was allocated
> (matching PCI Bus-Id), not the node's other two GPUs. So @mortent's claim
> checks out concretely, at least for a standard (non-sandboxed) CRI-O
> container.
>
> **What we did NOT verify, and think is the actual open question:** our
> test container was a plain Kubernetes pod via CRI-O directly, not a
> gVisor-sandboxed or micro-VM actor worker. @eliranw's point — that the
> spec's hooks still point at a host path that has to be reachable from
> wherever the container is actually created — is exactly the thing we can't
> speak to. For a classic runtime on the bare host, that path already
> resolves (GPU Operator put it there). We don't know whether it resolves
> the same way when the "container" is a gVisor sentry or a micro-VM guest,
> where the hook presumably needs to run inside that sandbox's own mount
> namespace rather than the host's. That seems like the actual crux of
> whether Tier 1's "no host toolchain in the worker" claim holds for
> Substrate's sandbox types specifically, not just for an ordinary pod — and
> we don't have a gVisor/micro-VM setup to test it ourselves.
>
> Happy to share the driver code (~250 lines) if it's a useful minimal
> reference for whoever tests the gVisor/micro-VM side of this.

## What's needed before this can actually be posted

1. **The user's explicit sign-off on this exact text** (or an edited
   version) — required regardless of anything else, per this session's
   standing policy on publishing to public content.
2. **A GitHub identity to post as.** Not currently known whether `gh` in
   this environment is authenticated as an account the user wants attached
   to a public comment on someone else's open-source repo — confirm before
   using it.
3. **The "happy to share the driver code" offer currently has nothing to
   point at.** `D:\codes\gnwes` has no `git remote` configured — it's
   local-only. If the user wants to make that offer concrete (a link, not
   just a verbal offer), the repo (or just `cmd/mock-gpu-driver/`) would
   need to be pushed somewhere public first. That's a separate decision —
   don't do it as a side effect of posting this comment. If declined, the
   comment reads fine without a link; drop the last paragraph or soften it
   to "can describe it further in a follow-up" instead.

## Next stage

Stage 3 (`handover/03-live-attach-prototype.md`): attempt a real prototype
of attaching/detaching a device on an already-running, reused Substrate
worker pod — the harder problem this comment explicitly does NOT attempt to
solve. Flagged to the user up front as genuine open-ended research scope.
