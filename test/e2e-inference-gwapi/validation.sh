#!/bin/bash
# Assert what the InferencePool controller produced, on a live cluster and a
# live loxilb-inference-gateway.
#
# One sentinel at the end: SCENARIO-e2e-inference-gwapi [OK] or [FAILED].
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
IGW_ADDR="${IGW_ADDR:-12.12.12.1}"
VIP="${VIP:-12.12.12.1}"
GW_PORT="${GW_PORT:-8080}"
POOL_SVC="vllm-qwen3-inference"

fails=0
ok()   { echo "  [OK]   $1"; }
bad()  { echo "  [FAIL] $1${2:+ - $2}"; fails=$((fails+1)); }
check() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1" "got '$2', want '$3'"; fi; }

# poll <seconds> <command...> - retry until the command succeeds
poll() { local t=$1; shift; for ((i=0;i<t;i++)); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }

rules() { curl -s --max-time 5 "http://$IGW_ADDR:11111/netlox/v1/config/loadbalancer/all"; }
svc_json() { kubectl -n llm get svc "$1" -o json 2>/dev/null; }
pool_cond() { # <pool> <conditionType> -> status
  kubectl -n llm get inferencepool "$1" -o json 2>/dev/null |
    jq -r --arg t "$2" '.status.parents[]? | select(.controllerName=="loxilb.io/kube-loxilb")
                        | .conditions[]? | select(.type==$t) | .status' | head -1
}
pool_reason() {
  kubectl -n llm get inferencepool "$1" -o json 2>/dev/null |
    jq -r '.status.parents[]? | select(.controllerName=="loxilb.io/kube-loxilb")
           | .conditions[]? | select(.type=="Accepted") | .reason' | head -1
}

echo "=== 1. the pool became a Service ==="
if ! poll 90 kubectl -n llm get svc "$POOL_SVC"; then
  bad "service $POOL_SVC created" "not found after 90s"
else
  ok "service $POOL_SVC created"
  SVC=$(svc_json "$POOL_SVC")
  check "annotation lbmode"        "$(jq -r '.metadata.annotations["loxilb.io/lbmode"]' <<<"$SVC")"        "fullproxy"
  check "annotation usepodnetwork" "$(jq -r '.metadata.annotations["loxilb.io/usepodnetwork"]' <<<"$SVC")" "yes"
  check "annotation epselect"      "$(jq -r '.metadata.annotations["loxilb.io/epselect"]' <<<"$SVC")"      "chwbl"
  check "annotation model-name"    "$(jq -r '.metadata.annotations["loxilb.io/model-name"]' <<<"$SVC")"    "qwen3-32b"
  check "owner annotation"         "$(jq -r '.metadata.annotations["parent-inference-pool"]' <<<"$SVC")"   "vllm-qwen3"
  check "owner label"              "$(jq -r '.metadata.labels["loxilb.io/inference-pool"]' <<<"$SVC")"     "vllm-qwen3"
  check "selector from the pool"   "$(jq -r '.spec.selector.app' <<<"$SVC")"                               "vllm-qwen3"
  check "listener port"            "$(jq -r '.spec.ports[0].port' <<<"$SVC")"                              "$GW_PORT"
  check "pool targetPort"          "$(jq -r '.spec.ports[0].targetPort' <<<"$SVC")"                        "8000"
fi

echo "=== 2. the Service was given the gateway address ==="
if poll 90 bash -c "kubectl -n llm get svc $POOL_SVC -o json | jq -e '.status.loadBalancer.ingress[0]' >/dev/null"; then
  check "external IP" "$(svc_json "$POOL_SVC" | jq -r '.status.loadBalancer.ingress[0].hostname // .status.loadBalancer.ingress[0].ip')" "llb-$VIP"
else
  bad "external IP assigned" "still pending after 90s"
fi

echo "=== 3. loxilb programmed an inference rule ==="
# Selected by name, not by address: the rule has to be told apart from anything
# else sharing the gateway address.
RULE_NAME="llm_$POOL_SVC"
if ! poll 60 bash -c "curl -s --max-time 5 http://$IGW_ADDR:11111/netlox/v1/config/loadbalancer/all | jq -e --arg n '$RULE_NAME' '.lbAttr[]? | select(.serviceArguments.name|startswith(\$n))' >/dev/null"; then
  bad "rule $RULE_NAME present" "not found after 60s"
  rules | jq -c '.lbAttr[]?.serviceArguments | {name,externalIP,port}' 2>/dev/null | head -5
