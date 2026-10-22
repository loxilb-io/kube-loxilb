# Shared by config.sh, validation.sh, rmconfig.sh and run.sh. Sourced, not run.
#
# Everything here can be overridden from the environment.
E2E_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$E2E_HERE/../.." && pwd)"
WORK="${WORK:-$E2E_HERE/_work}"

CLUSTER="${CLUSTER:-e2e-plain}"
NET="${NET:-e2e-plain-net}"
NET_SUBNET="${NET_SUBNET:-13.13.13.0/24}"
NET_GATEWAY="${NET_GATEWAY:-13.13.13.254}"
# The k3d node is given an address from here, away from the fixed ones.
NET_RANGE="${NET_RANGE:-13.13.13.128/25}"
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.33.1-k3s1}"
export KUBECONFIG="${KUBECONFIG_E2E:-$WORK/kubeconfig}"

LLB1_ADDR="${LLB1_ADDR:-13.13.13.1}"
LLB2_ADDR="${LLB2_ADDR:-13.13.13.2}"
# Service addresses are handed out from this pool. The host reaches them
# through llb1, so traffic is checked there; llb2 gets the same rules and only
# its REST state is checked.
VIP_CIDR="${VIP_CIDR:-20.20.20.0/28}"
POD_CIDR="${POD_CIDR:-10.42.0.0/16}"

LOXILB_IMAGE="${LOXILB_IMAGE:-ghcr.io/loxilb-io/loxilb:latest}"
NETTEST_IMAGE="${NETTEST_IMAGE:-ghcr.io/loxilb-io/nettest:latest}"

# The kube-loxilb builds compared: the baseline and the candidate.
KLB_BASE_REV="${KLB_BASE_REV:-upstream/main}"
KLB_CAND_REV="${KLB_CAND_REV:-HEAD}"

rest() { echo "http://$1:11111/netlox/v1"; }
# Images are tagged by commit, so a rerun at the same commit reuses them.
klb_tag() { echo "e2e-$(git -C "$REPO" rev-parse --short=7 "$1")"; }
klb_image() { echo "ghcr.io/loxilb-io/kube-loxilb:$(klb_tag "$1")"; }
node_container() { echo "k3d-$CLUSTER-server-0"; }
node_addr() {
  docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" "$(node_container)" 2>/dev/null
}

# in_netns <container> <cmd...> - run a host command in a container's netns.
in_netns() {
  local pid
  pid="$(docker inspect -f '{{.State.Pid}}' "$1")" || return 1
  shift
  sudo nsenter -t "$pid" -n "$@"
}

# run_llb <name> <address> - an upstream loxilb container on the network.
run_llb() {
  docker rm -f "$1" >/dev/null 2>&1
  docker run -d --name "$1" --network "$NET" --ip "$2" --privileged --cap-add SYS_ADMIN \
    "$LOXILB_IMAGE" >/dev/null || return 1
  llb_routes "$1" && wait_rest "$2"
}

# llb_routes <name> - endpoints of a Service on the pod network are pod
# addresses, so loxilb needs a way into the cluster network through the node.
llb_routes() {
  local node
  node="$(node_addr)"
  [[ -n "$node" ]] || return 0 # no cluster yet; config.sh calls again
  in_netns "$1" ip route replace "$POD_CIDR" via "$node"
}

# wait_rest <address> - until loxilb answers on its REST port.
wait_rest() {
  local i
  for ((i = 0; i < 90; i++)); do
    curl -sf --max-time 2 "$(rest "$1")/config/loadbalancer/all" >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "loxilb at $1 did not answer on 11111 in 90s"
  return 1
}
