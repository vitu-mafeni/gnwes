# Reproducing the GNWES ↔ real Agent Substrate integration, step by step

This is the single, ordered, copy-paste walkthrough for everything done on
`server1` (`192.168.3.234`, control-plane, no GPU) / `dcn-gpu-0` (`10.9.0.14`
on the cluster's internal WireGuard overlay, GPU worker) on 2026-11-12/13:
installing Agent Substrate's real control plane, fixing every real blocker
hit along the way, and reproducing GNWES's gating/leasing demo against it.

It assumes nothing survived from a previous attempt. If you're resuming
partway through, skip to the relevant phase — each step says what it's
fixing and why, so you can tell whether it still applies.

For the *reasoning* behind each fix (exact error messages, why the fix is
correct, what else was tried and ruled out), see:
- `INSTALL-ON-REAL-CLUSTER.md` — the Substrate control-plane install itself
- `KERNEL-UPGRADE-PLAN.md` — the kernel upgrade specifically
- `README.md` (this directory) — the GNWES integration's own design and honesty

This guide is the condensed, "just the commands that worked" version of all
three.

## Prerequisites

- Two (or more) nodes, Ubuntu 22.04.5, CRI-O as the container runtime,
  `kubeadm`-joined into one cluster. One control-plane node, any number of
  workers.
- `go`, `git`, `docker` CLI on the control-plane node (the daemon gets
  installed in Phase 0 if missing).
- A container registry every node already trusts and can push to. These
  steps assume Harbor at `10.9.0.13:30728`, project `substrate-test` — adjust
  every image reference below if yours differs.
- SSH access to every node, with passwordless `sudo`.
- If any node has NVIDIA GPUs: confirm the driver is DKMS-based
  (`dkms status`) before Phase 1 — this is what makes the kernel upgrade
  survive without breaking the GPU driver.

## Phase 0 — Node prerequisites

Run once per control-plane node (the Docker daemon / `cnlab-runtime` fix is
only needed on whichever node will actually run `ate-setup`/`ko` builds —
typically just the control-plane node).

**0.1 — Install Docker's daemon, working around a real runc conflict.**

If this node runs a custom CRIU-patched runtime package (here:
`cnlab-runtime`, confirmed present on every cluster in this lab) that also
owns `/usr/bin/runc`, a plain `apt-get install docker-ce containerd.io` will
refuse to install (`dpkg: error processing archive ...: trying to overwrite
'/usr/bin/runc', which is also in package cnlab-runtime`) — correctly: do
not force it. Use `dpkg-divert` instead, which keeps `cnlab-runtime`'s
`runc` at the real path and only redirects `containerd.io`'s own copy:

```bash
sudo dpkg-divert --package cnlab-runtime --rename \
  --divert /usr/bin/runc.containerd-io --add /usr/bin/runc
sudo apt-get update
sudo apt-get install -y docker-ce containerd.io
```

Verify `/usr/bin/runc`'s timestamp/size is unchanged afterward (still
`cnlab-runtime`'s):

```bash
ls -la /usr/bin/runc /usr/bin/runc.containerd-io
dpkg -S /usr/bin/runc   # expect: cnlab-runtime: /usr/bin/runc
```

**0.2 — Trust the registry as insecure (HTTP) for Docker specifically.**
CRI-O's own insecure-registry trust (`/etc/containers/registries.conf.d/`)
does not extend to Docker's separate daemon:

```bash
sudo tee /etc/docker/daemon.json <<'EOF'
{
  "insecure-registries": ["10.9.0.13:30728"]
}
EOF
sudo systemctl restart docker
docker login 10.9.0.13:30728   # your Harbor admin credentials
```

**0.3 — Confirm no regression on cluster networking.** Docker's own startup
inserts `DOCKER-USER`/`DOCKER-FORWARD` iptables chains; confirmed harmless on
this cluster (Cilium CNI), but re-check after install:

```bash
sudo iptables -L FORWARD -n --line-numbers | head -5   # policy ACCEPT, unchanged
sysctl net.ipv4.ip_forward                              # = 1, unchanged
kubectl get pods -A | grep -v Running | grep -v Completed  # empty
```

## Phase 1 — Kernel upgrade (needed for any actor to run at all)

**Skip this phase if `uname -r` is already >= 6.7, or >= 6.6.23.** Agent
Substrate's actor rootfs mounting (`internal/imagecache/bundle_linux.go`)
requires the overlayfs `lowerdir+` mount-API option, introduced in Linux
6.7 (backported to 6.6.23), with no fallback. Below that, every actor fails
identically: `while creating overlay superblock: invalid argument`.

Do this **one node at a time**, confirming each one before moving to the
next. See `KERNEL-UPGRADE-PLAN.md` for the full risk assessment (reboot
impact, GPU driver compatibility) before running this on a shared or
GPU-bearing node.

```bash
sudo apt-get update
sudo apt-get install -y linux-image-generic-hwe-22.04 linux-headers-generic-hwe-22.04
dpkg -l | grep linux-image   # confirm the new kernel installed ALONGSIDE the old one, not replacing it
sudo reboot
```

After reboot (wait ~1-2 minutes, longer for a GPU node):

```bash
uname -r   # expect the new 6.8.x (or whatever HWE resolved to)
```

**If a node loses network connectivity after the reboot** (SSH times out,
and `ping` from another node in the cluster also times out): this is not a
hung boot. It happened once on `dcn-gpu-0` and needed physical console
access to diagnose. Root cause: `i40e`'s newer driver version (bundled with
the new kernel) exposes a `phys_port_name` sysfs attribute the old kernel's
`i40e` didn't, so `systemd-udevd` renames interfaces (`enp96s0f0` becomes
`enp96s0f0np0`) — not a driver or hardware fault. Netplan's interface match
then matches nothing, leaving every interface `state DOWN`. From console:

