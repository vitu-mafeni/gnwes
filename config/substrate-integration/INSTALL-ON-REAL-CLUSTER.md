# Installing Agent Substrate's control plane on the real server1 / dcn-gpu-0 cluster

This is for Option A: get Substrate's real control plane running on the same
real, kubeadm-managed cluster that already runs HAMi, Volcano, and FluidCR —
so the Shared-tier (HAMi) checkpoint/restore work already built
(`internal/hamilifecycle`) can be end-to-end tested against a real Substrate
actor, not just a fake client.

Everything below comes from reading Agent Substrate's own source
(`D:\codes\agent-substrate`), not from guessing. Where something is a judgment
call rather than an official, documented path, it is marked **(judgment
call)** and explained.

## Confirmed on the real cluster (2026-11-12)

Surveyed directly over SSH, read-only, before changing anything. Replaces the
placeholders below with real values:

- Two nodes: `server1` (control-plane, `10.9.0.13`) and `dcn-gpu-0` (GPU
  worker, `10.9.0.14`). Both **CRI-O 1.35.0**, Kubernetes **v1.35.0** — not
  containerd, so the registry-config path below is CRI-O's, not containerd's.
- Real kubectl context name: **`kubernetes-admin@ml-cluster`**.
- A registry already exists and is already trusted as insecure/HTTP by every
  node's CRI-O config (`/etc/containers/registries.conf.d/50-insecure-docker-io.conf`):
  Harbor, reachable in-cluster at **`10.9.0.13:30728`** (NodePort). Use this
  as `KO_DOCKER_REPO`'s host instead of standing up a new registry.
- `cert-manager` is already installed (namespace `cert-manager`, Active) —
  not required by Substrate itself, but it's there regardless.
- `go 1.27.4`, `git` already on `server1`. The `docker` CLI (29.6.1) was
  present but **the daemon was not installed** (only `docker-ce-cli`,
  `docker-buildx-plugin`, `docker-compose-plugin` — no `docker-ce` engine, no
  `containerd.io`). See "Installing Docker's daemon" below before Step 2/3 —
  this is required even though the actual cluster runs CRI-O, purely because
  `ate-setup --kind` hardcodes a `docker info` check
  (`cmd/ate-setup/internal/config/kindcheck.go:33`, gated on `cfg.Kind &&
  isDeployCommand(cmd)` in `cmd/ate-setup/internal/cmd/root.go:140-144`, no
  bypass flag). `ko` itself doesn't need Docker at all — see Step 1.
  **`kubectl-ate` is also not installed yet** — Step 1 covers it.
- Storage classes exist (`longhorn` default, `longhorn-static`, `nfs-csi`) —
  not required by Substrate's base install, but available if ever needed.
