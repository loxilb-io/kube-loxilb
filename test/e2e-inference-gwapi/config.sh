#!/bin/bash
# Bring up the inference-gateway end-to-end testbed.
#
#   ./config.sh            # build the image, wire the testbed, deploy
#   SKIP_BUILD=1 ./config.sh
#   EPP_SRC=~/src/loxilb-inference-gateway IGW_IMAGE=<gateway with "epp"> ./config.sh
#
# EPP_SRC points at a loxilb-inference-gateway checkout; its cmd/epp-fake is
# built into the stand-in Endpoint Picker. Without it the Endpoint Picker
# checks are skipped.
#
# Topology - the gateway runs as a container beside the cluster, reached over a
# host bridge, which is how the loxilb cicd scenarios model an external LB:
#
#     k3s node (pods on 10.42.0.0/16)
#         | igwbr 12.12.12.254/24
#         |
#       igw1  12.12.12.1  loxilb-inference-gateway, REST :11111, VIP 12.12.12.100
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
WORK="${WORK:-$HERE/_work}"

IGW_IMAGE="${IGW_IMAGE:-ghcr.io/loxilb-io/loxilb-inference-gateway:latest}"
KLB_TAG="${KLB_TAG:-e2e-igw}"
GIE_VERSION="${GIE_VERSION:-v1.6.0}"
GWAPI_VERSION="${GWAPI_VERSION:-v1.5.1}"
IGW_ADDR="${IGW_ADDR:-12.12.12.1}"
HOST_ADDR="${HOST_ADDR:-12.12.12.254}"
# The VIP is the gateway's own address on purpose: fullproxy binds a socket to
# it, and an address loxilb only owns as a /32 rule device cannot be bound
# ("bind failed Cannot assign requested address"), so the rule is programmed
# and nothing ever listens.
VIP_POOL="${VIP_POOL:-defaultPool=12.12.12.1/32}"
SVC_CIDR="${SVC_CIDR:-10.43.0.0/16}"
EPP_SRC="${EPP_SRC:-}"
EPP_IMAGE="${EPP_IMAGE:-e2e-epp-fake:latest}"

mkdir -p "$WORK"
say() { echo "### $*"; }

say "1. inference gateway container"
docker rm -f igw1 >/dev/null 2>&1
docker run -d --name igw1 --privileged --cap-add SYS_ADMIN "$IGW_IMAGE" >/dev/null || exit 1

say "2. host bridge and veth into the container"
sudo ip link del igwbr 2>/dev/null
sudo ip link add igwbr type bridge && sudo ip addr add "$HOST_ADDR/24" dev igwbr && sudo ip link set igwbr up
sudo ip link del eigw1h 2>/dev/null
sudo ip link add eigw1h type veth peer name eigw1
sudo ip link set eigw1h master igwbr && sudo ip link set eigw1h up

pid=$(docker inspect -f '{{.State.Pid}}' igw1)
sudo mkdir -p /var/run/netns && sudo ln -sf "/proc/$pid/ns/net" /var/run/netns/igw1
sudo ip link set eigw1 netns igw1
sudo ip netns exec igw1 ip link set eigw1 up
sudo ip netns exec igw1 ip addr add "$IGW_ADDR/24" dev eigw1
# The pool endpoints are pod addresses, so the gateway needs a way back into
# the cluster network; without this the rule is programmed and every endpoint
# is unreachable.
sudo ip netns exec igw1 ip route add 10.42.0.0/16 via "$HOST_ADDR"
# The Endpoint Picker is reached at its Service's cluster IP, which kube-proxy
# DNATs on the node - the same requirement any loxilb outside the cluster has.
sudo ip netns exec igw1 ip route add "$SVC_CIDR" via "$HOST_ADDR"
sudo sysctl -qw net.ipv4.ip_forward=1

say "3. k3s"
if [[ ! -f /usr/local/bin/k3s-uninstall.sh ]]; then
  curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server --disable traefik --disable servicelb" K3S_KUBECONFIG_MODE="644" sh - >/dev/null || exit 1
fi
mkdir -p "$HOME/.kube" && sudo cp -f /etc/rancher/k3s/k3s.yaml "$HOME/.kube/config"
sudo chown "$(id -u):$(id -g)" "$HOME/.kube/config"
until kubectl get nodes 2>/dev/null | grep -q " Ready"; do sleep 3; done

say "4. CRDs (Gateway API $GWAPI_VERSION, Inference Extension $GIE_VERSION)"
kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/$GWAPI_VERSION/standard-install.yaml" >/dev/null || exit 1
kubectl apply -k "https://github.com/kubernetes-sigs/gateway-api-inference-extension/config/crd?ref=$GIE_VERSION" >/dev/null || exit 1

say "5. kube-loxilb"
if [[ -z "${SKIP_BUILD:-}" ]]; then
  sudo docker build -t "ghcr.io/loxilb-io/kube-loxilb:$KLB_TAG" "$REPO" > "$WORK/build.log" 2>&1 || {
    echo "build failed - see $WORK/build.log"; exit 1; }
fi
sudo docker save "ghcr.io/loxilb-io/kube-loxilb:$KLB_TAG" | sudo k3s ctr images import - >/dev/null || exit 1

KLB_TAG="$KLB_TAG" LOXIURL="http://$IGW_ADDR:11111" VIP_POOL="$VIP_POOL" \
  python3 "$HERE/gen-kube-loxilb.py" "$REPO/manifest/gateway-api/kube-loxilb.yaml" > "$WORK/kube-loxilb.yaml" || exit 1
kubectl apply -f "$WORK/kube-loxilb.yaml" >/dev/null || exit 1

if [[ -n "$EPP_SRC" ]]; then
  say "5b. stand-in Endpoint Picker image from $EPP_SRC"
  command -v go >/dev/null 2>&1 || { echo "go is needed to build cmd/epp-fake"; exit 1; }
  mkdir -p "$WORK/epp"
  ( cd "$EPP_SRC" && CGO_ENABLED=0 go build -o "$WORK/epp/epp-fake" ./cmd/epp-fake/ ) || exit 1
  printf 'FROM scratch\nCOPY epp-fake /epp-fake\nENTRYPOINT ["/epp-fake"]\n' > "$WORK/epp/Dockerfile"
  sudo docker build -t "$EPP_IMAGE" "$WORK/epp" > "$WORK/epp-build.log" 2>&1 || {
    echo "epp-fake image build failed - see $WORK/epp-build.log"; exit 1; }
  sudo docker save "$EPP_IMAGE" | sudo k3s ctr images import - >/dev/null || exit 1
fi

say "6. workloads and gateway resources"
kubectl apply -f "$HERE/fixtures/model-servers.yaml" >/dev/null
kubectl apply -f "$HERE/fixtures/gatewayclass.yaml" -f "$HERE/fixtures/gateway.yaml" >/dev/null
kubectl -n llm rollout status deploy/vllm-qwen3 --timeout=180s >/dev/null || exit 1
kubectl -n kube-system rollout status deploy/kube-loxilb --timeout=180s >/dev/null || exit 1
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

say "testbed is up"
kubectl -n kube-system logs deploy/kube-loxilb 2>/dev/null | grep -m1 "Build:"