else
  ok "rule $RULE_NAME present"
  RULE=$(rules | jq -c --arg n "$RULE_NAME" '.lbAttr[] | select(.serviceArguments.name|startswith($n))' | head -1)
  check "rule address is the gateway's" "$(jq -r '.serviceArguments.externalIP' <<<"$RULE")" "$VIP"
  # 8 = CHWBL, 4 = fullproxy. Both are gateway-only, so their presence here is
  # the proof that the flavor was detected and the AI fields were not stripped.
  check "sel is chwbl(8)"     "$(jq -r '.serviceArguments.sel' <<<"$RULE")"        "8"
  check "mode is fullproxy(4)" "$(jq -r '.serviceArguments.mode' <<<"$RULE")"      "4"
  check "model_name on the rule" "$(jq -r '.serviceArguments.model_name' <<<"$RULE")" "qwen3-32b"
  EPS=$(jq -r '[.endpoints[]?.endpointIP] | sort | join(",")' <<<"$RULE")
  PODS=$(kubectl -n llm get pods -l app=vllm-qwen3 -o json | jq -r '[.items[].status.podIP] | sort | join(",")')
  check "endpoints are the pool's pods" "$EPS" "$PODS"
  # The proxy looks a pool up by host + path prefix + match mode. Leaving them
  # empty still produces a rule that reads back correctly and answers every
  # request with model_unavailable.
  check "routing key host"       "$(jq -r '.serviceArguments.host' <<<"$RULE")"            "$VIP"
  check "routing key path"       "$(jq -r '.serviceArguments.path_prefix' <<<"$RULE")"     "/"
  check "routing key match mode" "$(jq -r '.serviceArguments.path_match_mode' <<<"$RULE")" "prefix"
fi

echo "=== 3b. the pool owns the listener alone ==="
COUNT=$(rules | jq --arg ip "$VIP" '[.lbAttr[]? | select(.serviceArguments.externalIP==$ip and .serviceArguments.port==('"$GW_PORT"'))] | length')
# Two rules on one address and port is a race over which one the data plane
# binds - the gateway's own ingress service must yield the claimed listener.
check "exactly one rule on $VIP:$GW_PORT" "$COUNT" "1"
if kubectl -n llm get svc inference-gw-ingress-service >/dev/null 2>&1; then
  bad "no ingress service on the claimed listener" "inference-gw-ingress-service still exists"
else
  ok "no ingress service on the claimed listener"
fi

echo "=== 3c. fullproxy is listening on the VIP ==="
# mode=4 is only real if loxilb bound a socket for it; a rule that failed to
# bind still reads back exactly the same over REST.
if sudo ip netns exec igw1 ss -tln 2>/dev/null | grep -q "$VIP:$GW_PORT"; then
  ok "socket bound on $VIP:$GW_PORT"
else
  bad "socket bound on $VIP:$GW_PORT" "$(sudo ip netns exec igw1 ss -tln 2>/dev/null | tail -3 | tr '\n' ' ')"
fi

echo "=== 4. the pool reports its status ==="
if poll 60 bash -c "[[ \"\$(kubectl -n llm get inferencepool vllm-qwen3 -o json | jq -r '.status.parents | length')\" != '0' ]]"; then
  check "Accepted"     "$(pool_cond vllm-qwen3 Accepted)"     "True"
  check "ResolvedRefs" "$(pool_cond vllm-qwen3 ResolvedRefs)" "True"
  check "parent is the gateway" \
    "$(kubectl -n llm get inferencepool vllm-qwen3 -o json | jq -r '.status.parents[0].parentRef.name')" "inference-gw"
else
  bad "status written" "no parents after 60s"
fi

echo "=== 5. an Endpoint Picker with FailClose is refused ==="
kubectl apply -f "$HERE/fixtures/inferencepool-failclose.yaml" >/dev/null
kubectl apply -f "$HERE/fixtures/httproute-failclose.yaml" >/dev/null
if poll 60 bash -c "[[ \"\$(kubectl -n llm get inferencepool epp-required -o json | jq -r '.status.parents | length')\" != '0' ]]"; then
  check "Accepted is False"   "$(pool_cond epp-required Accepted)" "False"
  check "reason"              "$(pool_reason epp-required)"        "NotSupportedByParent"
  if kubectl -n llm get svc epp-required-inference >/dev/null 2>&1; then
    bad "no service for a refused pool" "epp-required-inference exists"
  else
    ok "no service for a refused pool"
  fi
else
  bad "refused pool reports status" "no parents after 60s"
fi

