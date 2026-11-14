#!/usr/bin/env bash
# Turns on the three Kubernetes feature gates Agent Substrate's
# podcertificate-controller / pod-identity system needs
# (ClusterTrustBundle, ClusterTrustBundleProjection, PodCertificateRequest),
# plus the certificates.k8s.io/v1beta1 runtime-config entry, on a real
# kubeadm-managed control-plane node. Confirmed OFF by default on this
# project's real cluster (server1, Kubernetes v1.35.0) -- see
# config/substrate-integration/INSTALL-ON-REAL-CLUSTER.md.
#
# Idempotent: safe to re-run. Each edit only applies if the exact line this
# script expects is still present and the target gate/entry is still
# missing; otherwise it stops with an error rather than guessing, so a
# manifest that has since changed (a cluster upgrade, someone else's edit)
# never gets silently corrupted.
#
# Run as: sudo bash scripts/enable-substrate-feature-gates.sh
# Run with DRY_RUN=1 to see the diffs without changing anything:
#   sudo DRY_RUN=1 bash scripts/enable-substrate-feature-gates.sh

set -euo pipefail

APISERVER_MANIFEST="${APISERVER_MANIFEST:-/etc/kubernetes/manifests/kube-apiserver.yaml}"
CONTROLLER_MANAGER_MANIFEST="${CONTROLLER_MANAGER_MANIFEST:-/etc/kubernetes/manifests/kube-controller-manager.yaml}"
KUBELET_CONFIG="${KUBELET_CONFIG:-/var/lib/kubelet/config.yaml}"

DRY_RUN="${DRY_RUN:-0}"
STAMP="$(date +%Y%m%d%H%M%S)"

if [[ "${REQUIRE_ROOT:-1}" == "1" ]] && [[ "$(id -u)" -ne 0 ]]; then
  echo "must run as root (sudo)" >&2
  exit 1
fi

# apply_edit FILE OLD_LINE NEW_LINE DESCRIPTION
# Replaces OLD_LINE with NEW_LINE in FILE, but only if OLD_LINE is present
# verbatim and NEW_LINE is not already present. Shows a diff; under DRY_RUN
# only shows it. Backs up FILE before writing.
apply_edit() {
  local file="$1" old="$2" new="$3" desc="$4"

  if grep -qF -- "$new" "$file"; then
    echo "[skip] $desc: already present in $file"
    return 0
  fi
  if ! grep -qF -- "$old" "$file"; then
    echo "[ERROR] $desc: expected line not found in $file -- refusing to guess." >&2
    echo "        expected to find exactly: $old" >&2
    echo "        check the file by hand before re-running." >&2
    exit 1
  fi

  local tmp
  tmp="$(mktemp)"
  sed "s#${old}#${new}#" "$file" > "$tmp"

  echo "----- $desc: diff for $file -----"
  diff -u "$file" "$tmp" || true
  echo "---------------------------------"

  if [[ "$DRY_RUN" == "1" ]]; then
    echo "[dry-run] $desc: not applied"
    rm -f "$tmp"
    return 0
  fi

  cp -a "$file" "${file}.bak.${STAMP}"
  mv "$tmp" "$file"
  echo "[applied] $desc (backup: ${file}.bak.${STAMP})"
}

# Worker nodes (e.g. dcn-gpu-0) have no kube-apiserver/kube-controller-manager
# static pods at all -- only kubelet. Confirmed the hard way (2026-11-12):
# the same missing-feature-gate symptom ("ClusterTrustBundle projection is
# not supported in static kubelet mode") hits every node independently,
# since each node's kubelet validates its own feature gates. Skip the
# control-plane-only sections there instead of erroring, so this one script
# is safe to run, unmodified, on any node in the cluster.
if [[ -f "$APISERVER_MANIFEST" ]]; then
  echo "== kube-apiserver.yaml: feature gates =="
  apply_edit "$APISERVER_MANIFEST" \
    "    - --feature-gates=DynamicResourceAllocation=true,DRAConsumableCapacity=true" \
    "    - --feature-gates=DynamicResourceAllocation=true,DRAConsumableCapacity=true,ClusterTrustBundle=true,ClusterTrustBundleProjection=true,PodCertificateRequest=true" \
    "apiserver feature-gates"

  echo "== kube-apiserver.yaml: runtime-config =="
  apply_edit "$APISERVER_MANIFEST" \
    "    - --runtime-config=resource.k8s.io/v1beta1=true" \
    "    - --runtime-config=resource.k8s.io/v1beta1=true,certificates.k8s.io/v1beta1=true" \
    "apiserver runtime-config"

  echo "== kube-apiserver.yaml: per-resource runtime-config =="
  # A blanket --runtime-config=certificates.k8s.io/v1beta1=true does NOT serve
  # ClusterTrustBundle/PodCertificateRequest -- confirmed the hard way
  # (2026-11-12): kube-apiserver logs "Skipping API certificates.k8s.io/v1beta1
  # because it has no resources" even with the feature gates on. These beta
  # APIs need the specific resource path enabled, not just the group/version.
  # Both v1alpha1 and v1beta1 variants are added since it's harmless to enable
  # a resource path this build doesn't serve under one of the two.
  apply_edit "$APISERVER_MANIFEST" \
    "    - --runtime-config=resource.k8s.io/v1beta1=true,certificates.k8s.io/v1beta1=true" \
    "    - --runtime-config=resource.k8s.io/v1beta1=true,certificates.k8s.io/v1beta1=true,certificates.k8s.io/v1alpha1/clustertrustbundles=true,certificates.k8s.io/v1alpha1/podcertificaterequests=true,certificates.k8s.io/v1beta1/clustertrustbundles=true,certificates.k8s.io/v1beta1/podcertificaterequests=true" \
    "apiserver per-resource runtime-config"
