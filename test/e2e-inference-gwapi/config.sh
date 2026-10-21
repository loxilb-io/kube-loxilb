#!/bin/bash
# Bring up the inference-gateway end-to-end testbed on k3d.
#
#   ./config.sh               # build kube-loxilb (once per commit), wire the testbed, deploy
#   SKIP_BUILD=1 ./config.sh  # fail rather than build a missing kube-loxilb image
#   EPP_SRC=~/src/loxilb-inference-gateway IGW_IMAGE=<gateway with "epp"> ./config.sh
#
# The gateway image comes from build-igw.sh (or IGW_IMAGE). EPP_SRC points at
# a loxilb-inference-gateway checkout; its cmd/epp-fake is built into the
# stand-in Endpoint Picker. Without it the Endpoint Picker checks are skipped.
#
# Topology - the gateways run as containers beside the cluster node, on one
# docker network, which is how the loxilb cicd scenarios model an external LB:
#
#   e2e-net 12.12.12.0/24, host 12.12.12.254
#     igw1      12.12.12.1   gateway under test, REST :11111; the VIP, traffic checks
#     igw2      12.12.12.2   second peer, swapped between builds; REST checks only
#     e2e-oidc  12.12.12.10  stand-in OIDC provider (discovery document and JWKS)
#     e2e-pg    12.12.12.11  PostgreSQL, the gateways' API-key store
#     k3d node  from 12.12.12.128/25; pods 10.42.0.0/16, services 10.43.0.0/16
set -u
source "$(dirname "$0")/env.sh"
HERE="$E2E_HERE"
EPP_SRC="${EPP_SRC:-}"
EPP_IMAGE="${EPP_IMAGE:-e2e-epp-fake:latest}"

mkdir -p "$WORK"
say() { echo "### $*"; }
die() { echo "!!! $*"; exit 1; }

[[ -n "$IGW_IMAGE" ]] || die "no gateway image: run build-igw.sh or set IGW_IMAGE"
[[ -f "$PG_BOOTSTRAP" ]] || die "no API-key store bootstrap script at $PG_BOOTSTRAP: run build-igw.sh or set PG_BOOTSTRAP"
docker image inspect "$IGW_IMAGE" >/dev/null 2>&1 || die "$IGW_IMAGE is not a local image"

# build_klb <rev> - a kube-loxilb image of one commit, built from a clone so
# the working tree does not leak into it.
build_klb() {
  local image src
  image="$(klb_image "$1")" || return 1
  if docker image inspect "$image" >/dev/null 2>&1; then
    echo "reusing $image"
    return 0
  fi
  [[ -z "${SKIP_BUILD:-}" ]] || { echo "$image is missing and SKIP_BUILD is set"; return 1; }
  src="$WORK/klb-src"
  rm -rf "$src"
  git clone -q "$REPO" "$src" && git -C "$src" checkout -q "$(git -C "$REPO" rev-parse "$1")" || return 1
  echo "building $image"
  docker build --build-arg GIT_VERSION="$(klb_tag "$1")" -t "$image" "$src" > "$WORK/build-$(klb_tag "$1").log" 2>&1 || {
    echo "build failed - see $WORK/build-$(klb_tag "$1").log"; return 1; }
}

say "1. kube-loxilb images ($KLB_REV, and $KLB_OLD_REV for the upgrade check)"
build_klb "$KLB_REV" || die "kube-loxilb $KLB_REV"
build_klb "$KLB_OLD_REV" || die "kube-loxilb $KLB_OLD_REV"

say "2. network $NET $NET_SUBNET"
k3d cluster delete "$CLUSTER" >/dev/null 2>&1
docker rm -f igw1 igw2 e2e-oidc e2e-pg >/dev/null 2>&1
docker network rm "$NET" >/dev/null 2>&1
docker network create "$NET" --subnet "$NET_SUBNET" --gateway "$NET_GATEWAY" \
  --ip-range "$NET_RANGE" >/dev/null || die "network $NET"

# The fixed addresses are taken before the cluster is created, so the node
# cannot be given one of them.
say "3. API-key store ($PG_IMAGE)"
docker run -d --name e2e-pg --network "$NET" --ip "$PG_ADDR" \
  -e POSTGRES_USER="$PG_OWNER" -e POSTGRES_PASSWORD="$PG_OWNER_PW" -e POSTGRES_DB="$PG_DB" \
  "$PG_IMAGE" >/dev/null || die "PostgreSQL"
for ((i = 0; i < 60; i++)); do
  docker exec e2e-pg pg_isready -h 127.0.0.1 -U "$PG_OWNER" -d "$PG_DB" >/dev/null 2>&1 && break
  sleep 1
done
docker cp "$PG_BOOTSTRAP" e2e-pg:/tmp/bootstrap.sql >/dev/null || die "bootstrap script"
docker exec -e AIGW_DB_PASSWORD="$PG_DP_PW" -e AIGW_MGMT_DB_PASSWORD="$PG_MGMT_PW" e2e-pg \
  psql -h 127.0.0.1 -U "$PG_OWNER" -d "$PG_DB" -q -f /tmp/bootstrap.sql || die "API-key store bootstrap"
echo "$PG_DP_PW" > "$WORK/aikey_password"

