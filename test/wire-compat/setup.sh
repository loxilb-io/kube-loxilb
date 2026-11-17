#!/bin/bash
# Bring up the throwaway cluster the wire-compat A/B runs against.
#
#   ./setup.sh          # create cluster, load endpoint image, apply fixtures
#   CLUSTER=foo ./setup.sh
#
# Idempotent: re-running against an existing cluster just re-applies fixtures.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/_out}"
CLUSTER="${CLUSTER:-wire-compat}"
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.31.5-k3s1}"
EP_IMAGE="${EP_IMAGE:-ghcr.io/loxilb-io/nettest:latest}"

mkdir -p "$OUT"

if ! k3d cluster list "$CLUSTER" >/dev/null 2>&1; then
  echo "==> creating k3d cluster $CLUSTER"
  k3d cluster create "$CLUSTER" --servers 1 --image "$K3S_IMAGE" \
      --k3s-arg "--disable=traefik@server:0" \
      --k3s-arg "--disable=servicelb@server:0" --wait
else
  echo "==> cluster $CLUSTER already exists"
fi

docker image inspect "$EP_IMAGE" >/dev/null 2>&1 || docker pull "$EP_IMAGE"
k3d image import "$EP_IMAGE" -c "$CLUSTER"

k3d kubeconfig get "$CLUSTER" > "$OUT/kubeconfig.yaml"
cat > "$OUT/agent-conf.yaml" <<YAML
clientConnection:
  kubeconfig: $OUT/kubeconfig.yaml
hostProcPathPrefix: "/"
YAML

kubectl --kubeconfig "$OUT/kubeconfig.yaml" apply -f "$HERE/fixtures.yaml"
kubectl --kubeconfig "$OUT/kubeconfig.yaml" -n l1 rollout status deploy/ep --timeout=180s
echo "==> ready. artifacts in $OUT"
