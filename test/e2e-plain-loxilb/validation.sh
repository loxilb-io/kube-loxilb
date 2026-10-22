#!/bin/bash
# kube-loxilb against upstream loxilb, end to end, for one kube-loxilb build:
#
#   ./validation.sh <rev>
#
# Starts both loxilb instances afresh, deploys kube-loxilb of that commit with
# its own manifest, and checks what every user of upstream loxilb relies on:
# rules on every instance, traffic through the VIP, endpoint changes, restarts
# of kube-loxilb and of an instance, an instance that stops answering, and
# many Services at once while an instance comes and goes. Leaves no Service.
#
# One sentinel at the end: SCENARIO-e2e-plain-loxilb-<tag> [OK] or [FAILED].
set -u
source "$(dirname "$0")/env.sh"
HERE="$E2E_HERE"
FIX="$HERE/fixtures"
REV="${1:?usage: validation.sh <rev>}"
IMG="$(klb_image "$REV")"
TAG="$(klb_tag "$REV")"
R1="$(rest "$LLB1_ADDR")"
R2="$(rest "$LLB2_ADDR")"
POOL_PREFIX="${VIP_CIDR%.*}."
DIAG="${DIAG:-$WORK/diag}/$TAG"
mkdir -p "$DIAG"

fails=0
ok()   { echo "  [OK]   $1"; }
bad()  { echo "  [FAIL] $1${2:+ - $2}"; fails=$((fails+1)); }
note() { echo "  [NOTE] $1"; }
check() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1" "got '$2', want '$3'"; fi; }
# poll <seconds> <command...> - retry until the command succeeds
poll() { local t=$1 n; shift; for ((n=0;n<t;n++)); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }
# expect <description> <seconds> <command...> - poll, and say how it went
expect() { local d=$1; shift; if poll "$@"; then ok "$d"; else bad "$d" "not after ${1}s"; fi; }

# --- loxilb state ----------------------------------------------------------
rules_on() { curl -s --max-time 5 "$1/config/loadbalancer/all"; }
# rule_at <rest> <ip> <port> - the rule on that address and port
rule_at() {
  rules_on "$1" | jq -c --arg ip "$2" --argjson p "$3" \
    '.lbAttr[]? | select(.serviceArguments.externalIP==$ip and .serviceArguments.port==$p)' | head -1
}
has_rule() { [[ -n "$(rule_at "$@")" ]]; }
no_rule() { [[ -z "$(rule_at "$@")" ]]; }
eps_at() { rule_at "$@" | jq -r '[.endpoints[]?.endpointIP] | sort | join(",")'; }
eps_are() { [[ "$(eps_at "$1" "$2" "$3")" == "$4" ]]; } # <rest> <ip> <port> <eps>
# pool_rules <rest> - "ip:port/proto=eps" of every rule on a pool address
pool_rules() {
  rules_on "$1" | jq -r --arg p "$POOL_PREFIX" '[.lbAttr[]? | select(.serviceArguments.externalIP|startswith($p))
    | "\(.serviceArguments.externalIP):\(.serviceArguments.port)/\(.serviceArguments.protocol)=\([.endpoints[]?.endpointIP] | sort | join("+"))"]
    | sort | join(" ")'
}
pool_count() { rules_on "$1" | jq --arg p "$POOL_PREFIX" '[.lbAttr[]? | select(.serviceArguments.externalIP|startswith($p))] | length'; }
pool_empty() { [[ "$(pool_count "$1")" == 0 ]]; }
# ports_on <rest> <from> <to> - how many of the ports from..to have a rule
ports_on() {
  rules_on "$1" | jq --arg p "$POOL_PREFIX" --argjson a "$2" --argjson b "$3" \
    '[.lbAttr[]? | select((.serviceArguments.externalIP|startswith($p)) and .serviceArguments.port >= $a and .serviceArguments.port <= $b)
      | .serviceArguments.port] | unique | length'
}

