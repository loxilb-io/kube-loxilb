#!/bin/bash
# Bring up the plain-loxilb testbed on k3d: two upstream loxilb instances
# beside a one-node cluster. kube-loxilb itself is deployed by validation.sh,
# once per build compared.
#
#   ./config.sh
#   SKIP_BUILD=1 ./config.sh   # fail rather than build a missing kube-loxilb image
#
# Topology - one docker network:
#
#   e2e-plain-net 13.13.13.0/24, host 13.13.13.254
#     llb1      13.13.13.1   loxilb; the host routes the Service pool 20.20.20.0/28 here
#     llb2      13.13.13.2   loxilb; replaced and cut off by the checks, REST only
#     k3d node  from 13.13.13.128/25; pods 10.42.0.0/16
set -u
source "$(dirname "$0")/env.sh"
HERE="$E2E_HERE"
mkdir -p "$WORK"
say() { echo "### $*"; }
die() { echo "!!! $*"; exit 1; }

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

say "1. kube-loxilb images ($KLB_BASE_REV and $KLB_CAND_REV)"
build_klb "$KLB_BASE_REV" || die "kube-loxilb $KLB_BASE_REV"
build_klb "$KLB_CAND_REV" || die "kube-loxilb $KLB_CAND_REV"

say "2. network $NET $NET_SUBNET"
k3d cluster delete "$CLUSTER" >/dev/null 2>&1
docker rm -f llb1 llb2 >/dev/null 2>&1
docker network rm "$NET" >/dev/null 2>&1
docker network create "$NET" --subnet "$NET_SUBNET" --gateway "$NET_GATEWAY" \
  --ip-range "$NET_RANGE" >/dev/null || die "network $NET"

# The fixed addresses are taken before the cluster is created, so the node
# cannot be given one of them.
say "3. loxilb ($LOXILB_IMAGE)"
run_llb llb1 "$LLB1_ADDR" || die "llb1"
run_llb llb2 "$LLB2_ADDR" || die "llb2"
docker image inspect -f '{{index .RepoDigests 0}}' "$LOXILB_IMAGE"

say "4. k3d cluster $CLUSTER ($K3S_IMAGE)"
k3d cluster create "$CLUSTER" --network "$NET" --image "$K3S_IMAGE" --no-lb \
  --k3s-arg "--disable=traefik@server:0" --k3s-arg "--disable=servicelb@server:0" \
  --kubeconfig-update-default=false --kubeconfig-switch-context=false \
  --wait --timeout 180s >/dev/null || die "k3d cluster create"
k3d kubeconfig get "$CLUSTER" > "$KUBECONFIG" || die "kubeconfig"
NODE="$(node_addr)"
case "$NODE" in
  "" | "$LLB1_ADDR" | "$LLB2_ADDR") die "node address '$NODE' is unusable" ;;
esac
until kubectl get nodes 2>/dev/null | grep -q " Ready"; do sleep 3; done
llb_routes llb1 && llb_routes llb2 || die "routes into the cluster"
# The host sends the Service pool to llb1.
sudo ip route replace "$VIP_CIDR" via "$LLB1_ADDR" || die "route to $VIP_CIDR"
say "node $NODE"

say "5. images into the cluster"
k3d image import -c "$CLUSTER" "$(klb_image "$KLB_BASE_REV")" "$(klb_image "$KLB_CAND_REV")" \
  "$NETTEST_IMAGE" >/dev/null 2>&1 || die "k3d image import"

say "6. workload"
kubectl apply -f "$HERE/fixtures/workload.yaml" >/dev/null || die "workload"
kubectl -n plain rollout status deploy/ep --timeout=180s >/dev/null || die "workload rollout"

say "testbed is up (KUBECONFIG=$KUBECONFIG)"
