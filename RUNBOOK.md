# GNWES — run it and test it

One place for every command, organized so you can run the fast stuff first
and go deeper only if you want to. Every command block below was actually
run against a live cluster while writing this doc — not copied from memory.

## 0. Check what's already running

This project tends to get left running across sessions (kind clusters and
`cmd/manager` processes are cheap to leave up). Before doing anything, check:

```bash
docker info --format '{{.ServerVersion}}'   # Docker Desktop must be running
kind get clusters                           # expect: gnwes-demo, kind (maybe more)
kubectl config get-contexts
curl -s http://localhost:8081/healthz; echo  # a manager, if one's running
curl -s http://localhost:8082/healthz; echo  # a second manager, if one's running
```

If `kind get clusters` is empty, skip to the per-tier "fresh setup" steps
below. If clusters already exist, each tier's "quick re-check" command lets
you confirm things still work without rebuilding anything.

Three independent tiers live here, each with its own cluster/role:

| Tier | What it proves | Needs |
|---|---|---|
| 1. Simulated demo | The core lease/attach/release mechanics and admission gating | Any plain `kind` cluster |
| 2. Real DRA + CDI | The GPU attach step is a real Kubernetes mechanism, not a sleep | A cluster with DRA (`resource.k8s.io/v1`), e.g. `kindest/node:v1.34+` |
| 3. Real Substrate integration | GNWES gating a real upstream Agent Substrate actor | A cluster running Substrate's control plane (heavy one-time setup) |

---

## 1. Automated tests (no cluster needed for this part)

```bash
cd /d/codes/gnwes
go build ./...
go vet ./...
gofmt -l .                 # should print nothing
go test ./... -v           # 30 tests: internal/pool + internal/controller
```

Expect all tests to pass in a couple of seconds. `-race` needs cgo/a C
compiler — if you don't have one locally (we don't, on this Windows box),
skip it locally and let CI's Linux runners enforce it
(`.github/workflows/ci.yaml`).

---

## 2. Tier 1 — simulated demo

**Fresh setup** (skip if `gnwes-demo` already exists and a manager is healthy
on its context):

```bash
cd /d/codes/gnwes
bash hack/up.sh                      # creates kind cluster "gnwes-demo", installs CRDs
kubectl config use-context kind-gnwes-demo
go run ./cmd/manager                 # leave running in its own terminal
```

**Run the demo** (in another terminal):

```bash
cd /d/codes/gnwes
go run ./cmd/demo -mode=both -actors=8 -pool-size=3
```

Expect output like:

```
pool warm-up (one-time, paid ahead of any actor): 4109ms for 3 slots
  actor 1   slot=mock-gpu-0   attach=191ms    (controller-measured=163ms)
  ...
--- warm (lease attach): n=8  min=190ms  avg=223ms  max=312ms ---
...
--- cold (dedicated slot): n=8  min=4311ms  avg=4534ms  max=4630ms ---
 => leasing from a warm GNWES pool is ~20x faster than provisioning a dedicated GPU slot per actor
```

Try `-gate=false` to see the same run with GNWES's admission gating turned
off (pods schedule immediately, no capacity check) — see `DEMO.md` for the
full flag table and talking points.

**Tear down:** `bash hack/down.sh` (deletes the `gnwes-demo` cluster entirely).

---

## 3. Tier 2 — real DRA + CDI

Needs a cluster where `kubectl api-resources --api-group=resource.k8s.io`
actually lists something (`DeviceClass`, `ResourceClaim`, `ResourceSlice`,
...). Plain `gnwes-demo` (`kindest/node:v1.30.0`) does **not** have this —
`kind-kind` (`kindest/node:v1.37.0`) does. If your own cluster doesn't have
DRA, see Troubleshooting below.

**Quick re-check**, if `kind-kind` and the driver are already up:

```bash
kubectl --context kind-kind get resourceclaim -n gnwes-dra-demo -o wide
kubectl --context kind-kind get pod gnwes-dra-test -n gnwes-dra-demo
kubectl --context kind-kind exec -n gnwes-dra-demo gnwes-dra-test -- env | grep GNWES_MOCK_GPU
```

Expect:

```
GNWES_MOCK_GPU_SLOT=mock-gpu-0
GNWES_MOCK_GPU_NOTE=stand-in-device-not-real-nvidia-hardware
```

That's a REAL CDI injection into a REAL running container — not a log line
claiming it happened.

**Fresh setup** on any cluster with DRA:

```bash
cd /d/codes/gnwes/config/dra
kubectl apply -f deviceclass.yaml
kubectl apply -f driver.yaml              # namespace, RBAC (cluster-scoped — review before applying), DaemonSet
kubectl apply -f resourceclaimtemplate.yaml
kubectl apply -f test-pod.yaml
kubectl get pod gnwes-dra-test -n gnwes-dra-demo -w   # wait for Running
kubectl logs -n gnwes-dra-demo -l app=gnwes-mock-gpu-driver --tail=5
kubectl exec gnwes-dra-test -n gnwes-dra-demo -- env | grep GNWES_MOCK_GPU
```

See `config/dra/README.md` for exactly what's real vs mocked here, and the
Substrate `WorkerPoolPodTemplate` gap this surfaced.