# --- cluster state ---------------------------------------------------------
vip_of() {
  kubectl -n plain get svc "$1" -o json 2>/dev/null |
    jq -r '.status.loadBalancer.ingress[0] | .ip // .hostname // ""' | sed 's/^llb-//'
}
has_vip() { [[ -n "$(vip_of "$1")" ]]; }
pod_ips() { kubectl -n plain get pods -l app=ep -o json | jq -r '[.items[] | select(.metadata.deletionTimestamp == null) | .status.podIP // empty] | sort | join(",")'; }
pod_names() { kubectl -n plain get pods -l app=ep -o jsonpath='{.items[*].metadata.name}'; }
# served <vip> <port> - a request through the VIP is answered by an endpoint pod
served() {
  local body
  body="$(curl -s --max-time 5 "http://$1:$2/" 2>/dev/null)"
  [[ -n "$body" ]] && grep -qw -- "$body" <<<"$(pod_names)"
}

klb_log() { kubectl -n kube-system logs deploy/kube-loxilb --tail=-1 2>/dev/null; }
klb_pod() { kubectl -n kube-system get pods -l app=kube-loxilb-app -o jsonpath='{.items[0].metadata.name}'; }
klb_restarts() { kubectl -n kube-system get pods -l app=kube-loxilb-app -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}'; }
log_count() { klb_log | grep -cF -- "$1"; }
log_count_gt() { [[ "$(log_count "$1")" -gt "$2" ]]; }
save_log() {
  klb_log > "$DIAG/kube-loxilb.$1.log"
  kubectl -n kube-system logs deploy/kube-loxilb --previous > "$DIAG/kube-loxilb.$1.previous.log" 2>/dev/null ||
    rm -f "$DIAG/kube-loxilb.$1.previous.log"
}
alive() { log_count_gt "loxilb-lb($1) is alive" 0; }
both_alive() { alive "$LLB1_ADDR" && alive "$LLB2_ADDR"; }

LLB2_DOWN="loxilb-lb($LLB2_ADDR) is down"
LLB2_UP="loxilb-lb($LLB2_ADDR) is alive"
# swap_llb2 - replace llb2 with a fresh container, holding nothing, waiting for
# kube-loxilb to see it go and come back.
swap_llb2() {
  local downs ups
  downs=$(log_count "$LLB2_DOWN")
  ups=$(log_count "$LLB2_UP")
  docker rm -f llb2 >/dev/null
  poll 30 log_count_gt "$LLB2_DOWN" "$downs" || { echo "  kube-loxilb did not see llb2 go"; return 1; }
  run_llb llb2 "$LLB2_ADDR" || return 1
  poll 60 log_count_gt "$LLB2_UP" "$ups" || { echo "  kube-loxilb did not see llb2 back"; return 1; }
}

echo "=== P0. kube-loxilb $TAG ($REV) on fresh loxilb instances ==="
kubectl -n plain delete svc --all --wait=true >/dev/null 2>&1
kubectl -n kube-system delete deploy kube-loxilb --ignore-not-found --wait=true >/dev/null
kubectl -n kube-system wait --for=delete pod -l app=kube-loxilb-app --timeout=120s >/dev/null 2>&1
run_llb llb1 "$LLB1_ADDR" && run_llb llb2 "$LLB2_ADDR" || bad "loxilb instances started"
git -C "$REPO" show "$REV:manifest/ext-cluster/kube-loxilb.yaml" > "$WORK/kube-loxilb.$TAG.src.yaml"
KLB_IMAGE="$IMG" \
  KLB_ARGS="--loxiURL=http://$LLB1_ADDR:11111,http://$LLB2_ADDR:11111 --cidrPools=defaultPool=$VIP_CIDR --setLBMode=2 --v=4" \
  python3 "$HERE/gen-kube-loxilb.py" "$WORK/kube-loxilb.$TAG.src.yaml" > "$WORK/kube-loxilb.$TAG.yaml"
kubectl apply -f "$WORK/kube-loxilb.$TAG.yaml" >/dev/null
kubectl -n kube-system rollout status deploy/kube-loxilb --timeout=180s >/dev/null || bad "kube-loxilb rolled out"
expect "kube-loxilb sees both loxilb instances" 60 both_alive
POD0="$(klb_pod)"
RESTARTS0="$(klb_restarts)"
kubectl -n kube-system logs deploy/kube-loxilb 2>/dev/null | grep -m1 "Build:" | sed 's/^/  /'

