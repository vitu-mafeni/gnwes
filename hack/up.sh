#!/usr/bin/env bash
# Creates the kind cluster, installs the GNWES CRDs, and creates the demo
# namespace. Requires Docker Desktop running (kind needs it to create nodes).
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER_NAME="gnwes-demo"

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  echo "creating kind cluster '$CLUSTER_NAME'..."
  kind create cluster --name "$CLUSTER_NAME" --config kind-config.yaml
else
  echo "kind cluster '$CLUSTER_NAME' already exists, reusing it"
fi

kubectl config use-context "kind-$CLUSTER_NAME"

echo "installing GNWES CRDs..."
kubectl apply -f config/crd/bases/

echo "creating namespace gnwes-demo..."
kubectl create namespace gnwes-demo --dry-run=client -o yaml | kubectl apply -f -

cat <<'EOF'

Cluster is ready. Next:
  1) In one terminal:  go run ./cmd/manager
  2) In another:        go run ./cmd/demo
EOF
