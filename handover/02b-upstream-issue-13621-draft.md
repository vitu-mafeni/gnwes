# Handover 2b — Draft technical input for upstream issue #13621

**Status: DRAFT WRITTEN, NOT POSTED.** Same rule as
[`02-upstream-issue-752-draft.md`](02-upstream-issue-752-draft.md): posting
to a public GitHub issue needs the user's explicit sign-off on the exact
final text first.

## Why this exists, and why it's a separate draft from #752's

Stage 3's research (`research/live-attach-poc/README.md`) read gVisor's own
`nvproxy` source while investigating whether the container-level
hot-attach primitive could reach a gVisor-sandboxed actor. It found the
architectural root cause behind
[google/gvisor#13621](https://github.com/google/gvisor/issues/13621)
("nvproxy is incompatible with the NVIDIA DRA driver"): `nvproxy`'s device
registry (`pkg/sentry/devices/nvproxy/nvproxy.go`, `Register()` /
`DeviceInfoFromVFS`) is built once, at sandbox creation, and the device
files the app sees are created only during container/sub-container setup
(`runsc/boot/vfs.go`, `createDeviceFiles`) — there's no mechanism to
register a device into an already-built nvproxy instance at all.

That's precise, useful technical context, but it's a **reading of the
source**, not something tested hands-on against a real gVisor+DRA setup
(no gVisor/microVM environment was available in this session — see
`research/live-attach-poc/README.md`'s "what this did NOT do" section). The
user decided to post it where it's most precisely on-topic — #13621 is
*specifically* about this nvproxy/DRA incompatibility already, with a
maintainer's own diagnosis of the symptom (hardcoded device paths,
bind-mounted from host `/dev` by path) — rather than folding it into the
#752 draft, which is about the (different, broader) per-container
device-selection / Tier-1-vs-Tier-2 architecture question.

## The draft comment

> Been testing a DRA driver's CDI-spec-reuse approach against real
> GPU-Operator hardware (unrelated project, context:
> agent-substrate/substrate#752) and ended up reading
> `pkg/sentry/devices/nvproxy/nvproxy.go` while trying to understand whether
> this issue is a one-off path-handling bug or something more structural.
>
> From the source: `Register()` builds `nvp.regularDevs` and the rest of the
> registry once, and `DeviceInfoFromVFS`'s own doc comment says "If
> DeviceInfoFromVFS returns nil, nvproxy.Register(vfsObj) has not been
> called" — this is one-time setup at sandbox creation, not something
> re-evaluated per access or per container. The device files themselves are
> only created in `createDeviceFiles`/`createDeviceFile`
> (`runsc/boot/vfs.go`), called from sandbox/subcontainer creation.
>
> So beyond the specific symptoms here (ignoring CDI's actual device list in
> favor of `NVIDIA_VISIBLE_DEVICES`, or a nil-panic on an unexpected device
> path), there doesn't look like there's any existing path to reusing a
> device-plugin- or DRA-driver-authored CDI spec with `nvproxy` at all,
> short of either (a) nvproxy gaining a way to actually consult the
> CDI-injected device list at creation time, or (b) some way to register a
> device into an already-built nvproxy instance in the first place, which
> doesn't seem to exist today.
>
> Sharing in case it's useful context for whoever scopes the real fix here —
> and happy to be corrected if there's an extension point I missed; this is
> a source-reading conclusion, not something I've tested against a live
> gVisor+DRA setup myself.

## What's needed before this can actually be posted

Same three items as `02-upstream-issue-752-draft.md`: the user's explicit
sign-off on the exact text, a decision on which GitHub identity posts it,
and (not needed here, since this draft makes no code-sharing offer) nothing
else to resolve.

Worth deciding alongside this: whether to post it at all before someone
with real gVisor expertise can sanity-check the "no existing extension
point" claim — it's phrased with that hedge already, but it's still an
outside contributor telling maintainers their own subsystem has no escape
hatch, based on a source read rather than a conversation with them. Flagging
that judgment call explicitly rather than deciding it here.