else
  echo "== kube-apiserver.yaml: not present on this node, skipping (worker node) =="
fi

if [[ -f "$CONTROLLER_MANAGER_MANIFEST" ]]; then
  echo "== kube-controller-manager.yaml: feature gates =="
  apply_edit "$CONTROLLER_MANAGER_MANIFEST" \
    "    - --feature-gates=DynamicResourceAllocation=true,DRAConsumableCapacity=true" \
    "    - --feature-gates=DynamicResourceAllocation=true,DRAConsumableCapacity=true,ClusterTrustBundle=true,ClusterTrustBundleProjection=true,PodCertificateRequest=true" \
    "controller-manager feature-gates"
else
  echo "== kube-controller-manager.yaml: not present on this node, skipping (worker node) =="
fi

# apply_insert_after FILE ANCHOR_LINE NEW_LINE DESCRIPTION
# Inserts NEW_LINE right after the first occurrence of ANCHOR_LINE, but only
# if ANCHOR_LINE is present verbatim and NEW_LINE is not already present
# anywhere in the file. Uses awk instead of sed so a NEW_LINE never has to
# be smuggled through sed's s/// as an embedded literal newline.
apply_insert_after() {
  local file="$1" anchor="$2" new="$3" desc="$4"

  if grep -qF -- "$new" "$file"; then
    echo "[skip] $desc: already present in $file"
    return 0
  fi
  if ! grep -qF -- "$anchor" "$file"; then
    echo "[ERROR] $desc: expected anchor line not found in $file -- refusing to guess." >&2
    echo "        expected to find exactly: $anchor" >&2
    exit 1
  fi

  local tmp
  tmp="$(mktemp)"
  awk -v anchor="$anchor" -v newline="$new" '
    { print }
    $0 == anchor { print newline }
  ' "$file" > "$tmp"

  echo "----- $desc: diff for $file -----"
  diff -u "$file" "$tmp" || true
  echo "---------------------------------"

  if [[ "$DRY_RUN" == "1" ]]; then
    echo "[dry-run] $desc: not applied"
    rm -f "$tmp"
    return 0
  fi

  cp -a "$file" "${file}.bak.${STAMP}"
  mv "$tmp" "$file"
  echo "[applied] $desc (backup: ${file}.bak.${STAMP})"
}

echo "== kubelet config.yaml: featureGates =="
# kubelet validates its own feature gates at startup: ClusterTrustBundleProjection
# requires ClusterTrustBundle to ALSO be set on the kubelet itself, not just on
# the apiserver/controller-manager -- confirmed the hard way (2026-11-12): setting
# only ClusterTrustBundleProjection here panics kubelet on boot with "...is
# enabled, but depends on features that are disabled: [ClusterTrustBundle]",
# which takes down the whole control plane since kubelet manages the static pods.
apply_insert_after "$KUBELET_CONFIG" \
  "featureGates:" \
  "  ClusterTrustBundle: true" \
  "kubelet featureGates (ClusterTrustBundle)"
apply_insert_after "$KUBELET_CONFIG" \
  "featureGates:" \
  "  ClusterTrustBundleProjection: true" \
  "kubelet featureGates (ClusterTrustBundleProjection)"
# kubelet also needs its own copy of PodCertificateRequest=true to start its
# "PodCertificateRequest manager" (the component that actually serves a pod's
# `projected.sources[].podCertificate` volume mount) -- setting this only on
# the apiserver/controller-manager is not enough. Confirmed the hard way
# (2026-11-12): without it, mounting that volume fails with "unimplemented",
# which blocked Substrate's bundled Postgres StatefulSet (and would block any
# pod using Substrate's pod-identity system) from ever starting. Note: the
# kubelet log for this condition says '...or the PodCertificateProjection
# feature gate is disabled' -- that exact gate name is NOT recognized by this
# kubelet's --help output; the real, verified fix is PodCertificateRequest.
apply_insert_after "$KUBELET_CONFIG" \
  "featureGates:" \
  "  PodCertificateRequest: true" \
  "kubelet featureGates (PodCertificateRequest)"

if [[ "$DRY_RUN" == "1" ]]; then
  echo
  echo "Dry run only -- nothing was changed, kubelet was not restarted."
  exit 0
fi

echo
echo "Static pod manifests changed: kubelet detects this on its own and"
echo "restarts kube-apiserver / kube-controller-manager automatically --"
echo "no action needed for those two."
echo "The kubelet config change needs an explicit restart:"
echo "  sudo systemctl restart kubelet"
read -r -p "Restart kubelet now? [y/N] " reply
if [[ "$reply" =~ ^[Yy]$ ]]; then
  systemctl restart kubelet
  echo "kubelet restarted."
else
  echo "Skipped. Run 'sudo systemctl restart kubelet' yourself when ready --"
  echo "the kubelet feature gate (ClusterTrustBundleProjection) won't take"
  echo "effect until it does."
fi

echo
echo "Verify once the control plane settles (may take ~30-60s):"
echo "  kubectl explain clustertrustbundle"
echo "  (should describe the resource instead of erroring)"