```bash
ip link   # compare actual interface names against /etc/netplan/*.yaml's match keys
sudo cp /etc/netplan/00-installer-config.yaml /etc/netplan/00-installer-config.yaml.bak-pre-rename
sudo sed -i 's/<old-name>/<old-name>npN/' /etc/netplan/00-installer-config.yaml   # per interface, matching what `ip link` showed
sudo netplan apply
```

This is permanent (netplan regenerates `systemd-networkd`'s config from
this file and reapplies it on every boot).

If a GPU node, confirm the driver survived:

```bash
nvidia-smi                      # same driver version, same GPUs listed
dkms status                     # module built for BOTH the old and new kernel version
```

From the control-plane node, confirm the cluster is healthy:

```bash
kubectl get nodes               # all Ready
kubectl -n ate-system get pods  # all Running (expect some restarts from the reboot, settling within ~1 min)
```

## Phase 2 — Install Agent Substrate's control plane

Done once, on the control-plane node.

**2.1 — Get the source and `kubectl-ate`.** `ko` (the image builder) needs
no separate install — `ate-setup` resolves it itself via Go's tool
mechanism (`hack/tools/ko`), fetched from the normal Go module proxy on
first use.

```bash
git clone https://github.com/agent-substrate/substrate.git
cd substrate
go install ./cmd/kubectl-ate
export PATH="$PATH:$(go env GOPATH)/bin"
```

**2.2 — Enable the Kubernetes feature gates Substrate's pod-identity system
needs.** Confirmed off by default on a real kubeadm cluster (unlike kind,
which turns them on in its own bootstrap config). Use the script below —
rehearsed locally, idempotent, safe to re-run, and correctly skips the
control-plane-only steps on worker nodes:

```bash
# scripts/enable-substrate-feature-gates.sh (from this gnwes checkout)
sudo DRY_RUN=1 bash scripts/enable-substrate-feature-gates.sh   # review the diff first
sudo bash scripts/enable-substrate-feature-gates.sh             # say 'y' to the kubelet restart prompt
```

Run the same script on **every node**, including workers — each node's
kubelet validates its own feature gates independently. **Do this one node
at a time**, confirming cluster health before moving to the next:

```bash
kubectl explain clustertrustbundle   # should describe the resource, not error
kubectl get nodes                    # all Ready
```

If this crashes a node's kubelet (`ClusterTrustBundleProjection is enabled,
but depends on features that are disabled: [ClusterTrustBundle]`): the
script already sets both gates together to avoid this. If you hit it
anyway (e.g. editing by hand), restore from the script's own
`.bak.<timestamp>` backup and re-run the script as-is.

**2.3 — Three source patches, needed on every real cluster this was tried
on.** None of these are config — they're real fixes to Agent Substrate's
own Go source, each with its own section in `INSTALL-ON-REAL-CLUSTER.md`
explaining why. Apply all three before building:

1. **`manifests/ate-install/ate-api-server.yaml`** — add a pod-level
   `sysctls` entry so `ate-api-server` can bind `:443` as non-root (the
   container's `NET_BIND_SERVICE` capability grant is not honored on every
   runtime; the sysctl sidesteps the capability check entirely, the same
   fix `atenet-egress.yaml` already uses):
   ```yaml
   spec:
     template:
       spec:
         securityContext:
           sysctls:
           - name: net.ipv4.ip_unprivileged_port_start
             value: "0"
   ```
2. **`cmd/atecontroller/internal/controllers/workerpool_apply.go`** — add
   `"NET_BIND_SERVICE"` to `workerGvisorCapabilities`. Without it, every
   gVisor actor worker crash-loops on `while opening atunnel listener:
   listen tcp :443: bind: permission denied` — a real missing capability
   (dropping to a specific capability list means even UID 0 needs this
   capability explicitly; it isn't implied by running as root).
3. **`internal/podidentity/podidentity.go`** — when falling back to the
   `v1beta1` `PodCertificateRequest` API (real clusters on Kubernetes 1.35
   serve only this, not stable `v1`), also set `PKIXPublicKey` and a
   correctly-constructed `ProofOfPossession` (SHA-256 of the pod UID,
   signed with `ecdsa.SignASN1`) — both are documented as deprecated/optional
   once `StubPKCS10Request` is set, but this cluster's live apiserver still
   requires them. See the file's own comments (added alongside the fix) for
   the exact three validation errors this resolves, in order.

**2.4 — Point at the real cluster and registry, then deploy.** Replace
context/registry with your own. `--sandbox-classes=gvisor` skips microVM
asset staging (a separate, unrelated rustfs/storage issue — not needed for
the Shared/gVisor-tier work this is for):

```bash
export KUBECTL_CONTEXT=<your context>
export KO_DOCKER_REPO=10.9.0.13:30728/substrate-test