echo "=== 6. switching that pool to FailOpen accepts it ==="
kubectl apply -f "$HERE/fixtures/inferencepool-failopen.yaml" >/dev/null
if poll 90 kubectl -n llm get svc epp-required-inference; then
  ok "service appears once FailOpen is set"
  check "Accepted is True" "$(pool_cond epp-required Accepted)" "True"
else
  bad "service appears once FailOpen is set" "not found after 90s"
fi

echo "=== 7. removing the route removes the Service ==="
kubectl -n llm delete httproute epp-route >/dev/null 2>&1
if poll 90 bash -c "! kubectl -n llm get svc epp-required-inference >/dev/null 2>&1"; then
  ok "service removed with its route"
else
  bad "service removed with its route" "still present after 90s"
fi

# The Service going is only half of it. loxilb keys a rule by its host and
# path as well as the L4 tuple, so a delete addressed by the tuple alone is
# answered 404 no-rule: the rule stays, still bound, and kube-loxilb retries
# it forever. This pool names no model, so it is the plain /hosturl/ case.
if poll 60 bash -c "! curl -s --max-time 5 http://$IGW_ADDR:11111/netlox/v1/config/loadbalancer/all |
                      jq -e '.lbAttr[]? | select(.serviceArguments.name|startswith(\"llm_epp-required-inference\"))' >/dev/null"; then
  ok "its loxilb rule went with it"
else
  bad "its loxilb rule went with it" "$(rules | jq -c '.lbAttr[]?.serviceArguments|{name,host,model_name}' 2>/dev/null | tr '\n' ' ')"
fi

echo "=== 8. traffic reaches a model server through the VIP ==="
# The banner is the pod name, so this says which endpoint served - a 200 with
# no name would not distinguish "routed" from "answered by the proxy".
BODY='{"model":"qwen3-32b","messages":[{"role":"user","content":"ping"}]}'
PODS=$(kubectl -n llm get pods -l app=vllm-qwen3 -o jsonpath='{.items[*].metadata.name}')
served=""
for attempt in 1 2 3 4 5; do
  ANSWER=$(curl -s --max-time 8 -X POST "http://$VIP:$GW_PORT/v1/chat/completions" \
             -H 'Content-Type: application/json' -d "$BODY" 2>/dev/null)
  if grep -qw "${ANSWER:-__none__}" <<<"$PODS"; then served="$ANSWER"; break; fi
  sleep 5
done
if [[ -n "$served" ]]; then
  ok "served by pod $served"
else
  bad "traffic reaches a model server" "gateway answered '${ANSWER:-<empty>}'"
fi

# The same request with a model the rule does not carry must not be served by
# this pool - otherwise model_name is decoration.
OTHER=$(curl -s --max-time 8 -X POST "http://$VIP:$GW_PORT/v1/chat/completions" \
          -H 'Content-Type: application/json' -d '{"model":"not-this-one","messages":[]}' 2>/dev/null)
if grep -qw "${OTHER:-__none__}" <<<"$PODS"; then
  bad "an unknown model is refused" "served by $OTHER"
else
  ok "an unknown model is refused"
fi

echo "=== 9. removing the pool's route removes the rule carrying traffic ==="
# The model_name case, and the worst one: model_name is part of loxilb's rule
# key too, and has to ride along on the delete as a query param. Get it wrong
# and the rule outlives its Service, still bound to the VIP and still
# answering.
kubectl -n llm delete httproute llm-route >/dev/null 2>&1
if poll 90 bash -c "! kubectl -n llm get svc $POOL_SVC >/dev/null 2>&1"; then
  ok "service removed with its route"
else
  bad "service removed with its route" "still present after 90s"
fi
if poll 60 bash -c "[ \"\$(curl -s --max-time 5 http://$IGW_ADDR:11111/netlox/v1/config/loadbalancer/all |
                          jq '[.lbAttr[]? | select(.serviceArguments.externalIP==\"$VIP\" and .serviceArguments.port==$GW_PORT)] | length')\" = 0 ]"; then
  ok "no rule left on $VIP:$GW_PORT"
else
  bad "no rule left on $VIP:$GW_PORT" "$(rules | jq -c '.lbAttr[]?.serviceArguments|{name,model_name}' 2>/dev/null | tr '\n' ' ')"
fi

echo
if [[ $fails -eq 0 ]]; then
  echo "SCENARIO-e2e-inference-gwapi [OK]"
  exit 0
fi
echo "SCENARIO-e2e-inference-gwapi [FAILED] ($fails check(s))"
exit 1
