# GNWES ↔ real Agent Substrate integration

This is the real thing, not a simulation of it: GNWES gating and leasing a
genuine [agent-substrate/substrate](https://github.com/agent-substrate/substrate)
worker pod, with a real Substrate actor repeatedly resuming onto that exact
pod. Verified end-to-end on 2026-11-11 against a local `kind` cluster running
Substrate's real control plane (`ate-system`: `ate-api-server`,
`ate-controller`, `ate-idp-server`, `atelet`, `atenet`, `postgres`, `rustfs`)
built from an unmodified clone of the upstream repo.

## Why a webhook, and why that's still "no Substrate source changes"

Substrate's `WorkerPool` API (`api/infra/k8s/ate/v1/workerpool_types.go`) has
no field for Kubernetes scheduling gates, so a WorkerPool's generated pods
never carry GNWES's `gnwes.io/gpu-capacity` gate for free. Rather than patch
Substrate's source to add one, GNWES registers a `MutatingWebhookConfiguration`
(`../webhook/podgate-webhook.yaml`) — a standard, external Kubernetes
extension point — that injects the gate onto any pod already carrying a
`gnwes.io/gpu-pool` label, which only the GNWES-owned `WorkerPool`'s own
`spec.template.labels` sets. It never touches any other pod in the cluster
(enforced by `objectSelector`) and fails open (`failurePolicy: Ignore`) if the
webhook is ever unavailable. This is the same answer our earlier research
gave for "can GNWES avoid modifying Kubernetes or Substrate itself": yes, by
using the extension points each project already exposes.

`PodGateReconciler` (`internal/controller/podgate_controller.go`) is
unchanged from the fake-pod demo — it doesn't know or care whether the pod in
front of it came from GNWES's own test harness or from a real Substrate
`WorkerPool`'s Deployment. That's the point: the admission mechanism is
generic; only the webhook is Substrate-specific.

## What this proves, concretely

1. Substrate's `WorkerPool` controller creates a pod for `gnwes-counter`.
2. GNWES's webhook adds the scheduling gate on CREATE — confirmed via
   `kubectl get pod ... -o jsonpath='{.status.conditions}'` showing
   `SchedulingGated` before any GNWES controller had reconciled it.
3. `PodGateReconciler` reserves a slot in `gnwes-counter-pool` and removes the
   gate (`"pod admitted: GPU slot reserved ahead of scheduling"`); the pod
   schedules and goes `Running`.
4. A `GPULease` targeting that exact pod binds, inheriting the reservation
   (`pool.Transfer`, no second `Acquire`), and annotates the pod with
   `gnwes.io/gpu-slot` / `gnwes.io/gpu-attached-at` — standing in for a real
   CDI injection.
5. `kubectl-ate resume actor` (a REAL Substrate gRPC call) lands the actor on
   that exact pod — `ACTOR_STATE_RUNNING`, worker pod matches. Suspend and
   resume again: same pod, same already-attached GPU, no further GNWES
   involvement needed — Substrate's own fast resume path does the rest.

## What this is honest about NOT proving

- **No real CDI injection.** The attach step is still a sleep + annotation,
  exactly as in the simulated demo. Wiring a real `cdiinject.IntoBundle` call
  into Substrate's actual resume path would mean touching
  `cmd/ateworker-gvisor/runsc.go`'s `writeSpec` — genuinely Substrate source,
  and genuinely not done here, by us or upstream. That remains the real open
  problem to present, not something quietly worked around.
- **Gating is per-pod, not per-activation.** A worker pod hosts one actor at
  a time but can serve many different actors sequentially across its life
  (suspend one, resume another). GNWES's Kubernetes-level scheduling gate
  only fires once, at pod creation — it gates "does this GPU-flavored warm
  pod get to exist at all," not "does every individual activation onto it get
  re-checked." In practice this is the stronger demo result: GNWES's one-time
  ~150ms attach cost is amortized across however many real, fast
  (observed: as low as 320ms) Substrate resume/suspend cycles that pod serves
  afterward.
- **Pool state is in-memory only.** If the GNWES manager process targeting
  this cluster restarts, `internal/pool.Manager`'s bookkeeping resets — any
  already-Bound `GPULease` CRs stay `Bound` in etcd but the live pool no
  longer actually reflects their slot as leased. Keep one manager instance
  running for the duration of a demo; don't restart it mid-sequence.

## Reproducing this

Prerequisites: a `kind` cluster with Substrate's control plane deployed (see
upstream's own `hack/` and `demos/counter/README.md` — building/pulling those
images is the slow part, budget 20-30+ minutes on a cold cache) and
`demos/counter` deployed as a baseline. `kubectl-ate` does not build natively
on Windows (`internal/imagecache` uses `syscall.Stat_t`); cross-compile it for
linux and run it from a throwaway container on the cluster's Docker network
instead — see the exact pattern in this repo's git history / DEMO.md.

```bash
# GNWES CRDs + webhook, once per cluster
kubectl --context kind-kind apply -f ../crd/bases/
kubectl --context kind-kind apply -f ../webhook/podgate-webhook.yaml

# This integration's own objects
kubectl --context kind-kind apply -f gnwes-counter-pool.yaml
# resolve the ko:// refs in the two .tmpl files the same way demos/counter's
# own deploy path does, then:
kubectl --context kind-kind apply -f <resolved gnwes-counter-workerpool.yaml>
kubectl ate create actor-template -f <resolved gnwes-counter-template.yaml>

# point a GNWES manager at this cluster
kubectl config use-context kind-kind
go run ../../cmd/manager --health-probe-bind-address=:8082

# once the pod is Running, lease it, then exercise it as a real actor
kubectl --context kind-kind apply -f - <<'EOF'
apiVersion: gnwes.io/v1alpha1
kind: GPULease
metadata: {name: gnwes-counter-lease, namespace: ate-demo-counter}
spec: {poolRef: gnwes-counter-pool, targetPod: <the pod name>}
EOF
kubectl-ate create actor gnwes-actor-1 -a ate-demo-counter --template gnwes-counter
kubectl-ate resume actor gnwes-actor-1 -a ate-demo-counter
```