echo "=== P1. Services get an address, a rule on each instance, and traffic ==="
kubectl apply -f "$FIX/web.yaml" -f "$FIX/podnet.yaml" >/dev/null
expect "web has an address" 60 has_vip web
expect "podnet has an address" 60 has_vip podnet
WEB_VIP="$(vip_of web)"
POD_VIP="$(vip_of podnet)"
note "web $WEB_VIP, podnet $POD_VIP"
for r in "$R1" "$R2"; do
  n=llb1; [[ "$r" == "$R2" ]] && n=llb2
  expect "$n has web:18080" 60 has_rule "$r" "$WEB_VIP" 18080
  expect "$n has web:18081" 30 has_rule "$r" "$WEB_VIP" 18081
  expect "$n has podnet:18090" 30 has_rule "$r" "$POD_VIP" 18090
done
expect "web:18080 served through the VIP" 30 served "$WEB_VIP" 18080
expect "web:18081 served through the VIP" 30 served "$WEB_VIP" 18081
expect "podnet served through the VIP" 30 served "$POD_VIP" 18090
check "podnet endpoints on llb1 are the pods" "$(eps_at "$R1" "$POD_VIP" 18090 | tr '+' ',')" "$(pod_ips)"
check "podnet endpoints on llb2 are the pods" "$(eps_at "$R2" "$POD_VIP" 18090 | tr '+' ',')" "$(pod_ips)"

echo "=== P2. an endpoint change reaches every instance ==="
kubectl -n plain scale deploy/ep --replicas=3 >/dev/null
kubectl -n plain rollout status deploy/ep --timeout=120s >/dev/null
expect "llb1 has the three pods" 60 eps_are "$R1" "$POD_VIP" 18090 "$(pod_ips)"
expect "llb2 has the three pods" 60 eps_are "$R2" "$POD_VIP" 18090 "$(pod_ips)"
kubectl -n plain scale deploy/ep --replicas=2 >/dev/null
kubectl -n plain rollout status deploy/ep --timeout=120s >/dev/null
poll 60 bash -c "[[ \$(kubectl -n plain get pods -l app=ep --no-headers 2>/dev/null | wc -l) == 2 ]]"
expect "llb1 back to two pods" 60 eps_are "$R1" "$POD_VIP" 18090 "$(pod_ips)"
expect "llb2 back to two pods" 60 eps_are "$R2" "$POD_VIP" 18090 "$(pod_ips)"
expect "podnet still served" 30 served "$POD_VIP" 18090

echo "=== P3. a kube-loxilb restart changes nothing on the instances ==="
BEFORE1="$(pool_rules "$R1")"
BEFORE2="$(pool_rules "$R2")"
save_log p3-before
kubectl -n kube-system rollout restart deploy/kube-loxilb >/dev/null
kubectl -n kube-system rollout status deploy/kube-loxilb --timeout=180s >/dev/null
poll 60 both_alive
sleep 15
check "llb1 rules unchanged" "$(pool_rules "$R1")" "$BEFORE1"
check "llb2 rules unchanged" "$(pool_rules "$R2")" "$BEFORE2"
check "web keeps its address" "$(vip_of web)" "$WEB_VIP"
check "podnet keeps its address" "$(vip_of podnet)" "$POD_VIP"
check "nothing deleted by the new kube-loxilb" "$(klb_log | grep -cE 'delete lb|deleteAll lb')" 0
expect "web still served" 30 served "$WEB_VIP" 18080
POD0="$(klb_pod)"
RESTARTS0="$(klb_restarts)"
save_log p3-after

echo "=== P4. an instance replaced by a fresh one gets every rule back ==="
swap_llb2 || bad "llb2 replaced and seen back"
three_on_llb2() { [[ "$(pool_count "$R2")" == 3 ]]; }
expect "all three rules back on llb2" 60 three_on_llb2
check "llb2 rules as before" "$(pool_rules "$R2")" "$BEFORE2"
check "kube-loxilb not restarted" "$(klb_pod)/$(klb_restarts)" "$POD0/$RESTARTS0"
save_log p4

