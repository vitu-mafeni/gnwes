# GNWES ↔ real Kubernetes DRA + CDI

This replaces the simulated "sleep standing in for attach" with the real
mechanism: a genuine `resource.k8s.io/v1` `ResourceClaim`, allocated by the
real Kubernetes scheduler against a real `ResourceSlice`, resolved by a real
DRA kubelet plugin (`cmd/mock-gpu-driver`) that writes a real CDI spec file,
which containerd genuinely injects into the container. Only the device
identity is fake — there is no real NVIDIA GPU in this environment, so the
"device" the CDI spec exposes is a container environment variable
(`GNWES_MOCK_GPU_SLOT`), clearly labeled as a stand-in, not real hardware.

This is additive and separate from the existing simulated-attach demo
(`cmd/manager` + `internal/controller`/`internal/pool`, and the Substrate
integration in `config/substrate-integration`) — nothing there was changed.
It is a second, independent proof that the real DRA+CDI mechanism works, built
directly on `k8s.io/dynamic-resource-allocation` (the same helper library a
real GPU vendor's driver would use), not a replacement for the admission-gate
demo.

## What's real vs mocked

| Layer | This build |
|---|---|
| `DeviceClass`, `ResourceClaimTemplate`, `ResourceClaim` objects | **Real** K8s API objects |
| Scheduling/allocation (pod won't run until a device is actually allocated) | **Real** — native `kube-scheduler` DRA allocation, not a GNWES mechanism |
| `ResourceSlice` publication (how the driver advertises devices) | **Real** — via `k8s.io/dynamic-resource-allocation/resourceslice`, the same helper a real driver uses |
| Kubelet plugin registration + `PrepareResourceClaims`/`UnprepareResourceClaims` gRPC | **Real** — via `k8s.io/dynamic-resource-allocation/kubeletplugin` |
| CDI spec file written to `/etc/cdi` and loaded by containerd | **Real** file, real containerd CDI injection (confirmed: `enable_cdi = true`, `cdi_spec_dirs = ['/etc/cdi', '/var/run/cdi']` on this node by default in containerd 2.x) |
| The device itself | **Mocked.** `ContainerEdits.Env` sets `GNWES_MOCK_GPU_SLOT=<name>` — no real NVIDIA device node, driver, or silicon involved |

## A real gap this surfaced in Agent Substrate

Checked whether a real Substrate `WorkerPool` pod could declare this
`ResourceClaimTemplate` directly (so the GPU-attached warm-pod story from
`config/substrate-integration` could use real DRA instead of GNWES's own
simulated pool). It cannot, today: `WorkerPoolPodTemplate`
(`agent-substrate/substrate`'s `api/infra/k8s/ate/v1/workerpool_types.go`) has
no `resourceClaims` field, and the controller code that maps pod-template
settings onto the generated pod
(`cmd/atecontroller/internal/controllers/workerpool_apply.go`, function
`applyWorkerPoolPodTemplate`, ~lines 583-609) only ever sets
`NodeSelector`/`Tolerations`/`PriorityClassName`/`ServiceAccountName`/
`NodeAffinity` — never `pod.spec.resourceClaims`. This build therefore proves
the DRA+CDI mechanism against a plain GNWES-created test pod
(`test-pod.yaml`), not a real Substrate worker pod. Whether and how to raise
this with Substrate's maintainers is a separate decision, not made here.

## Reproducing this

```bash
kubectl --context kind-kind apply -f deviceclass.yaml
kubectl --context kind-kind apply -f driver.yaml        # namespace, RBAC, DaemonSet
kubectl --context kind-kind apply -f resourceclaimtemplate.yaml
kubectl --context kind-kind apply -f test-pod.yaml

# Confirm the claim allocated and the pod actually runs only once it did:
kubectl --context kind-kind get resourceclaim -n gnwes-dra-demo -o wide
kubectl --context kind-kind get pod gnwes-dra-test -n gnwes-dra-demo

# Confirm the driver's PrepareResourceClaims actually ran:
kubectl --context kind-kind logs -n gnwes-dra-demo -l app=gnwes-mock-gpu-driver

# The real proof: exec into the running container and see the CDI-injected
# env var actually present (not just that objects exist in etcd):
kubectl --context kind-kind exec gnwes-dra-test -n gnwes-dra-demo -- env | grep GNWES_MOCK_GPU
```
