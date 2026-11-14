#!/bin/bash
# Starts cmd/manager on a GPU worker node (e.g. dcn-gpu-0) for the real
# Exclusive-tier (VFIO) path. Run as: sudo bash scripts/start-manager.sh
#
# Two real requirements confirmed live (2026-11-14), both easy to get wrong
# when starting this by hand and worth keeping in one place:
#
# 1. Must run as root. ateworker-microvm creates each actor's VM directory
#    under GNWESHostVCVMDir (see internal/substratebridge.GNWESHostVCVMDir)
#    as root:root, mode 0700 -- confirmed via `stat`. A manager process
#    running as a non-root user gets "permission denied" dialing a real
#    actor's api-socket (not ENOENT -- a definitively different failure
#    from vmGone's recovery path in internal/controller/
#    vmgpuattachment_controller.go, which only triggers on a confirmed-gone
#    path, not a merely-inaccessible one).
# 2. Needs KUBECONFIG pointed at a kubeconfig readable by root with a CA
#    that matches the cluster's CURRENT CA. `sudo`'s HOME is /root, not the
#    invoking user's home, so it does NOT pick up ~ubuntu/.kube/config on
#    its own. If this cluster's CA has been rotated more recently than
#    whatever kubeconfig was last placed in /root (or wherever KUBECONFIG
#    below points), every API call fails with "x509: certificate signed by
#    unknown authority" -- confirmed live after this exact cluster's CA
#    rotated 2026-11-03. Fix: copy a CURRENT kubeconfig (e.g. this node's
#    own ~/.kube/config, or server1's /etc/kubernetes/admin.conf) to the
#    path below and keep it in sync with the cluster's CA going forward.
cd "$(dirname "$0")/.." || exit 1
export KUBECONFIG="${KUBECONFIG:-/home/ubuntu/.kube/config}"
nohup ./bin/manager --health-probe-bind-address=:8082 > manager.log 2>&1 < /dev/null &
echo "started cmd/manager (pid $!), logging to $(pwd)/manager.log"