- `sudo` is passwordless on `server1`.
- **Feature gates are confirmed OFF, not auto-enabled by v1.35**: checked
  directly (`kubectl explain clustertrustbundle` → "the server doesn't have
  a resource type"), not inferred from release notes. The feature-gate step
  below is required, not optional.
- `fluidcr-system` is healthy: `fluidcr-api`, two `fluidcr-archive-stager`,
  `fluidcr-checkpoint-storage-mount`, all Running.

## The one-sentence version

Substrate's own install tool, `ate-setup`, does not care whether the cluster
is `kind` or a real cluster — it just talks to whatever `kubectl` context you
give it. The only Kind-specific thing is a bundle of settings ("the kind
profile") that fills in things a real cluster normally gets from Google Cloud
(image registry, snapshot storage, a small built-in Postgres). We reuse that
same bundle on the real cluster, because it already does — in plain
Kubernetes YAML, nothing kind-tool-specific — exactly what we need: run our
own S3-compatible storage and our own Postgres, instead of needing GCS.

## Prerequisites

Run all of this **directly on `dcn-gpu-0` or `server1`** (not from the
Windows machine). That avoids the whole "`kubectl-ate` doesn't build on
Windows" problem from earlier — on Linux it just builds normally.

1. **Go, git, Docker** — all three already present on `server1`, confirmed.
   Docker is required even though we are not making a `kind` cluster —
   explained below.
2. **A working kubeconfig context** pointing at the real cluster, with
   cluster-admin rights (installing CRDs, webhooks, and namespaces). Confirmed:
   `kubernetes-admin@ml-cluster`.
3. **A container registry every node in the cluster can pull from**, and that
   this machine can push to. Confirmed already present and already trusted:
   Harbor at `10.9.0.13:30728`, listed as an insecure registry in CRI-O's
   config (`/etc/containers/registries.conf.d/50-insecure-docker-io.conf`), so
   no new registry setup is needed. (On a CRI-O node this file, not
   containerd's `/etc/containerd/certs.d/`, is what controls registry trust —
   confirmed by reading it directly on `server1`.)
4. **NOT required, confirmed by reading the source, and already true on this
   cluster regardless:**
   - cert-manager (`grep -rli cert-manager` across the whole Substrate repo:
     zero hits) — already installed on this cluster anyway, for other things.
   - A `StorageClass` / CSI driver (only the optional "external volume" demo
     feature needs one; the base Counter demo's volumes live inside the
     sandbox itself) — `longhorn` exists as the default anyway.
   - Pre-installing gVisor/`runsc` on nodes. `ateworker-gvisor` downloads and
     caches the right `runsc` version itself at runtime
     (`docs/concepts/sandbox-asset-cache.md`).
5. **Kubernetes API server feature gates — confirmed required, not optional.**
   Checked directly against this cluster (`kubectl explain clustertrustbundle`
   fails with "the server doesn't have a resource type"), not inferred from
   version/release notes. Agent Substrate's pod-identity / mTLS system
   (`podcertificate-controller`) depends on:
   - feature gates `ClusterTrustBundle=true`, `ClusterTrustBundleProjection=true`,
     `PodCertificateRequest=true`
   - `runtime-config` `certificates.k8s.io/v1beta1=true`

   Run [`scripts/enable-substrate-feature-gates.sh`](../../scripts/enable-substrate-feature-gates.sh)
   on `server1` (as the user that has `sudo`) instead of hand-editing YAML —
   it's idempotent (safe to re-run, skips lines already present) and prints
   a diff of what it's about to change before changing it:

   ```bash
   sudo bash scripts/enable-substrate-feature-gates.sh
   ```

   This edits the kube-apiserver and kube-controller-manager static pod
   manifests (kubelet picks up the change and restarts those pods
   automatically — no manual restart needed for those two) and the kubelet
   config (this one needs an explicit `sudo systemctl restart kubelet`, which
   the script does at the end). **This briefly restarts the control plane on
   `server1`, the cluster's only control-plane node** — expect a short
   (normally well under a minute) API-server blip, not downtime of running
   workloads.

   **Lessons learned running this for real (2026-11-12), both now fixed in
   the script, documented here so the reasoning isn't lost:**
   - kubelet validates its *own* feature gates at startup, independently of
     the apiserver/controller-manager. Setting only
     `ClusterTrustBundleProjection: true` on the kubelet (without also
     setting `ClusterTrustBundle: true` there) makes kubelet **panic on
     boot** ("...is enabled, but depends on features that are disabled:
     [ClusterTrustBundle]") — and because kubelet manages the static pods,
     that takes down the whole control plane, not just one component. The
     script now sets both on the kubelet.
   - Turning the feature gates on is not enough to actually serve the
     `ClusterTrustBundle`/`PodCertificateRequest` APIs. A blanket
     `--runtime-config=certificates.k8s.io/v1beta1=true` registers zero
     resources (confirmed via `kube-apiserver`'s own log: `Skipping API
     certificates.k8s.io/v1beta1 because it has no resources`). The actual
     switch is per-resource: `certificates.k8s.io/<version>/<resource>=true`
     (e.g. `certificates.k8s.io/v1beta1/clustertrustbundles=true`). The
     script now sets this explicitly for both resources, under both
     `v1alpha1` and `v1beta1` (harmless to enable a path this build doesn't
     use under one of the two).
   - Each time, the control plane recovered cleanly from the
     `.bak.<timestamp>` backups the script makes automatically — restore the
     right one and `sudo systemctl restart kubelet` if anything like this
     happens again.

6. **Confirm KVM, if you also plan to test the Exclusive/VFIO tier later.**
   Already proven real and working on `dcn-gpu-0` earlier in this project
   (`ls -la /dev/kvm`), so nothing new to do here for that part.

## Step 1 — get the Substrate source and `kubectl-ate` onto the Linux box

Confirmed working on `server1` (2026-11-12):

```bash
git clone https://github.com/agent-substrate/substrate.git
cd substrate
go install ./cmd/kubectl-ate
export PATH="$PATH:$(go env GOPATH)/bin"
kubectl-ate --help | head -5
```

(If you already have `D:\codes\agent-substrate` checked out on Windows, a
plain clone on the Linux box is simpler than trying to sync it over — the
repo is public.)

**Do NOT try to `go install` `ko` separately — there is no need to, and the
two obvious-looking import paths for it are both wrong.** `sigs.k8s.io/ko`
isn't a real module, and `ko`'s code doesn't live at
`github.com/kubernetes-sigs/ko` either (both guessed wrong here before
checking). The real answer, confirmed by reading
`cmd/ate-setup/internal/ko/ko.go:66-95`: `ate-setup` pins and resolves `ko`
itself via Go's tool-dependency mechanism — `hack/tools/ko/go.mod` declares
`github.com/google/ko v0.19.1` as a tool, and `ate-setup` runs `go tool -n ko`
from that directory to build/fetch it automatically (from the normal Go
module proxy, not a raw `git clone`) the first time it's needed. Step 3
below fetches it on its own; no separate install step exists or is needed.

## Installing Docker's daemon (needed only for the `--kind` gate check)

**This cluster's own container runtime stays CRI-O the entire time — this
installs a second, independent Docker daemon purely to satisfy
`ate-setup --kind`'s hardcoded check.** They don't share binaries and don't
conflict, with one real exception confirmed on `server1` (2026-11-12):

- This cluster's nodes run a custom `cnlab-runtime` package providing
  `/usr/bin/runc` — **this is the CRIU-patched runc CRI-O/FluidCR's
  checkpoint-restore depends on, present on every cluster in this lab, not
  just this one.** Docker's `containerd.io` package ships its own `runc` at
  the exact same path and will refuse to install (`dpkg: error processing
  archive ...: trying to overwrite '/usr/bin/runc', which is also in package
  cnlab-runtime`) — this is dpkg correctly refusing an unsafe overwrite, not
  a bug to route around with `--force-overwrite` or similar. A plain
  `apt-get install -y docker-ce containerd.io` will hit this every time on a
  node with `cnlab-runtime` installed. **A `path-exclude` dpkg rule does
  NOT fix this** (confirmed) — dpkg's ownership-conflict check runs
  independently of content-exclusion rules. The actual fix is `dpkg-divert`,
  which lets `cnlab-runtime` keep the real path while `containerd.io`'s own
  copy is silently redirected elsewhere; containerd finds the existing
  (`cnlab-runtime`'s) `runc` via `$PATH` on its own, so nothing else needs to
  point at it explicitly:

  ```bash
  sudo dpkg-divert --package cnlab-runtime --rename \
    --divert /usr/bin/runc.containerd-io --add /usr/bin/runc
  sudo apt-get install -y docker-ce containerd.io
  ```

  Verify `/usr/bin/runc`'s timestamp/size is unchanged afterward (still
  `cnlab-runtime`'s) and that `/usr/bin/runc.containerd-io` exists
  separately — confirmed exactly this on `server1`.

- Docker's startup inserts `DOCKER-USER`/`DOCKER-FORWARD` chains at the top
  of `iptables`' `FORWARD` chain, ahead of Cilium's/kube-proxy's own chains.
  Confirmed harmless on this cluster (`server1`, Cilium CNI): `FORWARD`'s
  default policy stayed `ACCEPT`, `net.ipv4.ip_forward` stayed `1`, and all
  pods stayed `Running` before and after — modern Docker's chains pass
  non-Docker traffic through rather than blocking it (unlike very old Docker
  versions' historical reputation for this). Still worth re-checking this
  trio on any cluster before/after, since it's a real (if now rare) class of
  regression: `iptables -L FORWARD -n --line-numbers | head -5`,
  `sysctl net.ipv4.ip_forward`, `kubectl get pods -A | grep -v Running |
  grep -v Completed`.

```bash
sudo systemctl enable --now docker
docker info | head -20
```

## Step 2 — point at the real cluster and the real registry

```bash
export KUBECTL_CONTEXT=kubernetes-admin@ml-cluster
export KO_DOCKER_REPO=10.9.0.13:30728/substrate   # Harbor, already trusted insecure by every node's CRI-O config
```

Sanity-check the context name is still current before trusting it (it's a
name, not a stable ID — confirm rather than assume if time has passed):

```bash
kubectl config get-contexts
```

## Step 3 — install the control plane, reusing the kind profile's storage bundle **(judgment call)**

```bash
go run ./cmd/ate-setup deploy ate-system --kind \
  --context="${KUBECTL_CONTEXT}" \
  --ko-docker-repo="${KO_DOCKER_REPO}" \
  --credential-provider='{"name":"k8s.io"}' \
  --ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/ate-system/sa/atenet-client
```

Why `--kind` on a real cluster: reading
`cmd/ate-setup/internal/config/kindcheck.go`, all `--kind` actually checks
before running is `docker info` succeeding and the named kubectl context
existing — it never checks that the cluster really is a `kind` cluster. What
it *does* do is select the `manifests/ate-install/kind/` overlay
(`cmd/ate-setup/internal/steps/overlay.go`'s `SystemOverlay`), which is plain
Kubernetes YAML — a small bundled Postgres, and a bundled, S3-compatible
`rustfs` snapshot-storage service (`manifests/ate-install/kind/rustfs.yaml`)
wired into `ate-api-server` and `atelet` via
`ATE_STORAGE_BACKEND=s3` + AWS env vars pointing at that in-cluster service.
None of that depends on the actual `kind` tool. It solves exactly the problem
a real, non-GKE cluster has: no Google Cloud Storage to use for snapshots.

The two flags you pass override the only two kind-only defaults that would
otherwise be wrong for a real cluster: `--context` (kind defaults to
`kind-kind`) and `--ko-docker-repo` (kind defaults to `localhost:5001`, which
only works inside kind's own Docker network).

This builds every Substrate component image from source with `ko` and pushes
it to `KO_DOCKER_REPO` — expect this step to take a while on a cold cache
(the repo's own docs budget 20–30+ minutes).

**Verify before continuing:**

```bash
kubectl --context="${KUBECTL_CONTEXT}" -n ate-system get pods
# expect: ate-api-server, ate-controller, ate-idp-server, atelet (DaemonSet),
# atenet-router, postgres-0, rustfs, otel-collector, prometheus — all Running
```

If `rustfs-bucket-init` shows `Completed` and everything else is `Running`,
storage is wired correctly. If `ate-api-server` is crash-looping, check its
logs for `ATE_STORAGE_BACKEND` or `ClusterTrustBundle` errors first — those
are the two things this guide added on top of a plain `ate-setup` run.

## Step 4 — install `kubectl-ate` and deploy the baseline Counter demo

Run natively on the Linux box — no cross-compile wrapper needed here, that
workaround was only for driving things from Windows.

```bash
go install ./cmd/kubectl-ate

go run ./cmd/ate-setup deploy demo counter --context="${KUBECTL_CONTEXT}" \
  --ko-docker-repo="${KO_DOCKER_REPO}"

kubectl-ate create actor my-counter-1 -a ate-demo-counter --template counter --kubeconfig=<path> --context="${KUBECTL_CONTEXT}"
kubectl-ate proxy actor my-counter-1 -a ate-demo-counter
# in a second shell:
curl -X POST -i http://localhost:8000/
```

If this round-trips (counter increments, survives a suspend/resume), the real
control plane is confirmed working end to end on the real cluster.

## Step 5 — wire in GNWES, same pattern already proven on kind

Identical to `config/substrate-integration/README.md`'s existing steps, just
against this cluster's context instead of `kind-kind`:

```bash
kubectl --context="${KUBECTL_CONTEXT}" apply -f ../crd/bases/
kubectl --context="${KUBECTL_CONTEXT}" apply -f ../webhook/podgate-webhook.yaml
```

Then build the actual Shared-tier end-to-end test: a real `ActorTemplate`
whose `WorkerPool` produces a pod running a HAMi-shared GPU container, driven
by `cmd/hami-substrate-bridge`, checkpointed/restored via the real FluidCR
install already on this cluster.

## kubectl-ate proxy's PodCertificateRequest bug, fixed (2026-11-13)

`kubectl-ate proxy` (used to verify an actor end-to-end, e.g. the Counter
demo) failed on this cluster with a sequence of three distinct validation
errors from the live v1.35.0 apiserver, each traced to
`internal/podidentity/podidentity.go`'s v1beta1 fallback path (this cluster
only serves `certificates.k8s.io/v1beta1`, not stable `v1`, for
`PodCertificateRequest`):

1. `spec.pkixPublicKey: Invalid value: null` -- despite this field's own doc
   comment saying it's superseded by (and must be empty when using)
   `stubPKCS10Request`, which the client already sets correctly. The live
   apiserver's validation hasn't caught up with that documented relaxation.
   Fixed by also setting `PKIXPublicKey` (marshaled from the same key used
   for the stub CSR) in the v1beta1 conversion.
2. `spec.proofOfPossession: Invalid value: could not verify proof-of-possession
   signature` -- same deprecated-but-still-required situation for this
   field. Fixed by also computing and setting `ProofOfPossession`.
3. Still failed after (2) with the *same* error, because the first
   implementation attempt signed the pod UID's raw ASCII bytes directly with
   `ecdsa.SignASN1` (literally what the field's doc comment describes). The
   real answer, confirmed via web search (not found in the doc comment
   itself): `ecdsa.VerifyASN1`-based proof-of-possession is checked against
   `sha256(podUID)`, not the raw UID bytes -- the doc omits the implied
   SHA-256 pre-hash step every real caller (including kube-apiserver's own
   verifier) performs. Fixed by hashing the UID before signing.

All three fixes are additive (setting fields that were previously left
unset) and only affect the v1beta1 fallback path -- clusters that serve
stable `v1` PodCertificateRequest are untouched, since that branch doesn't
use any of these deprecated fields.

**Confirmed working end-to-end** after this fix: `kubectl-ate proxy` against
a real Counter actor, 3 consecutive requests, each returning HTTP 200 with
an incrementing counter (`preserved memory count`, `preserved file counter`,
`preserved rootfs counter`) -- genuine proof of working actor state on real
hardware, through the real atunnel/atenet-router mTLS path.

## Open item this does not solve yet (flagging honestly)

Substrate's gVisor `WorkerPool` pods are scheduled by the **default Kubernetes
scheduler**, not Volcano. HAMi's GPU-sharing device plugin and Volcano's
scheduler extensions are what actually divide and place GPU slices on this
cluster today. Confirmed by reading `api/infra/k8s/ate/v1/workerpool_types.go`:
there is no `schedulerName` field, and no generic pod-template-override field,
on the `WorkerPool` CRD — so this cannot be set by writing a different
`WorkerPool` spec.

The likely fix is the same extension point GNWES already uses for the
scheduling gate: the existing `MutatingWebhookConfiguration`
(`../webhook/podgate-webhook.yaml`) fires on pod CREATE for any pod carrying
the `gnwes.io/gpu-pool` label, which only the GNWES-owned `WorkerPool`'s pod
template sets. `schedulerName` and HAMi's GPU-request annotations
(`nvidia.com/gpu`-style resource requests plus HAMi's own annotations) can
both be set at CREATE time by a mutating webhook, same as the scheduling gate
is today. This has **not been implemented or tested yet** — it is the next
concrete step, not a dead end, but it still needs to actually be built and
verified against a real HAMi-partitioned pod before the Shared-tier
end-to-end test can run.