go run ./cmd/ate-setup deploy ate-system \
  --context="${KUBECTL_CONTEXT}" \
  --ko-docker-repo="${KO_DOCKER_REPO}" \
  --credential-provider='{"name":"k8s.io"}' \
  --ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/ate-system/sa/atenet-client \
  --sandbox-classes=gvisor \
  --kind
```

(`--kind` is intentional even on a real cluster — confirmed from source,
it only checks `docker info` + the named context; it selects a storage
profile that solves the same problem a non-GKE cluster has: no GCS to use
for snapshots.)

Verify:

```bash
kubectl -n ate-system get pods   # all Running
```

**2.5 — Deploy and verify the Counter demo end to end.**

```bash
go run ./cmd/ate-setup deploy demo counter --context="${KUBECTL_CONTEXT}" --ko-docker-repo="${KO_DOCKER_REPO}"
```

```bash
kubectl-ate create actor my-counter-1 -a ate-demo-counter --template counter --context="${KUBECTL_CONTEXT}"
kubectl-ate proxy actor my-counter-1 -a ate-demo-counter --context="${KUBECTL_CONTEXT}" &
sleep 5
curl -X POST -i http://localhost:8000/
curl -X POST -i http://localhost:8000/
kill %1
```

Expect two `HTTP/1.1 200 OK` responses with an incrementing counter
(`preserved memory count: 1`, then `2`). If the golden-actor step reports a
stale `GoldenActorCrashed` status from before you applied Phase 1/2.3 (the
exact error, actor UID included, doesn't change on retry): the status is
cached, not re-attempted — `delete demo counter` first, then `deploy demo
counter` again for a genuine fresh attempt.

## Phase 3 — GNWES integration

Done from a checkout of this `gnwes` repo. Needs the same `KUBECTL_CONTEXT`.

**As of 2026-11-13 this phase requests a REAL GPU resource** (Backend=Shared;
see "Is this real, or simulated?" below) — `gnwes-counter-workerpool.yaml.tmpl`
requests `nvidia.com/gpu: "1"`. This needs a real NVIDIA device plugin (or
HAMi, for GPU-sharing) actually installed and advertising that resource on
at least one node, or the demo pod stays `Pending` forever at step 3.2. If
your cluster has no GPU at all, remove that resource line (and treat the
demo purely as a scheduling-gate/admission exercise, same as before).

**3.1 — CRDs and the podgate-webhook.**

```bash
kubectl --context="${KUBECTL_CONTEXT}" apply -f config/crd/bases/
```

The webhook's own manifest ships with a `localhost:5001` image reference
(a kind-registry convention) and no real TLS cert — both need resolving for
a real cluster:

```bash
# Fresh self-signed cert matching this cluster's Service DNS name
go run hack/gencert.go gnwes-podgate-webhook.ate-demo-counter.svc bin/webhook-certs

# CGO_ENABLED=0 is required: the Dockerfile's base image
# (gcr.io/distroless/static:nonroot) has no dynamic linker at all, and a
# native (non-cross-compiled) Go build defaults to dynamic linking against
# glibc unless told otherwise -- confirmed the hard way ("exec
# /podgate-webhook: no such file or directory", despite the file existing).
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/podgate-webhook ./cmd/podgate-webhook
chmod +x bin/podgate-webhook

docker build -t 10.9.0.13:30728/substrate-test/gnwes-podgate-webhook:latest -f cmd/podgate-webhook/Dockerfile .
docker push 10.9.0.13:30728/substrate-test/gnwes-podgate-webhook:latest
```

```bash
kubectl --context="${KUBECTL_CONTEXT}" apply -f config/webhook/podgate-webhook.yaml
kubectl --context="${KUBECTL_CONTEXT}" -n ate-demo-counter set image deployment/gnwes-podgate-webhook webhook=10.9.0.13:30728/substrate-test/gnwes-podgate-webhook:latest
```

Patch the `MutatingWebhookConfiguration`'s `caBundle` with the fresh cert
(this has to be done after `apply`, since the shipped manifest's
`caBundle` matches a different, older cert):

```bash
CA_BUNDLE=$(base64 -w0 bin/webhook-certs/tls.crt)
kubectl --context="${KUBECTL_CONTEXT}" patch mutatingwebhookconfiguration gnwes-podgate-webhook \
  --type='json' -p="[{\"op\":\"replace\",\"path\":\"/webhooks/0/clientConfig/caBundle\",\"value\":\"${CA_BUNDLE}\"}]"
