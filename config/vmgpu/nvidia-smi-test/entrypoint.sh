#!/bin/sh
# Runs once at actor start; output lands in the worker pod's own stdout
# (ateworker-microvm forwards actor container stdout/stderr there,
# labeled ate.dev/*), so `kubectl logs <worker-pod> -c worker` shows it
# -- no HTTP server, no wakeupProbe, no volumes needed for this test.
#
# Retries instead of running nvidia-smi once: confirmed live (2026-11-16)
# that the container starts (and this script runs) only ~0.2s after VM
# boot, but the GPU's PCI hot-attach itself -- a host-side
# cloud-hypervisor vm.add-device driven by the bridge sidecar's poll
# loop, unrelated to how fast the VM/container boots -- only completes
# around 47s after boot, and the guest init wrapper then still needs
# time to modprobe the driver and mknod the device files. A single
# immediate nvidia-smi call will therefore always fail here regardless
# of how well the wrapper itself works; a real workload that doesn't
# touch the GPU in its first instant wouldn't hit this at all.
echo "=== nvidia-smi ==="
i=0
rc=1
while [ "$i" -lt 36 ]; do
  nvidia-smi
  rc=$?
  [ "$rc" -eq 0 ] && break
  i=$((i + 1))
  sleep 5
done
echo "=== end nvidia-smi (exit $rc) ==="
sleep infinity
