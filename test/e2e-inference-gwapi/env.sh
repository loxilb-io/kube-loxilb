# Shared by config.sh, validation*.sh, rmconfig.sh and run.sh. Sourced, not run.
#
# Everything here can be overridden from the environment.
E2E_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$E2E_HERE/../.." && pwd)"
WORK="${WORK:-$E2E_HERE/_work}"

CLUSTER="${CLUSTER:-e2e-igw}"
NET="${NET:-e2e-net}"
NET_SUBNET="${NET_SUBNET:-12.12.12.0/24}"
# Docker's own address on the network; .1 is the gateway's.
NET_GATEWAY="${NET_GATEWAY:-12.12.12.254}"
# Containers without a fixed address - the k3d node - are given one from here,
# away from the fixed ones below.
NET_RANGE="${NET_RANGE:-12.12.12.128/25}"
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.33.1-k3s1}"
export KUBECONFIG="${KUBECONFIG_E2E:-$WORK/kubeconfig}"

IGW1_ADDR="${IGW1_ADDR:-12.12.12.1}"
IGW2_ADDR="${IGW2_ADDR:-12.12.12.2}"
OIDC_ADDR="${OIDC_ADDR:-12.12.12.10}"
PG_ADDR="${PG_ADDR:-12.12.12.11}"
IGW_ADDR="${IGW_ADDR:-$IGW1_ADDR}"
# The VIP is the gateway's own address on purpose: fullproxy binds a socket to
# it, and an address loxilb only owns as a /32 rule device cannot be bound
# ("bind failed Cannot assign requested address"), so the rule is programmed
# and nothing ever listens. igw2 gets the same rules and holds the VIP without
# answering for it (igw_vip); only its REST state is checked.
VIP="${VIP:-$IGW1_ADDR}"
VIP_POOL="${VIP_POOL:-defaultPool=$VIP/32}"
POD_CIDR="${POD_CIDR:-10.42.0.0/16}"
SVC_CIDR="${SVC_CIDR:-10.43.0.0/16}"

# The gateway build under test, written by build-igw.sh, and one without the
# credential features for the peer that is swapped between the two.
IGW_IMAGE="${IGW_IMAGE:-$(cat "$WORK/igw-image" 2>/dev/null)}"
IGW_OLD_IMAGE="${IGW_OLD_IMAGE:-ghcr.io/loxilb-io/loxilb-inference-gateway:hc-9092b99e}"
NETTEST_IMAGE="${NETTEST_IMAGE:-ghcr.io/loxilb-io/nettest:latest}"
PG_IMAGE="${PG_IMAGE:-postgres:18.6}"

# The gateways' API-key store. A gateway without one answers every request a
# token or key was verified for with 503 policy_store_unavailable: the QoS
# stage fails closed on an identity it has no store to look up. Same names as
# the gateway's own cicd/ai-jwtauth scenario; the schema and roles come from
# its scripts/aigw-db-bootstrap.sql, which build-igw.sh copies out.
PG_DB=loxilb
PG_OWNER=oamuser
PG_OWNER_PW=oampass
PG_DP_USER=aigwuser
PG_DP_PW=dp-secret-1
PG_MGMT_PW=mgmt-secret-1
PG_BOOTSTRAP="${PG_BOOTSTRAP:-$WORK/igw-db-bootstrap.sql}"

# kube-loxilb under test, and the one before rule ids for the upgrade check.
KLB_REV="${KLB_REV:-HEAD}"
KLB_OLD_REV="${KLB_OLD_REV:-474474c}"

OIDC_ISSUER="http://$OIDC_ADDR/realms/e2e"
OIDC_AUDIENCE="${OIDC_AUDIENCE:-kube-e2e}"

GIE_VERSION="${GIE_VERSION:-v1.6.0}"
GWAPI_VERSION="${GWAPI_VERSION:-v1.5.1}"

rest() { echo "http://$1:11111/netlox/v1"; }
# Images are tagged by commit, so a rerun at the same commit reuses them.
klb_tag() { echo "e2e-$(git -C "$REPO" rev-parse --short=7 "$1")"; }
klb_image() { echo "ghcr.io/loxilb-io/kube-loxilb:$(klb_tag "$1")"; }
node_container() { echo "k3d-$CLUSTER-server-0"; }
node_addr() {
  docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" "$(node_container)" 2>/dev/null
}

# run_igw <name> <address> <image> - a gateway container on the network, using
# the API-key store. Older builds start a shell by default, so the entrypoint
# is always named.
run_igw() {
  docker rm -f "$1" >/dev/null 2>&1
  docker run -d --name "$1" --network "$NET" --ip "$2" --privileged --cap-add SYS_ADMIN \
    -v "$WORK/aikey_password:/opt/e2e/aikey_password:ro" \
    --entrypoint /root/loxilb-io/loxilb/loxilb "$3" \
    --aikey-db-host "$PG_ADDR" --aikey-db-user "$PG_DP_USER" --aikey-db-name "$PG_DB" \
    --aikey-db-password-file /opt/e2e/aikey_password >/dev/null || return 1
  igw_vip "$1" "$2" && igw_routes "$1" && wait_rest "$2"
}

# igw_vip <name> <address> - a gateway that is not at the VIP holds it on lo,
# without answering ARP for it. A gateway refuses a fullproxy rule whose
# address it cannot bind (unless it is a cluster standby, which kube-loxilb
# without --setRoles does not make it), so every peer has to hold the VIP;
# traffic still reaches only the one at it. This is each gateway holding the
# VIP as with BGP anycast, with the announcement left out.
igw_vip() {
  [[ "$2" == "$VIP" ]] && return 0
  in_netns "$1" sysctl -qw net.ipv4.conf.all.arp_ignore=1 net.ipv4.conf.all.arp_announce=2 &&
    in_netns "$1" ip addr replace "$VIP/32" dev lo
}

# igw_routes <name> - the pool endpoints are pod addresses and the Endpoint
# Picker is reached at its cluster IP, so the gateway needs a way into both
# through the node; without it the rule is programmed and every endpoint is
# unreachable. Set from the host: the gateway image has no ip(8).
igw_routes() {
  local node pid
  node="$(node_addr)"
  [[ -n "$node" ]] || return 0 # no cluster yet; config.sh calls again
  pid="$(docker inspect -f '{{.State.Pid}}' "$1")" || return 1
  sudo nsenter -t "$pid" -n ip route replace "$POD_CIDR" via "$node" &&
    sudo nsenter -t "$pid" -n ip route replace "$SVC_CIDR" via "$node"
}

# wait_rest <address> - until the gateway answers on its REST port.
wait_rest() {
  local i
  for ((i = 0; i < 90; i++)); do
    curl -sf --max-time 2 "$(rest "$1")/version" >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "loxilb at $1 did not answer on 11111 in 90s"
  return 1
}

# in_netns <container> <cmd...> - run a host command in a container's netns.
in_netns() {
  local pid
  pid="$(docker inspect -f '{{.State.Pid}}' "$1")" || return 1
  shift
  sudo nsenter -t "$pid" -n "$@"
}
