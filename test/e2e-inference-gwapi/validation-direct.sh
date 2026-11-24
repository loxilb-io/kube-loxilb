#!/bin/bash
# HTTPRoutes served directly on a Gateway's HTTP listener, as host and path
# rules of loxilb-inference-gateway (--gatewayAPIDirectRoutes), on a live
# cluster and live gateways.
#
# Run after validation.sh and validation-auth.sh: it turns the flag on, which
# the older kube-loxilb build the credential checks downgrade to does not know.
#
# One sentinel at the end: SCENARIO-e2e-direct [OK] or [FAILED].
set -u
source "$(dirname "$0")/env.sh"
HERE="$E2E_HERE"
PORT=8090
TLS_PORT=8453

fails=0
ok()   { echo "  [OK]   $1"; }
bad()  { echo "  [FAIL] $1${2:+ - $2}"; fails=$((fails+1)); }
note() { echo "  [NOTE] $1"; }
check() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1" "got '$2', want '$3'"; fi; }
poll() { local t=$1; shift; for ((i=0;i<t;i++)); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }

rules() { curl -s --max-time 5 "$(rest "$1")/config/loadbalancer/all"; }
# direct_rules <gateway addr> - the rules on the VIP's direct port
direct_rules() {
  rules "$1" | jq -c --arg ip "$VIP" --argjson p "$PORT" \
    '[.lbAttr[]? | select(.serviceArguments.externalIP==$ip and .serviceArguments.port==$p)]'
}
rule_keys() { direct_rules "$1" | jq -r '[.[] | .serviceArguments | "\(.host)|\(.path_prefix)|\(.path_match_mode)|\(.mode)"] | sort | join(" ")'; }
gen() { kubectl -n web get svc -l loxilb.io/gateway-route=true -o json; }
# A rule's Service is installed once its status names the address - as an IP,
# or, in a zone, as the zone's hostname for it (llb-12.12.12.1).
installed_count() {
  gen | jq --arg ip "$VIP" '[.items[] | select(any(.status.loadBalancer.ingress[]?;
                              .ip==$ip or ((.hostname // "") | endswith("-" + $ip))))] | length'
}
# ask <host> <path> - "<body> <status>" of one request through the VIP
ask() { echo "$(body "$1" "$2") $(code "$1" "$2")"; }
body() { curl -s --max-time 5 -H "Host: $1" "http://$VIP:$PORT$2"; }
code() { curl -s -o /dev/null --max-time 5 -w '%{http_code}' -H "Host: $1" "http://$VIP:$PORT$2"; }
# pods_of <app> - the addresses of its pods that are not going away
pods_of() {
  kubectl -n web get pods -l "app=$1" -o json |
    jq -r '[.items[] | select(.metadata.deletionTimestamp == null and .status.podIP != null) | .status.podIP] | sort | join(",")'
}
route_cond() { # <route> <type> -> status/reason/message of this controller's condition
  kubectl -n web get httproute "$1" -o json |
    jq -r --arg t "$2" '.status.parents[]? | select(.controllerName=="loxilb.io/loxilb")
                        | .conditions[]? | select(.type==$t) | "\(.status)/\(.reason)/\(.message)"' | head -1
}
# Conditions to poll for.
installed_is() { [[ "$(installed_count)" == "$1" ]]; }
rules_are() { [[ "$(rule_keys "$1")" == "$2" ]]; }
served_by() { body "$1" "$2" | grep -q "^$3-"; }
api_endpoints_are() {
  [[ "$(direct_rules "$IGW1_ADDR" | jq -r '.[] | select(.serviceArguments.path_prefix=="/api") | [.endpoints[].endpointIP] | sort | join(",")')" == "$1" ]]
}
l4_rules_are() { # <count> - the rules on VIP:PORT with no host, on igw1
  [[ "$(direct_rules "$IGW1_ADDR" | jq '[.[] | select((.serviceArguments.host // "") == "")] | length')" == "$1" ]]
}
only_l4_on() { # <gateway addr> - on VIP:PORT, one rule with no host and none with one
  [[ "$(direct_rules "$1" | jq -r '[([.[] | select((.serviceArguments.host // "") == "")] | length),
                                    ([.[] | select((.serviceArguments.host // "") != "")] | length)] | join(":")')" == 1:0 ]]
}
host_rules_are() {
  [[ "$(direct_rules "$IGW1_ADDR" | jq --arg h "$1" '[.[] | select(.serviceArguments.host==$h)] | length')" == "$2" ]]
}
ingress_ports_are() { [[ "$(kubectl -n web get svc direct-gw-ingress-service -o json 2>/dev/null | jq -r '[.spec.ports[]?.port] | sort | join(",")')" == "$1" ]]; }
http_ingresses_are() {
  [[ "$(kubectl -n web get ingress -o json | jq -r '[.items[] | select(.metadata.annotations["parent-listener"]=="http")] | length')" == "$1" ]]
}
programmed() { [[ "$(kubectl -n web get gateway direct-gw -o json | jq -r '.status.conditions[] | select(.type=="Programmed") | .status')" == True ]]; }
refused() { kubectl -n web get events --field-selector "involvedObject.name=$1" -o json | jq -e '.items[] | select(.reason=="GatewayRouteRefused")'; }
manual_slice() { # <pod IPs, comma separated>
  local eps="" ip
  for ip in ${1//,/ }; do eps+="  - {addresses: [\"$ip\"], conditions: {ready: true}}"$'\n'; done
  kubectl apply -f - >/dev/null <<EOF
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: web-manual-1
  namespace: web
  labels:
    kubernetes.io/service-name: web-manual
    endpointslice.kubernetes.io/managed-by: e2e-direct
addressType: IPv4
ports: [{name: http, port: 8000, protocol: TCP}]
endpoints:
$eps
EOF
}

klb_log() { kubectl -n kube-system logs deploy/kube-loxilb --tail=-1 2>/dev/null; }
log_lines() { klb_log | wc -l; }
log_has() { klb_log | grep -qF -- "$1"; }
RELEASED="direct-gw-ingress-service: $VIP:$PORT:tcp is released to the Gateway's direct routes"
DROPPED="direct-gw-ingress-service: $VIP:$PORT:tcp is no longer the Service's"
PORTS_CHANGED="service web/direct-gw-ingress-service ports updated"
# line_of <first log line> <first|last> <text> - where in the log, counted
# from that line, the first or last line saying text is; 0 if none
line_of() {
  klb_log | tail -n +"$1" | awk -v t="$3" -v w="$2" 'index($0, t) { n = NR; if (w == "first") { print n; found = 1; exit } }
    END { if (!found) print n + 0 }'
}
count_of() { klb_log | tail -n +"$1" | grep -cF -- "$2"; } # <first log line> <text>
# line_of_both <first log line> <text> <text> - where in the log, counted from
# that line, the first line saying both is; 0 if none
line_of_both() {
  klb_log | tail -n +"$1" | awk -v a="$2" -v b="$3" 'index($0, a) && index($0, b) { print NR; found = 1; exit }
    END { if (!found) print 0 }'
}
no_rule_services() { [[ "$(gen | jq '.items | length')" == 0 ]]; }

echo "=== D0. direct routes on ==="
ARGS=$(kubectl -n kube-system get deploy kube-loxilb -o json | jq -c '.spec.template.spec.containers[0].args')
if ! jq -e 'index("--gatewayAPIDirectRoutes")' <<<"$ARGS" >/dev/null; then
  kubectl -n kube-system patch deploy kube-loxilb --type=json -p \
    "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":$(jq -c '. + ["--gatewayAPIDirectRoutes"]' <<<"$ARGS")}]" >/dev/null
fi
if kubectl -n kube-system rollout status deploy/kube-loxilb --timeout=180s >/dev/null; then
  ok "kube-loxilb runs with --gatewayAPIDirectRoutes"
else
  bad "kube-loxilb runs with --gatewayAPIDirectRoutes" "rollout did not finish"
fi

echo "=== D1. fixtures ==="
CERT_DIR=$(mktemp -d)
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=secure.example.com" \
  -keyout "$CERT_DIR/key" -out "$CERT_DIR/crt" >/dev/null 2>&1
kubectl apply -f "$HERE/fixtures/direct.yaml" >/dev/null
kubectl -n web create secret tls web-cert --cert="$CERT_DIR/crt" --key="$CERT_DIR/key" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
rm -rf "$CERT_DIR"
kubectl -n web rollout status deploy/web-a --timeout=180s >/dev/null || bad "web-a ready"
kubectl -n web rollout status deploy/web-b --timeout=180s >/dev/null || bad "web-b ready"
WEB_A=$(pods_of web-a)
WEB_B=$(pods_of web-b)
# Younger than shop-api, and the same host and path once its trailing slash
# is gone: it loses the pair.
sleep 2
kubectl apply -f - >/dev/null <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: shop-api-dup
  namespace: web
spec:
  parentRefs: [{name: direct-gw, sectionName: http}]
  hostnames: [shop.example.com]
  rules:
    - matches: [{path: {type: PathPrefix, value: /api/}}]
      backendRefs: [{name: web-b, port: 80}]
EOF
echo "  web-a pods $WEB_A; web-b pods $WEB_B"
# The Gateway last, its routes in the cache by then, and web-manual's
# EndpointSlices before it, serving its port on two target ports: one rule
# cannot hold those, so the manual.example.com rule is not installed, the
# HTTP port stays on the ingress Service, and its L4 rule reaches the
# gateways. (A rule without endpoints would not hold it: where the gateways
# list no_endpoint_rule, that is installed, answering 503.) The checks below
# see the port move to the rules once one target port is left, and that L4
# rule go.
manual_slice "${WEB_B%%,*}"
kubectl apply -f - >/dev/null <<EOF
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: web-manual-2
  namespace: web
  labels:
    kubernetes.io/service-name: web-manual
    endpointslice.kubernetes.io/managed-by: e2e-direct
addressType: IPv4
ports: [{name: http, port: 8001, protocol: TCP}]
endpoints:
  - {addresses: ["${WEB_B%%,*}"], conditions: {ready: true}}
EOF
sleep 2
kubectl apply -f "$HERE/fixtures/direct-gateway.yaml" >/dev/null
if poll 90 l4_rules_are 1; then
  ok "the HTTP port is on the ingress Service's L4 rule first"
else
  bad "the HTTP port is on the ingress Service's L4 rule first" "$(rule_keys "$IGW1_ADDR")"
fi
manual_slice "$WEB_B"
kubectl -n web delete endpointslice web-manual-2 >/dev/null

echo "=== D2. one Service per rule, controlled by the Gateway, installed ==="
if poll 180 installed_is 3; then
  ok "three rule Services, each installed"
else
  bad "three rule Services, each installed" "$(gen | jq -c '[.items[] | {n: .metadata.name, host: .metadata.annotations["loxilb.io/gateway-route-host"], path: .metadata.annotations["loxilb.io/gateway-route-path"], ing: .status.loadBalancer.ingress}]')"
fi
check "what each serves" \
  "$(gen | jq -r '[.items[] | .metadata.annotations | "\(.["loxilb.io/gateway-route-host"])|\(.["loxilb.io/gateway-route-path"])>\(.["loxilb.io/gateway-route-backend"])"] | sort | join(" ")')" \
  "manual.example.com|/>web/web-manual shop.example.com|/>web/web-b shop.example.com|/api>web/web-a"
check "controlled by direct-gw" \
  "$(gen | jq -r '[.items[].metadata.ownerReferences[]? | select(.controller==true and .kind=="Gateway") | .name] | unique | join(",")')" "direct-gw"

echo "=== D3. the rules on both gateways ==="
WANT="manual.example.com|/|prefix|4 shop.example.com|/api|prefix|4 shop.example.com|/|prefix|4"
for gw in "$IGW1_ADDR" "$IGW2_ADDR"; do
  poll 60 rules_are "$gw" "$WANT"
  check "host and path rules on $gw:$PORT, fullproxy" "$(rule_keys "$gw")" "$WANT"
done
check "/api is served by web-a's pods" \
  "$(direct_rules "$IGW1_ADDR" | jq -r '.[] | select(.serviceArguments.path_prefix=="/api") | [.endpoints[].endpointIP] | sort | join(",")')" "$WEB_A"
check "on their target port" \
  "$(direct_rules "$IGW1_ADDR" | jq -r '[.[].endpoints[].targetPort] | unique | join(",")')" "8000"
check "manual.example.com is served by the endpoints written by hand" \
  "$(direct_rules "$IGW1_ADDR" | jq -r '.[] | select(.serviceArguments.host=="manual.example.com") | [.endpoints[].endpointIP] | sort | join(",")')" "$WEB_B"

echo "=== D4. the ingress Service keeps HTTPS only ==="
# It lets the HTTP port go once the rules' Services report them installed,
# and the HTTP listener's Ingresses go after it.
poll 60 ingress_ports_are "$TLS_PORT"
poll 60 http_ingresses_are 0
ING=$(kubectl -n web get svc direct-gw-ingress-service -o json 2>/dev/null)
check "ingress Service ports" "$(jq -r '[.spec.ports[]?.port] | sort | join(",")' <<<"$ING")" "$TLS_PORT"
check "ingress Service controlled by direct-gw" \
  "$(jq -r '[.metadata.ownerReferences[]? | select(.controller==true and .kind=="Gateway") | .name] | join(",")' <<<"$ING")" "direct-gw"
check "no Ingress for the HTTP listener" \
  "$(kubectl -n web get ingress -o json | jq -r '[.items[] | select(.metadata.annotations["parent-listener"]=="http")] | length')" "0"
check "an Ingress for the HTTPS listener" \
  "$(kubectl -n web get ingress -o json | jq -r '[.items[] | select(.metadata.annotations["parent-listener"]=="https")] | length')" "1"
# The L4 rule comes off first: once the port is off the ingress Service, its
# node port is closed, and the gateway serves the L4 rule ahead of the host and
# path rules.
REL=$(line_of 1 first "$RELEASED")
UPD=$(line_of 1 first "$PORTS_CHANGED")
if [[ $REL -gt 0 && $UPD -gt $REL ]]; then
  ok "the HTTP port's L4 rule came off the gateways before the port left the ingress Service"
else
  bad "the HTTP port's L4 rule came off the gateways before the port left the ingress Service" \
    "released at log line $REL, ports changed at $UPD"
fi
check "the port did not leave with its L4 rule still on" "$(count_of 1 "$DROPPED")" 0
# loxilb-ingress runs in kube-system, and a selector picks pods of the
# Service's own namespace only: the ingress Service has none, and its
# EndpointSlice names the loxilb-ingress pods.
check "the ingress Service has no selector" "$(jq -r '.spec.selector // {} | length' <<<"$ING")" "0"
ingress_pods() { kubectl -n kube-system get pods -l app=loxilb-ingress -o json | jq -r '[.items[].status.podIP] | sort | join(",")'; }
ingress_slice() {
  kubectl -n web get endpointslices -l kubernetes.io/service-name=direct-gw-ingress-service,endpointslice.kubernetes.io/managed-by=kube-loxilb -o json |
    jq -r '[.items[].endpoints[]? | select(.conditions.ready) | .addresses[0]] | sort | join(",")'
}
slice_names_ingress_pods() { [[ -n "$(ingress_pods)" && "$(ingress_slice)" == "$(ingress_pods)" ]]; }
if poll 60 slice_names_ingress_pods; then
  ok "its EndpointSlice names the loxilb-ingress pods in kube-system"
else
  bad "its EndpointSlice names the loxilb-ingress pods in kube-system" "slice '$(ingress_slice)', pods '$(ingress_pods)'"
fi
tls_body() { curl -sk --max-time 5 --resolve "secure.example.com:$TLS_PORT:$VIP" "https://secure.example.com:$TLS_PORT/"; }
tls_served() { tls_body | grep -q '^web-a-'; }
tls_at() { curl -sk --max-time 5 --resolve "secure.example.com:$TLS_PORT:$VIP" "https://secure.example.com:$TLS_PORT$1"; }
tls_code() { curl -sk -o /dev/null --max-time 5 -w '%{http_code}' --resolve "secure.example.com:$TLS_PORT:$VIP" "https://secure.example.com:$TLS_PORT$1"; }
# The loxilb-ingress pods that report which Ingresses they have installed.
reporting_pods() {
  kubectl -n kube-system get pods -l app=loxilb-ingress,loxilb.io/programmed-report=v1 -o json |
    jq -r '[.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name] | join(" ")'
}
ingress_marked() { # <route> - each pod that reports has its Ingress's generation
  local ing p
  ing=$(kubectl -n web get ingress -o json | jq -c --arg r "$1" '[.items[] | select(.metadata.annotations["parent-http-route"]==$r)][0]')
  for p in $(reporting_pods); do
    jq -e --arg k "loxilb.io/programmed.$p" '.metadata.annotations[$k] == (.metadata.generation | tostring)' <<<"$ing" >/dev/null || return 1
  done
}
if poll 90 tls_served; then
  ok "secure.example.com over HTTPS: through the ingress Service and loxilb-ingress, to web-a"
else
  bad "secure.example.com over HTTPS: through the ingress Service and loxilb-ingress, to web-a" "'$(tls_body)'"
fi

echo "=== D4b. a rule whose backend has no endpoints ==="
# loxilb-ingress installs the paths of an Ingress whose backends it can
# resolve, and leaves out the rest (loxilb-ingress #22): the HTTPS route's
# rule to web-a still answers beside one to web-down, which has no endpoints.
# A request for that rule should get a 5xx - Gateway API asks for 503 - but
# with no rule of its own, the shorter path takes it. So it does for a
# direct route on the HTTP listener, unless both gateways list
# no_endpoint_rule (D13): elsewhere kube-loxilb does not send a rule with no
# endpoints, which the gateway refuses (400, endpoints-range error).
gen_count_is() { [[ "$(gen | jq '.items | length')" == "$1" ]]; }
GEN_BEFORE=$(gen | jq '.items | length')
kubectl apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Service
metadata: {name: web-down, namespace: web}
spec:
  selector: {app: nobody}
  ports: [{name: http, port: 80, targetPort: 8000}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: down, namespace: web}
spec:
  parentRefs: [{name: direct-gw, sectionName: http}]
  hostnames: [shop.example.com]
  rules:
    - matches: [{path: {type: PathPrefix, value: /down}}]
      backendRefs: [{name: web-down, port: 80}]
EOF
kubectl -n web patch httproute secure --type=json -p '[{"op":"add","path":"/spec/rules/0","value":
  {"matches":[{"path":{"type":"PathPrefix","value":"/down"}}],"backendRefs":[{"name":"web-down","port":80}]}}]' >/dev/null
secure_has_down() {
  kubectl -n web get ingress -o json |
    jq -e '.items[] | select(.metadata.annotations["parent-http-route"]=="secure") | select(any(.spec.rules[].http.paths[]; .path=="/down"))'
}
poll 30 secure_has_down
if [[ -n "$(reporting_pods)" ]]; then poll 30 ingress_marked secure; else sleep 10; fi
case "$(tls_at /)" in
  web-a-*) ok "secure.example.com/ is answered beside a rule whose backend has no endpoints";;
  *) bad "secure.example.com/ is answered beside a rule whose backend has no endpoints" "'$(tls_at /)'";;
esac
down_answer() { # <what answered> <code> <body> <why the shorter path took it>
  case "$2" in
    5??) ok "$1: a request for the rule with no endpoints gets $2";;
    *) note "$1: a request for the rule with no endpoints gets $2 from ${3:-nothing}: $4";;
  esac
}
down_answer "through loxilb-ingress" "$(tls_code /down)" "$(tls_at /down | cut -d- -f1,2)" \
  "loxilb-ingress leaves its path out, and the shorter path takes it"
# Its rule Service comes: installed without endpoints where both gateways
# take that, and answered 503; not installed elsewhere.
poll 30 gen_count_is $((GEN_BEFORE + 1))
down_is_5xx() { [[ "$(code shop.example.com /down)" == 5?? ]]; }
poll 10 down_is_5xx
down_answer "directly" "$(code shop.example.com /down)" "$(body shop.example.com /down | cut -d- -f1,2)" \
  "the gateway takes no rule without endpoints, and the shorter path takes it"
kubectl -n web delete httproute down >/dev/null
kubectl -n web patch httproute secure --type=json -p '[{"op":"remove","path":"/spec/rules/0"}]' >/dev/null
kubectl -n web delete svc web-down >/dev/null
poll 60 gen_count_is "$GEN_BEFORE"
check "the rule Services are as they were" "$(gen | jq '.items | length')" "$GEN_BEFORE"

echo "=== D5. requests ==="
poll 30 served_by shop.example.com /api/x web-a
case "$(body shop.example.com /api/items)" in web-a-*) ok "shop.example.com/api/items -> web-a";; *) bad "shop.example.com/api/items -> web-a" "$(ask shop.example.com /api/items)";; esac
case "$(body shop.example.com /cart)" in web-b-*) ok "shop.example.com/cart -> web-b";; *) bad "shop.example.com/cart -> web-b" "$(ask shop.example.com /cart)";; esac
case "$(body shop.example.com /apiary)" in web-b-*) ok "/apiary is not under /api";; *) bad "/apiary is not under /api" "$(ask shop.example.com /apiary)";; esac
case "$(body manual.example.com /)" in web-b-*) ok "manual.example.com -> its hand-written endpoints";; *) bad "manual.example.com -> its hand-written endpoints" "$(ask manual.example.com /)";; esac
check "a host no route names" "$(code nobody.example.com /)" "503"
check "a header match is not served broadly" "$(code hdr.example.com /)" "503"
UPPER=$(code SHOP.EXAMPLE.COM /cart)
[[ "$UPPER" == 200 ]] && ok "an upper-case Host is served" ||
  note "an upper-case Host gets $UPPER: the gateway compares hosts case-sensitively until its fix (request 1-4)"

