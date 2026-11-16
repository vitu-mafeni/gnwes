# Real-GPU-workload smoke test

Confirms a real, non-ko-built container (the kind an actual CUDA/ML
workload image looks like: a full userland + NVIDIA userspace
libraries) can actually use the hot-attached GPU on the Exclusive
tier -- not just that the device nodes are openable (already confirmed
in `config/vmgpu/README.md`'s 2026-11-16 "Container-level GPU device
access" entry). `nvidia-smi` is the simplest real NVIDIA userspace tool
to check this with.

## Build the image (on `dcn-gpu-0`, or any host with docker + the exact
matching nvidia-smi/library files)

```bash
# find the real library file nvidia-smi dlopen()s at runtime (NOT a
# direct ELF dependency -- `ldd nvidia-smi` never lists it):
REAL_LIB=$(find /usr/lib/x86_64-linux-gnu -maxdepth 1 -name 'libnvidia-ml.so.*' ! -name '*.so.1' | head -1)
echo "REAL_LIB=$REAL_LIB"   # e.g. libnvidia-ml.so.580.178.04

mkdir -p /tmp/nvidia-smi-test && cd /tmp/nvidia-smi-test
cp "$(which nvidia-smi)" nvidia-smi
cp "$REAL_LIB" "$(basename "$REAL_LIB")"
sed "s/REAL_VERSION/${REAL_LIB##*.so.}/" <path-to-gnwes-repo>/config/vmgpu/nvidia-smi-test/Dockerfile > Dockerfile
cp <path-to-gnwes-repo>/config/vmgpu/nvidia-smi-test/entrypoint.sh .

docker build -t gnwes-nvidia-smi-test:local .
```

## Push to this cluster's Harbor (same proven pattern as every other
image push in this project -- plain `docker push` reliably times out
on this registry's large layers)

```bash
HARBOR_IMAGE=10.9.0.13:30728/substrate-test/gnwes-nvidia-smi-test:latest
docker tag gnwes-nvidia-smi-test:local "$HARBOR_IMAGE"
docker save -o ~/gnwes-nvidia-smi-test.tar "$HARBOR_IMAGE"
skopeo copy docker-archive:$HOME/gnwes-nvidia-smi-test.tar docker://"$HARBOR_IMAGE" \
  --dest-tls-verify=false --authfile ~/.docker/config.json
```

## Deploy and run

```bash
sed "s#<resolved Harbor image.*>#${HARBOR_IMAGE}#" \
  config/substrate-integration/gnwes-nvidia-smi-template.yaml.tmpl > /tmp/gnwes-nvidia-smi-template.yaml
kubectl-ate create actor-template -f /tmp/gnwes-nvidia-smi-template.yaml

kubectl-ate create actor gnwes-actor-nvidia-smi-1 -a ate-demo-counter --template gnwes-nvidia-smi
kubectl-ate resume actor gnwes-actor-nvidia-smi-1 -a ate-demo-counter
# find the worker pod it landed on, same as any other actor:
kubectl-ate get actor gnwes-actor-nvidia-smi-1 -a ate-demo-counter

sleep 15
kubectl logs -n ate-demo-counter <worker-pod> -c worker --tail=200 | grep -A30 "nvidia-smi"
```

**Expect**: the worker pod's own log (actor stdout is forwarded there,
labeled `ate.dev/*`) shows `nvidia-smi`'s real table output -- driver
version, GPU name, memory, and (since this is a fresh process with no
other workload on the device) `No running processes found`.