say "4. gateways ($IGW_IMAGE) and the OIDC stub"
run_igw igw1 "$IGW1_ADDR" "$IGW_IMAGE" || die "igw1"
run_igw igw2 "$IGW2_ADDR" "$IGW_IMAGE" || die "igw2"
rm -rf "$WORK/oidc"
python3 "$HERE/oidc-stub.py" init "$WORK/oidc" "$OIDC_ISSUER" || die "OIDC stub keys"
docker run -d --name e2e-oidc --network "$NET" --ip "$OIDC_ADDR" \
  -v "$WORK/oidc/www:/www:ro" -v "$HERE/oidc-serve.sh:/serve.sh:ro" \
  --entrypoint socat "$NETTEST_IMAGE" -T5 tcp-l:80,fork,reuseaddr exec:/serve.sh >/dev/null || die "OIDC stub"

say "5. k3d cluster $CLUSTER ($K3S_IMAGE)"
k3d cluster create "$CLUSTER" --network "$NET" --image "$K3S_IMAGE" --no-lb \
  --k3s-arg "--disable=traefik@server:0" --k3s-arg "--disable=servicelb@server:0" \
  --kubeconfig-update-default=false --kubeconfig-switch-context=false \
  --wait --timeout 180s >/dev/null || die "k3d cluster create"
k3d kubeconfig get "$CLUSTER" > "$KUBECONFIG" || die "kubeconfig"
NODE="$(node_addr)"
case "$NODE" in
  "" | "$IGW1_ADDR" | "$IGW2_ADDR" | "$OIDC_ADDR" | "$PG_ADDR") die "node address '$NODE' is unusable" ;;
esac
until kubectl get nodes 2>/dev/null | grep -q " Ready"; do sleep 3; done
igw_routes igw1 && igw_routes igw2 || die "routes into the cluster"
say "node $NODE"

say "6. images into the cluster"
k3d image import -c "$CLUSTER" "$(klb_image "$KLB_REV")" "$(klb_image "$KLB_OLD_REV")" \
  "$NETTEST_IMAGE" >/dev/null 2>&1 || die "k3d image import"

say "7. CRDs (Gateway API $GWAPI_VERSION, Inference Extension $GIE_VERSION, JWTAuthProfile)"
kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/$GWAPI_VERSION/standard-install.yaml" >/dev/null || die "Gateway API CRDs"
kubectl apply -k "https://github.com/kubernetes-sigs/gateway-api-inference-extension/config/crd?ref=$GIE_VERSION" >/dev/null || die "Inference Extension CRDs"
git -C "$REPO" show "$KLB_REV:manifest/crds/jwt-auth-profile.yaml" | kubectl apply -f - >/dev/null || die "JWTAuthProfile CRD"

say "8. kube-loxilb $(klb_tag "$KLB_REV")"
git -C "$REPO" show "$KLB_REV:manifest/gateway-api/kube-loxilb.yaml" > "$WORK/kube-loxilb.src.yaml" || die "manifest"
KLB_TAG="$(klb_tag "$KLB_REV")" LOXIURL="http://$IGW1_ADDR:11111,http://$IGW2_ADDR:11111" \
  VIP_POOL="$VIP_POOL" python3 "$HERE/gen-kube-loxilb.py" "$WORK/kube-loxilb.src.yaml" > "$WORK/kube-loxilb.yaml" || die "manifest"
kubectl apply -f "$WORK/kube-loxilb.yaml" >/dev/null || die "kube-loxilb"

if [[ -n "$EPP_SRC" ]]; then
  say "8b. stand-in Endpoint Picker image from $EPP_SRC"
  command -v go >/dev/null 2>&1 || die "go is needed to build cmd/epp-fake"
  mkdir -p "$WORK/epp"
  ( cd "$EPP_SRC" && CGO_ENABLED=0 go build -o "$WORK/epp/epp-fake" ./cmd/epp-fake/ ) || exit 1
  printf 'FROM scratch\nCOPY epp-fake /epp-fake\nENTRYPOINT ["/epp-fake"]\n' > "$WORK/epp/Dockerfile"
  docker build -t "$EPP_IMAGE" "$WORK/epp" > "$WORK/epp-build.log" 2>&1 || die "epp-fake image - see $WORK/epp-build.log"
  k3d image import -c "$CLUSTER" "$EPP_IMAGE" >/dev/null 2>&1 || die "k3d image import $EPP_IMAGE"
fi

say "9. workloads and gateway resources"
kubectl apply -f "$HERE/fixtures/model-servers.yaml" >/dev/null
kubectl apply -f "$HERE/fixtures/gatewayclass.yaml" -f "$HERE/fixtures/gateway.yaml" >/dev/null
kubectl -n llm rollout status deploy/vllm-qwen3 --timeout=180s >/dev/null || die "model servers"
kubectl -n kube-system rollout status deploy/kube-loxilb --timeout=180s >/dev/null || die "kube-loxilb rollout"
kubectl apply -f "$HERE/fixtures/inferencepool.yaml" -f "$HERE/fixtures/httproute.yaml" >/dev/null

if [[ -n "$EPP_SRC" ]]; then
  # The picker names one model server for every request, so which pod
  # answered tells whether the request went where the picker said.
  EPP_DEST="$(kubectl -n llm get pods -l app=vllm-qwen3 -o jsonpath='{.items[0].status.podIP}'):8000"
  sed -e "s|__EPP_IMAGE__|$EPP_IMAGE|" -e "s|__EPP_DEST__|$EPP_DEST|" \
    "$HERE/fixtures/epp.yaml" > "$WORK/epp.yaml"
  kubectl apply -f "$WORK/epp.yaml" >/dev/null || exit 1
  kubectl -n llm rollout status deploy/epp --timeout=120s >/dev/null || exit 1
  say "Endpoint Picker names $EPP_DEST"
fi

say "testbed is up (KUBECONFIG=$KUBECONFIG)"
kubectl -n kube-system logs deploy/kube-loxilb 2>/dev/null | grep -m1 "Build:"