echo "=== D6. route status ==="
# Its one rule lost, so nothing of it is served: not accepted, and why.
DUP=$(route_cond shop-api-dup Accepted)
[[ "$DUP" == False/UnsupportedValue/*"served for route web/shop-api"* ]] && ok "the younger route says its pair is another's" ||
  bad "the younger route says its pair is another's" "$DUP"
HDR=$(route_cond headers Accepted)
[[ "$HDR" == False/UnsupportedValue/*headers* ]] && ok "the header match says it is not served" ||
  bad "the header match says it is not served" "$HDR"
check "shop-api Accepted" "$(route_cond shop-api Accepted | cut -d/ -f1-2)" "True/Accepted"
poll 30 programmed
check "direct-gw Programmed" \
  "$(kubectl -n web get gateway direct-gw -o json | jq -r '.status.conditions[] | select(.type=="Programmed") | "\(.status)"')" "True"

echo "=== D7. endpoints follow the backend ==="
kubectl -n web scale deploy/web-a --replicas=1 >/dev/null
kubectl -n web rollout status deploy/web-a --timeout=120s >/dev/null
poll 60 bash -c "[[ \$(kubectl -n web get pods -l app=web-a --no-headers 2>/dev/null | wc -l) == 1 ]]"
WEB_A1=$(pods_of web-a)
if poll 60 api_endpoints_are "$WEB_A1"; then
  ok "web-a scaled to one: the /api rule follows"
else
  bad "web-a scaled to one: the /api rule follows" "$(direct_rules "$IGW1_ADDR" | jq -c '.[] | select(.serviceArguments.path_prefix=="/api") | [.endpoints[].endpointIP]')"
fi
manual_slice "$WEB_A1"
if poll 60 served_by manual.example.com / web-a; then
  ok "the hand-written EndpointSlice moves to web-a: so does manual.example.com"
else
  bad "the hand-written EndpointSlice moves to web-a: so does manual.example.com" "$(ask manual.example.com /)"
fi

echo "=== D7b. a route's match mode changes ==="
# The gateway keys a rule by its match mode too: the prefix rule has to go
# when the exact one comes, on both gateways, and come back after.
match_is() { # <PathPrefix|Exact>
  kubectl -n web patch httproute shop-api --type=json \
    -p "[{\"op\":\"replace\",\"path\":\"/spec/rules/0/matches/0/path/type\",\"value\":\"$1\"}]" >/dev/null
}
D7B_FROM=$(($(log_lines) + 1))
match_is Exact
EXACT="manual.example.com|/|prefix|4 shop.example.com|/api|exact|4 shop.example.com|/|prefix|4"
for gw in "$IGW1_ADDR" "$IGW2_ADDR"; do
  poll 60 rules_are "$gw" "$EXACT"
  check "/api matched exactly on $gw, the prefix rule gone" "$(rule_keys "$gw")" "$EXACT"
done
poll 30 served_by shop.example.com /api web-a
case "$(body shop.example.com /api)" in web-a-*) ok "shop.example.com/api -> web-a";; *) bad "shop.example.com/api -> web-a" "$(ask shop.example.com /api)";; esac
case "$(body shop.example.com /api/items)" in web-b-*) ok "/api/items is not /api: web-b";; *) bad "/api/items is not /api: web-b" "$(ask shop.example.com /api/items)";; esac
match_is PathPrefix
for gw in "$IGW1_ADDR" "$IGW2_ADDR"; do
  poll 60 rules_are "$gw" "$WANT"
  check "/api by prefix again on $gw" "$(rule_keys "$gw")" "$WANT"
done
# The port stays with the direct routes while a rule of it changes; it went
# back to the ingress Service until the new rule was installed.
check "the ingress Service's ports did not change meanwhile" "$(count_of "$D7B_FROM" "$PORTS_CHANGED")" 0

echo "=== D8. a Service that only looks like a rule is refused ==="
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: lookalike
  namespace: web
  labels: {loxilb.io/gateway-route: "true"}
  annotations:
    loxilb.io/lbmode: fullproxy
    loxilb.io/gateway-route-host: evil.example.com
    loxilb.io/gateway-route-path: /
    loxilb.io/gateway-route-backend: llm/vllm-qwen3-inference
    loxilb.io/gateway-route-backend-port: "8080"
spec:
  type: LoadBalancer
  loadBalancerClass: loxilb.io/loxilb
  loadBalancerIP: $VIP
  ports: [{name: http, port: $PORT, protocol: TCP}]
EOF
if poll 60 refused lookalike; then
  ok "GatewayRouteRefused on the lookalike"
else
  bad "GatewayRouteRefused on the lookalike" "no event"
fi
check "no rule for evil.example.com" \
  "$(direct_rules "$IGW1_ADDR" | jq -r '[.[] | select(.serviceArguments.host=="evil.example.com")] | length')" "0"
kubectl -n web delete svc lookalike >/dev/null 2>&1

echo "=== D9. a route that goes takes its rule ==="
D9_FROM=$(($(log_lines) + 1))
kubectl -n web delete httproute manual >/dev/null
if poll 60 host_rules_are manual.example.com 0; then
  ok "manual.example.com's rule is gone"
else
  bad "manual.example.com's rule is gone" "$(rule_keys "$IGW1_ADDR")"
fi
check "manual.example.com is not served" "$(code manual.example.com /)" "503"
check "the other rules stay" "$(installed_count)" "2"
check "the ingress Service's ports did not change" "$(count_of "$D9_FROM" "$PORTS_CHANGED")" 0
WANT2="shop.example.com|/api|prefix|4 shop.example.com|/|prefix|4"

echo "=== D10. kube-loxilb restarts while igw2 does not answer ==="
# No peer has said what it is when kube-loxilb starts, and igw2 does not until
# it answers. The HTTP port stays with its direct routes meanwhile; it went
# back to the ingress Service, and its rule Services were deleted, until igw2
# answered.
in_netns igw2 iptables -I INPUT -p tcp --dport 11111 -j DROP
kubectl -n kube-system rollout restart deploy/kube-loxilb >/dev/null
kubectl -n kube-system rollout status deploy/kube-loxilb --timeout=180s >/dev/null ||
  bad "kube-loxilb restarted" "rollout did not finish"
HELD="port $PORT stays with its direct routes: loxilb-lb($IGW2_ADDR) has not reported what it is yet"
if poll 60 log_has "$HELD"; then
  ok "the Gateway is synced while igw2 has not said what it is"
else
  bad "the Gateway is synced while igw2 has not said what it is" "no such line in the log"
fi
sleep 5
check "the ingress Service keeps HTTPS only" \
  "$(kubectl -n web get svc direct-gw-ingress-service -o json | jq -r '[.spec.ports[]?.port] | sort | join(",")')" "$TLS_PORT"
check "the rule Services stay" "$(gen | jq '.items | length')" 2
check "the rules stay on igw1" "$(rule_keys "$IGW1_ADDR")" "$WANT2"
case "$(body shop.example.com /api/x)" in web-a-*) ok "shop.example.com/api is still served";; *) bad "shop.example.com/api is still served" "$(ask shop.example.com /api/x)";; esac
in_netns igw2 iptables -D INPUT -p tcp --dport 11111 -j DROP
if poll 90 log_has "loxilb-lb($IGW2_ADDR) is alive"; then
  ok "igw2 is back"
else
  bad "igw2 is back" "kube-loxilb did not see it"
fi
poll 60 rules_are "$IGW2_ADDR" "$WANT2"
check "the rules on igw2" "$(rule_keys "$IGW2_ADDR")" "$WANT2"
check "and the ingress Service still keeps HTTPS only" \
  "$(kubectl -n web get svc direct-gw-ingress-service -o json | jq -r '[.spec.ports[]?.port] | sort | join(",")')" "$TLS_PORT"

echo "=== D11. the HTTP port goes back to the ingress Service, and comes again ==="
# A port goes back once it cannot be served directly. Of the ways there are,
# the one this testbed has that keeps the port HTTP is an HTTPS listener on it
# as well; the ingress Service leaves that listener out, as it conflicts.
# The port's direct rules serve it until the Ingress path can: its L4 rule is
# held back until each loxilb-ingress pod that reports has installed the
# port's Ingresses, and the rules go once both gateways have the L4 rule. A
# host's requests, sent all the while, each on a new connection, are each
# answered.
kubectl apply -f - >/dev/null <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: back, namespace: web}
spec:
  parentRefs: [{name: direct-gw, sectionName: http}]
  hostnames: [back.example.com]
  rules:
    - backendRefs: [{name: web-a, port: 80}]
EOF
if poll 60 served_by back.example.com / web-a; then
  ok "back.example.com is served directly"
else
  bad "back.example.com is served directly" "$(ask back.example.com /)"
fi
REPORTING=$(kubectl -n kube-system get pods -l app=loxilb-ingress,loxilb.io/programmed-report=v1 -o name | wc -l)
PROBE="$WORK/d11-requests"
rm -f "$PROBE" "$PROBE.stop"
(while [[ ! -e "$PROBE.stop" ]]; do
  printf '%s %s\n' "$(date +%T.%N | cut -c1-12)" "$(curl -s --max-time 2 -H 'Host: back.example.com' "http://$VIP:$PORT/" | head -c 6)"
  sleep 0.05
done >"$PROBE") &
PROBER=$!
sleep 1
D11_FROM=$(($(log_lines) + 1))
kubectl -n web patch gateway direct-gw --type=json -p '[{"op":"add","path":"/spec/listeners/-","value":
  {"name":"https-on-http","protocol":"HTTPS","port":'"$PORT"',"tls":{"mode":"Terminate","certificateRefs":[{"kind":"Secret","name":"web-cert"}]}}}]' >/dev/null
if poll 60 ingress_ports_are "$PORT,$TLS_PORT"; then
  ok "the HTTP port is back on the ingress Service"
else
  bad "the HTTP port is back on the ingress Service" "$(kubectl -n web get svc direct-gw-ingress-service -o json | jq -c '[.spec.ports[]?.port]')"
fi
# A port added to a Service was not sent until something else about it changed.
for gw in "$IGW1_ADDR" "$IGW2_ADDR"; do
  if poll 90 only_l4_on "$gw"; then
    ok "on $gw, the ingress Service's L4 rule for it, and no host and path rule left"
  else
    bad "on $gw, the ingress Service's L4 rule for it, and no host and path rule left" "$(rule_keys "$gw")"
  fi
done
poll 30 no_rule_services
check "the rule Services are gone" "$(gen | jq '.items | length')" 0
sleep 3
touch "$PROBE.stop"
wait "$PROBER"

# In the log: the L4 rule on each gateway, after loxilb-ingress has the
# Ingresses, and the rule Services' deletes after both.
SENT=$(line_of "$D11_FROM" first "the L4 rule of port $PORT is sent: ")
L4_1=$(line_of_both "$D11_FROM" "loxilb-lb($IGW1_ADDR): add lb {{$VIP  $PORT tcp" "direct-gw-ingress-service")
L4_2=$(line_of_both "$D11_FROM" "loxilb-lb($IGW2_ADDR): add lb {{$VIP  $PORT tcp" "direct-gw-ingress-service")
GONE=$(line_of "$D11_FROM" first "gateway web/direct-gw: direct route service ")
if [[ $REPORTING -gt 0 ]]; then
  check "loxilb-ingress pods that report" "$REPORTING" "$(kubectl -n kube-system get pods -l app=loxilb-ingress -o name | wc -l)"
  if [[ $SENT -gt 0 ]] && klb_log | tail -n +"$D11_FROM" | sed -n "${SENT}p" | grep -qF "loxilb-ingress has the port's Ingresses" &&
     [[ $L4_1 -gt $SENT && $L4_2 -gt $SENT ]]; then
    ok "the L4 rule went to the gateways once loxilb-ingress had the port's Ingresses"
  else
    bad "the L4 rule went to the gateways once loxilb-ingress had the port's Ingresses" \
      "sent at log line $SENT ($(klb_log | tail -n +"$D11_FROM" | sed -n "${SENT}p" | cut -c1-200)), L4 rule at $L4_1 and $L4_2"
  fi
else
  note "no loxilb-ingress pod reports which Ingresses it has installed: the L4 rule was not held back for it"
fi
if [[ $L4_1 -gt 0 && $L4_2 -gt 0 && $GONE -gt $L4_1 && $GONE -gt $L4_2 ]]; then
  ok "the rule Services went once both gateways had the L4 rule"
else
  bad "the rule Services went once both gateways had the L4 rule" "L4 rule at log lines $L4_1 and $L4_2, rule Services deleted at $GONE"
fi
SENT_REQS=$(wc -l <"$PROBE")
MISSED=$(grep -vc ' web-a-$' "$PROBE")
if [[ $SENT_REQS -lt 20 ]]; then
  bad "requests to back.example.com while the port went back" "only $SENT_REQS sent"
elif [[ $MISSED -eq 0 ]]; then
  ok "each of $SENT_REQS requests to back.example.com was answered while the port went back"
elif [[ $REPORTING -gt 0 ]]; then
  bad "each request to back.example.com was answered while the port went back" \
    "$MISSED of $SENT_REQS were not: $(grep -v ' web-a-$' "$PROBE" | head -3 | tr '\n' ';')"
else
  note "$MISSED of $SENT_REQS requests to back.example.com went unanswered while the port went back: loxilb-ingress does not report, and its L4 rule was sent before it had the Ingress"
fi
kubectl -n web delete httproute back >/dev/null
kubectl -n web patch gateway direct-gw --type=json -p '[{"op":"remove","path":"/spec/listeners/2"}]' >/dev/null
if poll 90 ingress_ports_are "$TLS_PORT"; then
  ok "the HTTP port is with the direct routes again"
else
  bad "the HTTP port is with the direct routes again" "$(kubectl -n web get svc direct-gw-ingress-service -o json | jq -c '[.spec.ports[]?.port]')"
fi
for gw in "$IGW1_ADDR" "$IGW2_ADDR"; do
  poll 60 rules_are "$gw" "$WANT2"
  check "the host and path rules on $gw, no L4 rule" "$(rule_keys "$gw")" "$WANT2"
done
BACK=$(line_of "$D11_FROM" first "$PORTS_CHANGED")
REL=$(line_of "$D11_FROM" last "$RELEASED")
LEFT=$(line_of "$D11_FROM" last "$PORTS_CHANGED")
if [[ $BACK -gt 0 && $REL -gt $BACK && $LEFT -gt $REL ]]; then
  ok "its L4 rule came off again before the port left"
else
  bad "its L4 rule came off again before the port left" "back at log line $BACK, released at $REL, left at $LEFT"
fi
check "the port did not leave with its L4 rule still on" "$(count_of "$D11_FROM" "$DROPPED")" 0
case "$(body shop.example.com /api/x)" in web-a-*) ok "shop.example.com/api is served directly again";; *) bad "shop.example.com/api is served directly again" "$(ask shop.example.com /api/x)";; esac

echo "=== D12. loxilb-ingress restarts ==="
# A loxilb-ingress pod is ready once loxilb answers and the Ingresses there
# were when it started have been handled once (its readinessProbe,
# loxilb-ingress #22), and the ingress Service's endpoints name it from then.
# With one pod, as here, nothing answers while it restarts; from when the
# new pod is ready, each request on a new connection is answered.
PROBE="$WORK/d12-requests"
rm -f "$PROBE" "$PROBE.stop"
(while [[ ! -e "$PROBE.stop" ]]; do
  printf '%s %s\n' "$(date +%s.%N | cut -c1-14)" "$(curl -sk --max-time 2 --resolve "secure.example.com:$TLS_PORT:$VIP" "https://secure.example.com:$TLS_PORT/" | head -c 6)"
  sleep 0.05
done >"$PROBE") &
PROBER=$!
sleep 2
RESTART=$(date +%s)
kubectl -n kube-system rollout restart ds/loxilb-ingress >/dev/null
kubectl -n kube-system rollout status ds/loxilb-ingress --timeout=180s >/dev/null ||
  bad "loxilb-ingress restarted" "rollout did not finish"
poll 60 slice_names_ingress_pods
poll 30 tls_served
sleep 3
touch "$PROBE.stop"
wait "$PROBER"
NEW_POD=$(kubectl -n kube-system get pods -l app=loxilb-ingress -o json |
  jq -c '[.items[] | select(.metadata.deletionTimestamp == null)][0]')
READY=$(date -d "$(jq -r '.status.conditions[] | select(.type=="Ready") | .lastTransitionTime' <<<"$NEW_POD")" +%s)
MISSED=$(awk '$2 != "web-a-"' "$PROBE" | wc -l)
# Ready is to the second, and the endpoints follow it within one.
LATE=$(awk -v r="$READY" '$2 != "web-a-" && $1 > r + 2' "$PROBE" | wc -l)
if [[ "$(kubectl -n kube-system get ds loxilb-ingress -o json | jq '.spec.template.spec.containers[0].readinessProbe != null')" == true ]]; then
  check "no request sent after the new loxilb-ingress pod was ready went unanswered" "$LATE" 0
else
  note "loxilb-ingress has no readinessProbe: $LATE requests sent after its new pod was ready went unanswered"
fi
note "$MISSED of $(wc -l <"$PROBE") requests went unanswered while the one loxilb-ingress pod restarted; it was ready $((READY - RESTART))s after the restart"

echo "=== D13. rules the spec answers with a 5xx ==="
# A rule whose backend has no ready endpoint should be answered 503, as
# Gateway API advises, and one whose backend is invalid 500, as it requires
# (HTTPRouteRule.BackendRefs). kube-loxilb sends both as rules without
# endpoints where every gateway lists no_endpoint_rule in its version
# features. Elsewhere it sends neither - the gateway refuses a rule without
# endpoints - and the longest other prefix of the host, web-b's "/", takes
# their requests.
features_of() { curl -s --max-time 5 "$(rest "$1")/version" | jq -r '(.features // []) | join(",")'; }
TAKES=yes
for gw in "$IGW1_ADDR" "$IGW2_ADDR"; do
  [[ ",$(features_of "$gw")," == *",no_endpoint_rule,"* ]] || TAKES=no
done
gen_count_is() { [[ "$(gen | jq '.items | length')" == "$1" ]]; }
GEN_BEFORE=$(gen | jq '.items | length')
INSTALLED_BEFORE=$(installed_count)
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: Service
metadata: {name: web-down, namespace: web}
spec:
  selector: {app: nobody}
  ports: [{name: http, port: 80, targetPort: 8000}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: fivexx, namespace: web}
spec:
  parentRefs: [{name: direct-gw, sectionName: http}]
  hostnames: [shop.example.com]
  rules:
    - matches: [{path: {type: PathPrefix, value: /down}}]
      backendRefs: [{name: web-down, port: 80}]
    - matches: [{path: {type: PathPrefix, value: /missing}}]
      backendRefs: [{name: web-missing, port: 80}]
    - matches: [{path: {type: PathPrefix, value: /foreign}}]
      backendRefs: [{name: kubernetes, namespace: default, port: 443}]
YAML
answer_of() { echo "$(code shop.example.com "$1") $(body shop.example.com "$1" | head -c 40)"; }
if [[ $TAKES == yes ]]; then
  ok "both gateways take rules without endpoints"
  if poll 60 installed_is $((INSTALLED_BEFORE + 3)); then
    ok "the three rules are installed"
  else
    bad "the three rules are installed" "$(installed_count) installed, want $((INSTALLED_BEFORE + 3))"
  fi
  without_endpoints() { # <gateway addr> -> path:status of the rules on shop.example.com with none
    direct_rules "$1" | jq -r '[.[] | select(.serviceArguments.host=="shop.example.com" and ((.endpoints // []) | length) == 0)
                               | "\(.serviceArguments.path_prefix):\(.serviceArguments.no_endpoint_status)"] | sort | join(" ")'
  }
  for gw in "$IGW1_ADDR" "$IGW2_ADDR"; do
    check "on $gw, the rules without endpoints" "$(without_endpoints "$gw")" "/down:503 /foreign:500 /missing:500"
  done
  check "a request for the rule with no ready endpoint" "$(code shop.example.com /down/x)" 503
  check "a request for the rule whose Service does not exist" "$(code shop.example.com /missing)" 500
  check "a request for the rule whose Service it may not reach" "$(code shop.example.com /foreign)" 500
  ACC=$(route_cond fivexx Accepted)
  [[ "$ACC" == True/* ]] && ok "the route is accepted" || bad "the route is accepted" "$ACC"
  RES=$(route_cond fivexx ResolvedRefs)
  [[ "$RES" == False/*"web-missing"* && "$RES" == *"default"* ]] &&
    ok "its status names the invalid backends" || bad "its status names the invalid backends" "$RES"
  # Endpoints come: the rule is served by them; they go: 503 again.
  kubectl -n web patch svc web-down --type=merge -p '{"spec":{"selector":{"app":"web-a"}}}' >/dev/null
  if poll 30 served_by shop.example.com /down web-a; then
    ok "the rule's backend gets endpoints, and serves it"
  else
    bad "the rule's backend gets endpoints, and serves it" "$(answer_of /down)"
  fi
  kubectl -n web patch svc web-down --type=merge -p '{"spec":{"selector":{"app":"nobody"}}}' >/dev/null
  down_is_503() { [[ "$(code shop.example.com /down)" == 503 ]]; }
  if poll 30 down_is_503; then
    ok "its endpoints go, and it is answered 503 again"
  else
    bad "its endpoints go, and it is answered 503 again" "$(answer_of /down)"
  fi
else
  # The rule Services of /down and /missing come, not installed; /foreign,
  # whose backend is invalid, gets none.
  poll 30 gen_count_is $((GEN_BEFORE + 2))
  check "the rule Services of /down and /missing, and none for /foreign" "$(gen | jq '.items | length')" $((GEN_BEFORE + 2))
  sleep 3
  check "none of them is installed" "$(installed_count)" "$INSTALLED_BEFORE"
  for path in /down /missing /foreign; do
    ANSWER=$(answer_of "$path")
    case "$ANSWER" in
      5*) ok "a request for $path gets ${ANSWER%% *}";;
      *) note "a request for $path gets $ANSWER: the gateways do not list no_endpoint_rule, so its rule is not sent and the shorter prefix takes it";;
    esac
  done
fi
check "the HTTP port stays with the direct routes" \
  "$(kubectl -n web get svc direct-gw-ingress-service -o json | jq -r '[.spec.ports[]?.port] | join(",")')" "$TLS_PORT"
case "$(body shop.example.com /api/x)" in
  web-a-*) ok "shop.example.com/api is served beside them";;
  *) bad "shop.example.com/api is served beside them" "$(ask shop.example.com /api/x)";;
esac
kubectl -n web delete httproute fivexx >/dev/null
kubectl -n web delete svc web-down >/dev/null
poll 60 gen_count_is "$GEN_BEFORE"
check "the rule Services are as they were" "$(gen | jq '.items | length')" "$GEN_BEFORE"

echo "=== D14. a route in another namespace on the HTTPS listener ==="
# The HTTPS listener's certificate, web-cert, is in the Gateway's namespace,
# and loxilb-ingress reads an Ingress's TLS Secrets from the Ingress's own.
# So a route in another namespace gets its HTTPS listener's Ingress in web,
# naming the namespace of its backend there, and is served. Builds before
# this one put that Ingress in the route's namespace, where the Secret is
# not, and loxilb-ingress installed none of it.
# The backend is web-b's pods, through a Service without a selector in
# web-other: a Service selects pods in its own namespace only.
WEB_B=$(pods_of web-b)
kubectl create namespace web-other >/dev/null 2>&1
kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Service
metadata: {name: other, namespace: web-other}
spec:
  ports: [{name: http, port: 80, targetPort: 8000}]
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: other-1
  namespace: web-other
  labels: {kubernetes.io/service-name: other, endpointslice.kubernetes.io/managed-by: e2e-direct}
addressType: IPv4
ports: [{name: http, port: 8000, protocol: TCP}]
endpoints:
$(for ip in ${WEB_B//,/ }; do echo "  - {addresses: [\"$ip\"], conditions: {ready: true}}"; done)
YAML
# The route's HTTPS Ingress as a build before this one made it: in the
# route's namespace, without the route's namespace on it.
OLD_ING=other-legacy
kubectl apply -f - >/dev/null <<YAML
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: $OLD_ING
  namespace: web-other
  labels: {implementation: kube-loxilb}
  annotations:
    gateway-api-controller: loxilb.io/loxilb
    parent-gateway: direct-gw
    parent-gateway-namespace: web
    parent-listener: https
    parent-http-route: other
spec:
  ingressClassName: loxilb
  tls: [{secretName: web-cert, hosts: [secure.example.com]}]
  rules:
    - host: secure.example.com
      http:
        paths: [{path: /other, pathType: Prefix, backend: {service: {name: other, port: {number: 80}}}}]
YAML
HTTPS_IDX=$(kubectl -n web get gateway direct-gw -o json | jq '.spec.listeners | map(.name) | index("https")')
kubectl -n web patch gateway direct-gw --type=json \
  -p "[{\"op\":\"add\",\"path\":\"/spec/listeners/$HTTPS_IDX/allowedRoutes\",\"value\":{\"namespaces\":{\"from\":\"All\"}}}]" >/dev/null
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: other
  namespace: web-other
  # Names nothing, but is of a family whose Secrets loxilb-ingress would look
  # up in the Ingress's namespace: not carried to an Ingress in web.
  annotations: {loxilb.io/mtls-frontend-mode: disabled}
spec:
  parentRefs: [{name: direct-gw, namespace: web, sectionName: https}]
  hostnames: [secure.example.com]
  rules:
    - matches: [{path: {type: PathPrefix, value: /other}}]
      backendRefs: [{name: other, port: 80}]
YAML
other_ingress() { # the route's Ingress in web
  kubectl -n web get ingress -o json |
    jq -c '[.items[] | select(.metadata.annotations["parent-http-route"]=="other"
                             and .metadata.annotations["parent-http-route-namespace"]=="web-other")][0] // empty'
}
other_route_cond() {
  kubectl -n web-other get httproute other -o json |
    jq -r --arg t "$1" '.status.parents[]? | select(.controllerName=="loxilb.io/loxilb")
                        | .conditions[]? | select(.type==$t) | "\(.status)/\(.reason)/\(.message)"' | head -1
}
has_other_ingress() { [[ -n "$(other_ingress)" ]]; }
legacy_gone() { ! kubectl -n web-other get ingress "$OLD_ING" >/dev/null 2>&1; }
if poll 60 has_other_ingress; then
  ok "the route's HTTPS Ingress is in the Gateway's namespace"
else
  bad "the route's HTTPS Ingress is in the Gateway's namespace" "none in web after 60s"
fi
ING=$(other_ingress)
check "it names the backend's namespace" \
  "$(jq -r '[.metadata.annotations["external-backend-service"], .metadata.annotations["service-other-namespace"]] | join(",")' <<<"$ING")" "true,web-other"
check "with the listener's certificate" "$(jq -r '[.spec.tls[]?.secretName] | join(",")' <<<"$ING")" "web-cert"
check "without the route's mtls annotation" "$(jq -r '.metadata.annotations["loxilb.io/mtls-frontend-mode"] // "none"' <<<"$ING")" "none"
poll 30 legacy_gone
legacy_gone && ok "the Ingress a build before this one made in the route's namespace is gone" ||
  bad "the Ingress a build before this one made in the route's namespace is gone" "web-other/$OLD_ING is still there"
check "no Ingress in the route's namespace" "$(kubectl -n web-other get ingress -o json | jq '.items | length')" 0
ACC=$(other_route_cond Accepted)
[[ "$ACC" == True/Accepted/*"loxilb.io/mtls-frontend-mode are not carried"* ]] &&
  ok "the route is accepted, and says what its Ingress leaves out" || bad "the route is accepted, and says what its Ingress leaves out" "$ACC"
other_served() { tls_at /other/x | grep -q '^web-b-'; }
if poll 60 other_served; then
  ok "secure.example.com/other over HTTPS reaches the backend in web-other"
else
  bad "secure.example.com/other over HTTPS reaches the backend in web-other" "'$(tls_at /other/x | head -c 60)' $(tls_code /other/x)"
fi
case "$(tls_at /)" in
  web-a-*) ok "secure.example.com/ is still answered by web's own route";;
  *) bad "secure.example.com/ is still answered by web's own route" "'$(tls_at / | head -c 60)'";;
esac
# Taken back to routes from the Gateway's namespace only, the listener drops
# the route, and its Ingress in web goes.
kubectl -n web patch gateway direct-gw --type=json \
  -p "[{\"op\":\"remove\",\"path\":\"/spec/listeners/$HTTPS_IDX/allowedRoutes\"}]" >/dev/null
not_allowed() { [[ "$(other_route_cond Accepted)" == False/NotAllowedByListeners/* ]]; }
poll 30 not_allowed
not_allowed && ok "from its own namespace only, the listener does not take the route" ||
  bad "from its own namespace only, the listener does not take the route" "$(other_route_cond Accepted)"
no_other_ingress() { [[ -z "$(other_ingress)" ]]; }
poll 30 no_other_ingress
no_other_ingress && ok "and the route's Ingress in web goes" || bad "and the route's Ingress in web goes" "$(other_ingress | jq -r .metadata.name)"
kubectl delete namespace web-other --wait=false >/dev/null

echo
if [[ $fails -eq 0 ]]; then
  echo "SCENARIO-e2e-direct [OK]"
  exit 0
fi
echo "SCENARIO-e2e-direct [FAILED] ($fails check(s))"
exit 1