```

```bash
kubectl --context="${KUBECTL_CONTEXT}" -n ate-demo-counter rollout status deployment/gnwes-podgate-webhook --timeout=60s
kubectl --context="${KUBECTL_CONTEXT}" -n ate-demo-counter get pods -l app=gnwes-podgate-webhook   # 1/1 Running
```

**3.2 — GNWES's own GPUWorkerPool, and a real Substrate WorkerPool wired to
it.** The two `config/substrate-integration/*.yaml.tmpl` files use `ko://`
references into *Agent Substrate's own module* — resolve them with whatever
digests your Phase 2.5 run already published (reuse them; same source,
already verified working on this cluster):

```bash
kubectl --context="${KUBECTL_CONTEXT}" apply -f config/substrate-integration/gnwes-counter-pool.yaml

sed 's|ko://go.ate.dev/substrate/cmd/ateworker-gvisor|10.9.0.13:30728/substrate-test/ateworker-gvisor@sha256:<your digest>|' \
  config/substrate-integration/gnwes-counter-workerpool.yaml.tmpl > /tmp/gnwes-counter-workerpool.yaml
kubectl --context="${KUBECTL_CONTEXT}" apply -f /tmp/gnwes-counter-workerpool.yaml
```

Confirm the webhook actually gated the new pod before the manager starts:

```bash
POD=$(kubectl --context="${KUBECTL_CONTEXT}" -n ate-demo-counter get pods -l gnwes.io/gpu-pool=gnwes-counter-pool -o jsonpath='{.items[0].metadata.name}')
kubectl --context="${KUBECTL_CONTEXT}" -n ate-demo-counter get pod "$POD" -o jsonpath='{.status.conditions}'
# expect: "reason":"SchedulingGated"
```

**3.3 — Start the GNWES manager** (long-running; background it):

```bash
nohup go run ./cmd/manager --health-probe-bind-address=:8082 > /tmp/gnwes-manager.log 2>&1 &
sleep 5
tail -f /tmp/gnwes-manager.log   # expect "pod admitted: GPU budget ticket reserved ahead of scheduling"
```

```bash
kubectl --context="${KUBECTL_CONTEXT}" -n ate-demo-counter get pod "$POD"   # now Running
```

**3.4 — Lease that exact pod, resolve the ActorTemplate, and resume a real
actor onto it.**

```bash
sed 's|ko://go.ate.dev/substrate/demos/counter|10.9.0.13:30728/substrate-test/counter@sha256:<your digest>|' \
  config/substrate-integration/gnwes-counter-template.yaml.tmpl > /tmp/gnwes-counter-template.yaml
kubectl-ate create actor-template -f /tmp/gnwes-counter-template.yaml --context="${KUBECTL_CONTEXT}"
```

```bash
kubectl --context="${KUBECTL_CONTEXT}" apply -f - <<EOF
apiVersion: gnwes.io/v1alpha1
kind: GPULease
metadata: {name: gnwes-counter-lease, namespace: ate-demo-counter}
spec: {poolRef: gnwes-counter-pool, targetPod: ${POD}}
EOF
kubectl --context="${KUBECTL_CONTEXT}" -n ate-demo-counter get gpulease gnwes-counter-lease -o wide
# expect: PHASE Bound, SLOT = "<the real node name>/nvidia.com/gpu=1" --
# the REAL node and resource Kubernetes' own scheduler bound this pod to,
# not a mock identifier.
```

```bash
kubectl-ate create actor gnwes-actor-1 -a ate-demo-counter --template gnwes-counter --context="${KUBECTL_CONTEXT}"
kubectl-ate resume actor gnwes-actor-1 -a ate-demo-counter --context="${KUBECTL_CONTEXT}"
# expect: STATE ACTOR_STATE_RUNNING, WORKER POD = the exact pod name above
```

That last line is the actual proof: a real Substrate actor, scheduled by a
real Substrate `WorkerPool`, landed on the exact pod GNWES's webhook gated
and its manager leased, holding a REAL `nvidia.com/gpu` resource the
Kubernetes scheduler actually bound — no simulation left in the Shared-tier
chain (next section covers the Exclusive tier and what's still open there).

## Phase 4 — Exclusive tier (real VFIO hot-attach), run for real 2026-11-13

Same cluster, same atespace, a second WorkerPool/GPUWorkerPool pair using
`ateworker-microvm` instead of `ateworker-gvisor`. This section is the
actual, battle-tested procedure from running it end-to-end against the real
`server1`/`dcn-gpu-0` cluster, including every real snag hit along the way
— not the speculative version originally written before it was ever run.
Expect this phase to take real wall-clock time (asset uploads, two separate
Substrate binary rebuilds) even once you know every step.

### 4.0 — one-time node/cluster prerequisites

These are cluster-state fixes, not code changes, but a fresh cluster will
be missing every one of them:

1. **The target GPU must actually be bound to `vfio-pci`, not just
   `driver_override`-flagged.** The boot-time initramfs hook in
   `config/vmgpu/TESTING-WITH-REAL-AGENTS.md` sets `driver_override` but
   does **not** guarantee the `vfio-pci` kernel module itself is loaded at
   the point it runs — confirmed on this exact cluster: the GPU showed
   `driver=none` with `override=vfio-pci` sitting unused. Fix once per
   boot:
   ```bash
   modprobe vfio
   modprobe vfio_iommu_type1
   modprobe vfio-pci
   for dev in <your 4 PCI functions>; do echo "$dev" > /sys/bus/pci/drivers_probe; done
   # confirm: each shows driver=vfio-pci
   ```
2. **The microVM asset bundle must actually be in the object store**, not
   just attempted. Apply `microvm-assets-upload-job.yaml` (now with the
   throttle fix baked in — see its own comments) and confirm it reaches
   `Complete`, not just "ran without erroring" — a large (268MB `rootfs.img`)
   upload against a self-hosted S3-compatible store can fail mid-job in a
   way that still exits the smaller files successfully.
3. **The cluster-wide `microvm` `SandboxConfig` must exist.** `kubectl get
   sandboxconfig -A` must show a `microvm` row, not just `gvisor-default` —
   confirmed missing on this cluster even after the assets were staged (the
   asset Job and the SandboxConfig apply are two separate steps upstream's
   own `ate-setup` normally does together; see the Job's own comments for
   the one-liner to apply it standalone).
4. **`dcn-gpu-0` (or whichever node has the GPU) needs a kubeconfig whose CA
   actually matches the live cluster**, since `cmd/manager` must run there
   (see 4.5) — a stale local one fails with `x509: certificate signed by
   unknown authority`. Simplest fix: copy the control-plane node's own
   kubeconfig over rather than debugging the stale one.

### 4.1 — RBAC + the real VFIOGPUPool

Edit `gnwes-counter-exclusive-vfiopool.yaml`'s `CHANGE-ME` placeholders to
this node's real `nodeName` and PCI path (`lspci -Dnn | grep -i nvidia`,
then `/sys/bus/pci/devices/<address>/`) — this must be a *different* GPU
than whatever Phase 3's Shared-tier `nvidia.com/gpu` device plugin is
already advertising; a server with multiple GPUs may have 1-2 already
claimed by the Shared tier and one set aside and VFIO-bound specifically
for this tier (confirmed on this cluster: 2 GPUs under `nvidia`, a 3rd
cleanly alone in its own IOMMU group for VFIO).

```bash
kubectl apply -f config/webhook/bridge-rbac.yaml
kubectl apply -f gnwes-counter-exclusive-vfiopool.yaml
kubectl apply -f gnwes-counter-exclusive-pool.yaml
```

### 4.2 — the substrate-gpu-bridge image

```bash
CGO_ENABLED=0 go build -o bin/substrate-gpu-bridge ./cmd/substrate-gpu-bridge
# from a real agent-substrate checkout -- CGO_ENABLED=0 here is REQUIRED,
# not optional, even on a real Linux build host: cgo defaults to on, which
# produces a dynamically-linked binary distroless/static can't exec ("no
# such file or directory" despite the file existing -- confirmed the hard
# way, same failure class podgate-webhook itself already hit once):
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/kubectl-ate-linux ./cmd/kubectl-ate
# copy that binary into this repo's bin/ next to substrate-gpu-bridge, then:
docker build --no-cache -t <registry>/substrate-gpu-bridge:latest -f cmd/substrate-gpu-bridge/Dockerfile .
docker push <registry>/substrate-gpu-bridge:latest
```

Point the webhook at it (its own manifest ships a `localhost:5001` default,
which only resolves on whichever single node that registry actually runs
on — if your GPU-bearing worker pods land on a *different* node, as they
must, push to a cluster-wide-reachable registry instead, e.g. this
cluster's Harbor, and override `BRIDGE_IMAGE`):

```bash
kubectl set env deployment/gnwes-podgate-webhook BRIDGE_IMAGE=<registry>/substrate-gpu-bridge:latest -n ate-demo-counter
```

**Always rebuild under `--no-cache`, or at minimum diff the running pod's
`imageID` against what was just pushed, after any webhook/bridge source
change.** Hit a real case on this cluster where a plain `docker build`
silently produced a stale image despite a genuinely different source binary
— not yet root-caused, `--no-cache` reliably avoided it.

### 4.3 — the WorkerPool/ActorTemplate pair

Both `workerImage` (WorkerPool) and `containers[].image` (ActorTemplate)
need a real, resolved image reference — `ko://...` is accepted nowhere
except via an actual `ko build`/`ko resolve` pass; `kubectl-ate create
actor-template` rejects a raw `ko://` string outright with `must be a
well-formed image reference`.

```bash
# from the agent-substrate checkout, same KO_DOCKER_REPO used for the
# Shared tier's ateworker-gvisor image:
export KO_DOCKER_REPO=<registry>/<project>
RESOLVED_WORKER_IMAGE=$(ko build ./cmd/ateworker-microvm)
sed "s#ko://go.ate.dev/substrate/cmd/ateworker-microvm#${RESOLVED_WORKER_IMAGE}#" \
  gnwes-counter-exclusive-workerpool.yaml.tmpl > resolved-workerpool.yaml
kubectl apply -f resolved-workerpool.yaml

# the ActorTemplate reuses whatever image the Shared-tier counter actor
# already resolved to -- no need to ko build it again:
COUNTER_IMAGE=<the image ref your Shared-tier gnwes-counter-template.yaml already resolved to>
sed "s#ko://go.ate.dev/substrate/demos/counter#${COUNTER_IMAGE}#" \
  gnwes-counter-exclusive-template.yaml.tmpl > resolved-template.yaml
kubectl-ate create actor-template -f resolved-template.yaml
```

### 4.4 — if the pod crash-loops on `listen tcp :443: bind: permission denied`

This repo already has a local, unupstreamed patch to agent-substrate's
`cmd/atecontroller/internal/controllers/workerpool_apply.go` adding
`NET_BIND_SERVICE` to the **gVisor** worker's capability list for exactly
this reason — confirmed on this same cluster that the **microVM** worker's
capability list (`workerMicroVMCapabilities`) needed the identical fix,
which it never got. If you see this crash, the fix is already applied to
agent-substrate's source in this project's checkout; what's easy to get
wrong is **which binary to rebuild**: the capability list is applied by
`ate-controller` (the pod-template-constructing control-plane process) when
it builds each pod's `SecurityContext`, not by `ateworker-microvm` itself —
rebuilding the worker image changes nothing here.

```bash
export KO_DOCKER_REPO=<registry>/<project>
RESOLVED_ATECONTROLLER_IMAGE=$(ko build ./cmd/atecontroller)
kubectl set image deployment/ate-controller "*=${RESOLVED_ATECONTROLLER_IMAGE}" -n ate-system
kubectl rollout status deployment/ate-controller -n ate-system
kubectl delete pod -n ate-demo-counter -l gnwes.io/gpu-pool=gnwes-counter-exclusive-pool
```

### 4.5 — run `cmd/manager` on the GPU node itself, not the control plane

`VMGPUAttachmentReconciler` dials cloud-hypervisor's real api-socket as a
plain unix socket path — it needs filesystem access to wherever
`podgate-webhook`'s hostPath mount (`substratebridge.GNWESHostVCVMDir`,
`/var/lib/gnwes/vcvm` by default) actually lives on disk, which only exists
on the node the worker pod is scheduled to. Running `cmd/manager` anywhere
else (e.g. the control-plane node) means it can reconcile every other
GNWES object fine and simply never reach this one socket.

```bash
# on the GPU node, with a kubeconfig whose CA matches this cluster (4.0.4):
cd <gnwes checkout on that node>
go build -o bin/manager ./cmd/manager
KUBECONFIG=<that kubeconfig> ./bin/manager --health-probe-bind-address=:8082
```

### 4.6 — exercise it

Once the pod reaches `Running` with the sidecar attached (`kubectl get pod
<pod> -n ate-demo-counter -o jsonpath='{.spec.containers[*].name}'` lists
both `worker` and `gnwes-substrate-gpu-bridge`), and `cmd/manager` has
already created the `VMGPUAttachment` for it (`kubectl get vmgpuattachment
-n ate-demo-counter` — happens automatically, no manual step):

```bash
kubectl-ate create actor gnwes-actor-exclusive-1 -a ate-demo-counter --template gnwes-counter-exclusive
kubectl-ate resume actor gnwes-actor-exclusive-1 -a ate-demo-counter
# expect: STATE ACTOR_STATE_RUNNING, WORKER POD = the exclusive-tier pod

kubectl get vmgpuattachment -n ate-demo-counter -o wide
# expect: DESIRED Attached, PHASE Attached, a real DEVICE/GUESTBDF, within
# the sidecar's poll interval (default 1s) of the actor reaching RUNNING
kubectl logs <pod-name> -n ate-demo-counter -c gnwes-substrate-gpu-bridge --tail=5
# expect: "actor state ACTOR_STATE_RUNNING -> desiredState Attached apiSocket /var/lib/gnwes/vcvm/<pod>/<actor-uid>/clh-api.sock"
```

**Confirmed live on this cluster (2026-11-13):** the real microVM worker
pod (`ateworker-microvm`) boots a genuine Cloud Hypervisor/Kata guest on
real hardware — `"Micro-VM booted"`, `"kata-agent answered"`, the counter
app actually serving inside it, all from real log lines, not a simulation —
scheduled correctly on the GPU node via the `nodeSelector` fix, with the
bridge sidecar correctly resolving the live actor assignment and writing
the real host-visible api-socket path. See "Is this real, or simulated?"
below for exactly how far past that the real GPU hot-attach itself has
been confirmed as of this writing.

## Is this real, or simulated? Be precise about which parts

**Updated 2026-11-13: the GPUWorkerPool/GPULease mock pool described in the
original version of this section is gone.** `internal/pool/manager.go` no
longer generates `mock-gpu-N` device identities or simulates any timing —
see `internal/pool`'s own package doc comment. Three real mechanisms now
exist, matching GNWES's two-tier design:

| | What it proves | GPU device | Attach mechanism |
|---|---|---|---|
| **`GPUWorkerPool`/`GPULease`, Backend=Shared** (used above) | Real admission-budget control (never over-schedule more GPU-requesting pods than the pool allows) layered on top of Kubernetes' own real GPU scheduling | **Real.** The pod's spec directly requests `nvidia.com/gpu` (or a HAMi virtual-slice resource); Kubernetes' own scheduler and device plugin bind it, the same as any other GPU workload | Nothing to attach — binding happens once, at pod admission. GNWES's lease just confirms and records the real node/resource once the pod reaches `Running` (`internal/controller/gpulease_controller.go`'s `bindShared`) |
| **DRA + CDI** (`config/dra/README.md`) | Real Kubernetes `ResourceClaim` allocation, a real kubelet DRA plugin, real CDI injection | **Real**, verified on a real 3x NVIDIA TITAN RTX node (GPU Operator, CRI-O, CDI enabled) — `nvidia-smi` inside the test pod shows the real, exact GPU allocated | Kubernetes DRA's own allocation, at pod-creation time only — **cannot** attach to an already-running pod (a Kubernetes limitation, confirmed from `gvisor`'s own `nvproxy` source: a static, creation-time-only device registry) |
| **`GPUWorkerPool`/`GPULease`, Backend=Exclusive** + `VFIOGPUPool`/`VMGPUAttachment` (`config/vmgpu/README.md`) | Real, live GPU hot-attach/detach into an **already-running** guest, no recreation, driven by the real actor's real resume/suspend state | **Real** — verified on real hardware (2026-11-11) against a standalone test VM, and **end-to-end through the full `ateworker-microvm`/Kubernetes path on 2026-11-14**: a real worker pod (sandboxClasses: [microvm]), a real actor resumed onto it, and a real hot-attach landing `PHASE=Attached` with a real `deviceID`/`guestBDF` (`gpu-exclusive-0` / `0000:00:08.0`) | A real `PUT /api/v1/vm.add-device` / `vm.remove-device` call against cloud-hypervisor's own REST API (`internal/chclient`), driven by flipping `spec.desiredState` — now on a `VMGPUAttachment` GNWES itself creates per real worker pod (`GPUWorkerPoolReconciler.reconcileExclusive`), flipped by `cmd/substrate-gpu-bridge` (`internal/substratebridge`) polling the real actor's real state, never by `GPULeaseReconciler` itself (see its doc comment for why two writers to that field would fight) |

So both tiers' *control-plane logic* are now real, with no injected delay or
fake device identity anywhere. What's still open is narrower and more
specific than "is this simulated":

- **Shared tier**: real, but bind-once-at-pod-creation is an inherent
  Kubernetes limitation (same one DRA has), not something GNWES's own code
  can work around — see "Known limitations" below.
- **Exclusive tier**: the control-plane chain (`GPUWorkerPool` →
  `VMGPUAttachment` → `cmd/substrate-gpu-bridge` → real `chclient` call) is
  entirely real and wired end-to-end — **updated 2026-11-13**: Substrate's
  own `cmd/ateworker-microvm` (Kata on cloud-hypervisor) was found to
  already exist and ship upstream (confirmed via `pkg/sandboxclass.All()`
  and `docs/reference/sandbox-classes.md` in the real agent-substrate/substrate
  repo — it was never actually missing, just never exercised by this
  integration). `gnwes-counter-exclusive-workerpool.yaml.tmpl` now requests
  `sandboxClasses: [microvm]` + the real `ate.dev/kvm` extended resource,
  and `podgate-webhook` now auto-injects a `cmd/substrate-gpu-bridge`
  sidecar (plus the shared `/run/vc/vm` volume it needs to see
  cloud-hypervisor's api-socket) into every such pod — the one deployment
  step that previously had no automation. **Updated 2026-11-14: a real
  end-to-end hardware run of this exact path has now been performed** — see
  "Known limitations" below for what that run found and fixed (the
  `/dev/vfio` mount, the CRI-O `io.kubernetes.cri-o.Devices` device-cgroup
  annotation, and two unrelated bugs in `internal/chclient` and the
  reconciler's watch predicate) and what it leaves genuinely open.

## Known limitations (honest, as of 2026-11-13)

**Shared tier:**

- **Binding happens once, at pod creation — inherent to Kubernetes, not a
  GNWES shortcut.** A worker pod can serve many different actors
  sequentially across its life (suspend one, resume another); the SAME
  bound GPU resource stays with it across every one of those turns. There
  is no way for GNWES (or DRA, which has the identical limitation) to
  attach a *different* or *additional* real GPU resource to an
  already-running pod via standard Kubernetes scheduling — that dynamism is
  what the Exclusive tier exists for instead.
- **The admission-budget ticket count is in-memory only
  (`internal/pool.Manager`).** If the GNWES manager process restarts, every
  pool's ticket bookkeeping resets to empty — it does NOT resync against
  real pods that are already Running and already holding real GPU
  resources. Until those pods cycle through naturally, the pool can
  under-count real usage and over-admit past `PoolSize`. Keep one manager
  instance running for the duration of a demo sequence; a production
  deployment should resync ticket state from real pod labels/annotations on
  startup (not yet implemented).

**Exclusive tier:**

- **Real end-to-end hardware run of the `sandboxClasses: [microvm]` path:
  DONE, confirmed 2026-11-14.** A real worker pod
  (`gnwes-counter-exclusive-*`, `sandboxClasses: [microvm]`) reached `2/2
  Running` on `dcn-gpu-0`, a real actor (`kubectl-ate`) resumed onto it, and
  its `VMGPUAttachment` reached `phase: Attached` with `deviceID:
  gpu-exclusive-0`, `guestBDF: "0000:00:08.0"`, `attachLatencyMillis: 1739`
  — a real cloud-hypervisor `vm.add-device` call succeeding against its
  real nested-VM guest, not a `cmd/fakevmm` double. Getting there required
  fixing three real bugs (none of them this feature's design, all now
  fixed in source, not left as live cluster patches):
  - `internal/chclient.DeviceConfig.Iommu` was a string enum
    (`IommuOff`/etc); cloud-hypervisor's real API wants a JSON boolean —
    every `AddDevice` call was being rejected with a 400 until fixed.
  - `VMGPUAttachmentReconciler.SetupWithManager` had no watch predicate, so
    every one of its own status-only writes re-triggered itself
    immediately, racing past its own `RequeueAfter`/backoff — fixed via
    `ignoreStatusOnlyUpdates()` (generation-based, so the deletion/
    finalizer path still works).
  - The worker container never had `/dev/vfio` in its mount namespace at
    all (`podgate-webhook` now hostPath-mounts it), and even once mounted,
    the kernel's device cgroup still blocked opening it with `EPERM`.
    `privileged: true` fixes that generically but is a **hard
    incompatibility** for this specific workload: `ateworker-microvm`'s
    own startup check refuses to run under it ("workers must run
    unprivileged" — a side effect of privileged containers sharing the
    host's cgroup namespace under CRI-O). The real fix (confirmed live):
    add `allowed_annotations = ["io.kubernetes.cri-o.Devices"]` *and*
    `allowed_devices = ["/dev/vfio/vfio", "/dev/vfio/<iommu-group>"]` to
    the node's CRI-O config for whichever runtime handler is active (here,
    `runc` — confirmed via no pod on the cluster setting
    `runtimeClassName`, so CRI-O's resolved `default_runtime` applies),
    restart `crio`, and have `podgate-webhook` set the
    `io.kubernetes.cri-o.Devices` pod annotation
    (`$HOSTPATH:$CONTAINERPATH:$MODE`, comma-separated) via the new
    `VFIO_IOMMU_GROUPS` env var. See `config/vmgpu/README.md` for the full
    CRI-O-side setup this now depends on (**not yet automated** — it's a
    one-time node config step, not something `podgate-webhook` can do for
    itself, since it has no access to the node's own `/etc/crio/`).
  - One footgun worth recording for future debugging on this cluster: CRI-O
    loads **every** file under `/etc/crio/crio.conf.d/`, not just
    `*.conf` — a stray `.conf.bak` backup left in that directory gets
    loaded too and can silently override a later edit. Keep config
    backups outside `crio.conf.d/`.
- **The actor→api-socket resolution is now confirmed against a real
  `kubectl-ate get actor` and a real cloud-hypervisor VM's actual
  `/run/vc/vm/<id>/clh-api-restore.sock`** (previously unit-tested only)
  — `cmd/substrate-gpu-bridge` correctly resolved the actor resumed onto
  the fresh pod above and wrote a working `apiSocket` path.
- **The webhook's sidecar injection (`podgate-webhook`) is now confirmed
  live, via a real `MutatingWebhookConfiguration` admission request
  against the real apiserver** (previously verified only via a real
  JSON-Patch apply in a unit test) — the pod above came up with the
  sidecar, the `/run/vc/vm` and `/dev/vfio` mounts, and the
  `io.kubernetes.cri-o.Devices` annotation all correctly injected.
- **`cmd/substrate-gpu-bridge`'s hand-off when Substrate reassigns a
  different actor to the same pod is unverified on real hardware.** The
  logic (re-resolve the actor, write a fresh `apiSocket`) is real and
  unit-tested, but has not been watched against a real second actor
  resuming onto a pod whose first actor already came and went.
- **`internal/vfiopool.Manager`'s device state is also in-memory only**,
  same caveat as the Shared tier's ticket budget: a manager restart forgets
  which real devices were Attached, with no resync against cloud-hypervisor's
  own real device list on startup.

**Both tiers:**

- **Upstream Substrate's own GPU support is itself "being rebuilt" on
  Kubernetes DRA** as of late 2026, tracked only as an open design issue,
  not shipped code — GNWES's own mechanisms exist independently of whatever
  upstream eventually ships.

From today's real-cluster work specifically:

- **The kernel-version requirement (Linux 6.7+) is a hard floor for every
  actor**, not a soft recommendation — there is no fallback path in
  Substrate's own source. Any new node joining a cluster that will run
  Substrate actors needs this checked before it's trusted to schedule them.
- **The three Go source patches in Phase 2.3 are real patches against
  Agent Substrate's own upstream source**, not GNWES's — they are not
  (yet) upstreamed. A fresh `git clone` of `agent-substrate/substrate`
  needs them reapplied; they are not part of this `gnwes` repo's own
  history. Track them against upstream's own issue tracker before relying
  on this being permanent.
- **The interface-renaming kernel-upgrade gotcha (Phase 1) is specific to
  this hardware's NIC** (Intel X722, `i40e` driver) and this exact kernel
  jump (5.15 → 6.8). A different NIC or kernel jump may not hit it at all,
  or may hit a related-but-different naming issue — the fix pattern
  (compare `ip link` against netplan's match keys) generalizes; the exact
  before/after names do not.
- **Registry, context, and image-digest values throughout this guide are
  this cluster's own** (`10.9.0.13:30728`, `kubernetes-admin@ml-cluster`,
  specific `sha256` digests from one build). Reusing this guide on a
  different cluster needs all of these substituted, not just the ones
  marked `<your ...>`.