echo "=== P5. an instance that stops answering holds nothing else up ==="
downs=$(log_count "$LLB2_DOWN")
ups=$(log_count "$LLB2_UP")
in_netns llb2 iptables -I INPUT -p tcp --dport 11111 -j DROP
expect "kube-loxilb saw llb2 go" 30 log_count_gt "$LLB2_DOWN" "$downs"
kubectl -n plain delete svc web --wait=false >/dev/null
start=$(date +%s)
kubectl apply -f "$FIX/late.yaml" >/dev/null
late_on_llb1() { local v; v="$(vip_of late)"; [[ -n "$v" ]] && has_rule "$R1" "$v" 18100; }
if poll 240 late_on_llb1; then
  took=$(( $(date +%s) - start ))
  # The same description either way, so the two builds' outcomes line up.
  if (( took <= 30 )); then ok "a new Service reached llb1 within 30s - took ${took}s"; else bad "a new Service reached llb1 within 30s" "took ${took}s"; fi
else
  bad "a new Service reached llb1" "not after 240s"
fi
LATE_VIP="$(vip_of late)"
web_gone_from() { no_rule "$1" "$WEB_VIP" 18080 && no_rule "$1" "$WEB_VIP" 18081; }
# Its address may go to late, on another port; the ports tell them apart.
expect "the deleted Service is gone from llb1" 60 web_gone_from "$R1"
expect "late served through llb1" 30 served "$LATE_VIP" 18100
in_netns llb2 iptables -D INPUT -p tcp --dport 11111 -j DROP
expect "kube-loxilb saw llb2 back" 60 log_count_gt "$LLB2_UP" "$ups"
expect "late reaches llb2 once it answers" 90 has_rule "$R2" "$LATE_VIP" 18100
expect "the deleted Service is gone from llb2 too" 90 web_gone_from "$R2"
check "kube-loxilb not restarted" "$(klb_pod)/$(klb_restarts)" "$POD0/$RESTARTS0"
save_log p5

echo "=== P6. many Services at once while an instance comes and goes ==="
BULK=20
for i in $(seq 1 $BULK); do
  cat <<YAML
---
apiVersion: v1
kind: Service
metadata:
  name: bulk-$i
  namespace: plain
spec:
  type: LoadBalancer
  loadBalancerClass: loxilb.io/loxilb
  selector:
    app: ep
  ports:
    - port: $((19000 + i))
      targetPort: 8080
YAML
done > "$WORK/bulk.yaml"
kubectl apply -f "$WORK/bulk.yaml" >/dev/null
swap_llb2 || bad "llb2 replaced and seen back (1)"
swap_llb2 || bad "llb2 replaced and seen back (2)"
bulk_on() { [[ "$(ports_on "$1" 19001 $((19000 + BULK)))" == "$BULK" ]]; }
expect "all $BULK Services on llb1" 120 bulk_on "$R1"
expect "all $BULK Services on llb2" 120 bulk_on "$R2"
expect "and the earlier ones on llb2" 60 has_rule "$R2" "$LATE_VIP" 18100
check "kube-loxilb not restarted" "$(klb_pod)/$(klb_restarts)" "$POD0/$RESTARTS0"
crash="$( { klb_log; kubectl -n kube-system logs deploy/kube-loxilb --previous 2>/dev/null; } | grep -m1 -E 'concurrent map|fatal error|panic:')"
[[ -z "$crash" ]] && ok "no crash in the kube-loxilb log" || bad "no crash in the kube-loxilb log" "$crash"
save_log p6

echo "=== P7. deleting every Service leaves no rule behind ==="
kubectl -n plain delete svc --all --wait=true >/dev/null
expect "no pool rule left on llb1" 90 pool_empty "$R1"
expect "no pool rule left on llb2" 90 pool_empty "$R2"
check "kube-loxilb not restarted" "$(klb_pod)/$(klb_restarts)" "$POD0/$RESTARTS0"
save_log p7

echo
if [[ $fails -eq 0 ]]; then
  echo "SCENARIO-e2e-plain-loxilb-$TAG [OK]"
  exit 0
fi
echo "SCENARIO-e2e-plain-loxilb-$TAG [FAILED] ($fails check(s))"
exit 1