**Testing on a real GPU node instead of mock devices:** `cmd/mock-gpu-driver`
now has a real NVIDIA backend (`GNWES_GPU_BACKEND=nvidia`) that discovers
actual GPUs via `nvidia-smi` and generates a real CDI spec via `nvidia-ctk`
(the real NVIDIA Container Toolkit tool) instead of a hand-written fake one.
Use `config/dra/driver-real-gpu.yaml` + `config/dra/test-pod-real-gpu.yaml`
in place of `driver.yaml`/`test-pod.yaml`, and verify with
`kubectl exec ... -- nvidia-smi` instead of the `env | grep` check above. See
`config/dra/README.md`'s "Running on real GPU nodes" section for the full
walkthrough and prerequisites — **this path is code-reviewed, built, and unit
tested, but not yet run against real hardware**, since none exists in this
project's dev environment; treat your first run on a real GPU node as the
actual test of it.

---

## 4. Tier 3 — real Agent Substrate integration

This is the heaviest one to set up from scratch (building/pulling Substrate's
own images is the slow part — budget 20-30+ minutes on a cold cache). If
`kind-kind` already has the `ate-system` namespace running, skip straight to
the quick re-check.

**Quick re-check:**

```bash
kubectl --context kind-kind get workerpool,gpuworkerpool,gpulease -n ate-demo-counter
# Resume the real actor and see it land on the exact GPU-attached pod (needs
# the cross-compiled kubectl-ate binary — see config/substrate-integration/README.md
# for why, and how it's set up at D:\codes\agent-substrate\bin\):
MSYS_NO_PATHCONV=1 docker run --rm --network kind \
  -v "D:/codes/agent-substrate/bin:/bin2" -w /bin2 debian:bookworm-slim \
  ./kubectl-ate-linux --kubeconfig kubeconfig-internal.yaml \
  resume actor gnwes-actor-1 -a ate-demo-counter
```

Expect `ACTOR_STATE_RUNNING` with a worker pod name matching the one
annotated `gnwes.io/gpu-slot` — i.e. a real Substrate actor really resumed
onto the exact pod GNWES gated and attached. Suspend it again when you're
done poking (`suspend actor` in place of `resume actor`), to leave things
quiescent — don't delete it.

**Fresh setup:** follow `config/substrate-integration/README.md` in full —
it covers cloning upstream, standing up `ate-system`, and deploying the
`gnwes-counter` WorkerPool/ActorTemplate pair. Don't attempt it without
reading that file first; there are a few real gotchas (native Windows build
of `kubectl-ate` doesn't work, containerd image-pull bandwidth is the
long pole, etc.) already solved there.

---

## 5. Concurrency / load test (the real one, not a unit test)

Needs a manager running against whatever context is current. Uses
`gnwes-demo` by default below, but works against any cluster with GNWES's
CRDs installed and a manager running.

```bash
kubectl config use-context kind-gnwes-demo
cd /d/codes/gnwes
go run ./cmd/loadtest -actors=40 -concurrency=10 -pool-size=3
```

Expect:

```
pool "loadtest-pool" ready with 3 slots; racing 40 actors (concurrency=10)
----------------------------------------------------------------
succeeded=40 failed=0 double-booking violations=0
attach latency: min=298.147ms avg=1.952374072s max=7.548198s
RESULT: PASS
```

If you see `failed>0` with a `context deadline exceeded` on pod scheduling
(not on the lease itself), that's usually just a resource-constrained
single-node kind cluster under heavy concurrency, not a GNWES bug — raise
`-concurrency` down or the pod-wait timeout up (see the comment in
`cmd/loadtest/main.go`).

---

## Troubleshooting

- **"address already in use" starting `cmd/manager`** — a manager is likely
  already running from an earlier session (check `tasklist | grep manager`
  on Windows). Either reuse it, or pass a different
  `--health-probe-bind-address=:8083` (and `--metrics-bind-address=0` to
  keep metrics off, the default).
- **Docker Desktop isn't running** — `docker info` will error. Start it and
  wait ~30-60s for the daemon before retrying `kind create cluster`.
- **Your cluster has no DRA** (`kubectl api-resources --api-group=resource.k8s.io`
  is empty) — use Tier 1 (simulated) instead; it needs nothing beyond plain
  Kubernetes + scheduling gates (GA since 1.30). For `kind` specifically,
  recreate with `kindest/node:v1.34+`. For a real/managed cluster, DRA may
  need admin-level feature-gate flags you may not have access to — a
  classic device-plugin (`nvidia.com/gpu` extended resource) is the
  pre-DRA real-hardware option most production clusters already use.
- **`gh`/cluster-scoped `kubectl apply` gets denied** — some actions here
  (the admission webhook, the DRA driver's `ClusterRole`) are cluster-wide
  and deliberately require an explicit human decision; that's expected, not
  a bug. Review the manifest, then apply it yourself.
- **Pods stuck in `SchedulingGated` forever** — means no `cmd/manager` (for
  the webhook-based paths, also no `gnwes-podgate-webhook`) is actually
  running against that cluster/context. Check the health endpoint and
  `kubectl get pods -o jsonpath='{.status.conditions}'` for the gate reason.

## Tearing everything down

```bash
bash hack/down.sh                               # deletes gnwes-demo
kind delete cluster --name kind                 # deletes kind-kind (Substrate + DRA demo)
```

Stop any `cmd/manager` processes you started (Ctrl+C in their terminals, or
on Windows: `taskkill /F /IM manager.exe`).
