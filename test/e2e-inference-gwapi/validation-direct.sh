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
# EndpointSlice after it: until that is written, the manual.example.com rule
# has no endpoints and is not installed, so the HTTP port stays on the
# ingress Service and its L4 rule reaches the gateways. The checks below see
# the port move to the rules once it is written, and that L4 rule go.
sleep 2
kubectl apply -f "$HERE/fixtures/direct-gateway.yaml" >/dev/null
if poll 90 l4_rules_are 1; then
  ok "the HTTP port is on the ingress Service's L4 rule first"
else
  bad "the HTTP port is on the ingress Service's L4 rule first" "$(rule_keys "$IGW1_ADDR")"
fi
manual_slice "$WEB_B"

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
if poll 90 tls_served; then
  ok "secure.example.com over HTTPS: through the ingress Service and loxilb-ingress, to web-a"
else
  bad "secure.example.com over HTTPS: through the ingress Service and loxilb-ingress, to web-a" "'$(tls_body)'"
fi

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
kubectl -n web delete httproute manual >/dev/null
if poll 60 host_rules_are manual.example.com 0; then
  ok "manual.example.com's rule is gone"
else
  bad "manual.example.com's rule is gone" "$(rule_keys "$IGW1_ADDR")"
fi
check "manual.example.com is not served" "$(code manual.example.com /)" "503"
check "the other rules stay" "$(installed_count)" "2"

echo
if [[ $fails -eq 0 ]]; then
  echo "SCENARIO-e2e-direct [OK]"
  exit 0
fi
echo "SCENARIO-e2e-direct [FAILED] ($fails check(s))"
exit 1
