#!/bin/bash
# Assert what the InferencePool controller produced, on a live cluster and a
# live loxilb-inference-gateway.
#
# One sentinel at the end: SCENARIO-e2e-inference-gwapi [OK] or [FAILED].
set -u
source "$(dirname "$0")/env.sh"
HERE="$E2E_HERE"
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
pool_reason() { # <pool> [conditionType] -> reason
  kubectl -n llm get inferencepool "$1" -o json 2>/dev/null |
    jq -r --arg t "${2:-Accepted}" '.status.parents[]? | select(.controllerName=="loxilb.io/kube-loxilb")
           | .conditions[]? | select(.type==$t) | .reason' | head -1
}
rule_of() { # <service> -> the loxilb rule kube-loxilb programmed for it
  rules | jq -c --arg n "llm_$1" '.lbAttr[]? | select(.serviceArguments.name|startswith($n))' | head -1
}
rule_epp() { rule_of "$1" | jq -r '.serviceArguments.eppEndpoint // ""'; }
svc_ip() { kubectl -n llm get svc "$1" -o jsonpath='{.spec.clusterIP}' 2>/dev/null; }
# ask <model> - one chat request through the VIP; prints the body, which the
# stand-in model servers make the name of the pod that answered.
ask() {
  curl -s --max-time 8 -X POST "http://$VIP:$GW_PORT/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d "{\"model\":\"$1\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}" 2>/dev/null
}
# The Endpoint Picker checks need a gateway that applies the EPP fields and
# the stand-in picker config.sh deploys when EPP_SRC is given.
epp_ready() {
  curl -s --max-time 5 "http://$IGW_ADDR:11111/netlox/v1/version" | jq -e '.features // [] | index("epp")' >/dev/null &&
    kubectl -n llm get deploy epp >/dev/null 2>&1
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
  check "no picker on a pool that names none" "$(jq -r '.serviceArguments.eppEndpoint // ""' <<<"$RULE")" ""
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
if in_netns igw1 ss -tln 2>/dev/null | grep -q "$VIP:$GW_PORT"; then
  ok "socket bound on $VIP:$GW_PORT"
else
  bad "socket bound on $VIP:$GW_PORT" "$(in_netns igw1 ss -tln 2>/dev/null | tail -3 | tr '\n' ' ')"
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

echo "=== 4a. nothing is rewritten while nothing changes ==="
# Each status write used to be an update event that queued the pool again, so
# a pool and its Service were rewritten every couple of seconds forever. Give
# the testbed a few windows to settle; one quiet window is the pass.
quiet=""
for window in 1 2 3; do
  P1=$(kubectl -n llm get inferencepool vllm-qwen3 -o jsonpath='{.metadata.resourceVersion}')
  S1=$(kubectl -n llm get svc "$POOL_SVC" -o jsonpath='{.metadata.resourceVersion}')
  sleep 15
  P2=$(kubectl -n llm get inferencepool vllm-qwen3 -o jsonpath='{.metadata.resourceVersion}')
  S2=$(kubectl -n llm get svc "$POOL_SVC" -o jsonpath='{.metadata.resourceVersion}')
  if [[ "$P1" == "$P2" && "$S1" == "$S2" ]]; then quiet=yes; break; fi
done
if [[ -n "$quiet" ]]; then
  ok "pool and Service unchanged for 15s"
else
  bad "pool and Service unchanged for 15s" "pool $P1 -> $P2, service $S1 -> $S2 in the last window"
fi

echo "=== 4b. a picker annotation cannot stand in for endpointPickerRef ==="
# The address and failure mode belong to the pool's spec. Written by hand on a
# pool that names no picker, they must not reach the Service or the rule.
kubectl -n llm annotate inferencepool vllm-qwen3 loxilb.io/epp-endpoint=10.99.99.99:9002 --overwrite >/dev/null
sleep 10
check "not copied to the Service" \
  "$(svc_json "$POOL_SVC" | jq -r '.metadata.annotations["loxilb.io/epp-endpoint"] // ""')" ""
# An absent rule would also read as "no picker", so the rule has to be there.
R=$(rule_of "$POOL_SVC")
if [[ -z "$R" ]]; then
  bad "not on the rule" "no rule for $POOL_SVC"
else
  check "not on the rule" "$(jq -r '.serviceArguments.eppEndpoint // ""' <<<"$R")" ""
fi
kubectl -n llm annotate inferencepool vllm-qwen3 loxilb.io/epp-endpoint- >/dev/null

if ! epp_ready; then
  echo "=== 5-6. Endpoint Picker checks [SKIPPED] ==="
  echo "  needs a gateway listing \"epp\" in /version features and EPP_SRC at config time"
  # Check 7 still needs a pool without a model name to delete.
  kubectl apply -f "$HERE/fixtures/inferencepool-failopen.yaml" -f "$HERE/fixtures/httproute-failclose.yaml" >/dev/null
  poll 90 kubectl -n llm get svc epp-required-inference || bad "FailOpen pool served" "not found after 90s"
else
  # EPP_ALONE=1 takes the main pool's route away for checks 5-6d. Two pools
  # on one listener cannot share its address yet - the second waits with
  # ExternalIPUnavailable and never gets a rule - so the picker's pool is
  # given the listener to itself, and the main pool comes back for 7-9.
  if [[ -n "${EPP_ALONE:-}" ]]; then
    echo "  [NOTE] EPP_ALONE: route llm-route removed for checks 5-6d"
    kubectl -n llm delete httproute llm-route >/dev/null
    poll 90 bash -c "! kubectl -n llm get svc $POOL_SVC >/dev/null 2>&1" || bad "main pool's Service gone for EPP_ALONE"
  fi
  EPP_ADDR="$(svc_ip epp):9002"
  EPP_DEST_IP=$(kubectl -n llm get deploy epp -o json | jq -r '.spec.template.spec.containers[0].args as $a
                  | $a[($a | index("-dest")) + 1]' | cut -d: -f1)
  EPP_POD=$(kubectl -n llm get pods -o json | jq -r --arg ip "$EPP_DEST_IP" '.items[] | select(.status.podIP==$ip) | .metadata.name')

  echo "=== 5. a FailClose pool is routed by its Endpoint Picker ==="
  kubectl apply -f "$HERE/fixtures/inferencepool-failclose.yaml" -f "$HERE/fixtures/httproute-failclose.yaml" >/dev/null
  if poll 90 bash -c "[[ \"\$(curl -s --max-time 5 http://$IGW_ADDR:11111/netlox/v1/config/loadbalancer/all |
                           jq -r '.lbAttr[]? | select(.serviceArguments.name|startswith(\"llm_epp-required-inference\"))
                                  | .serviceArguments.eppEndpoint')\" == '$EPP_ADDR' ]]"; then
    ok "rule carries eppEndpoint $EPP_ADDR"
  else
    bad "rule carries eppEndpoint $EPP_ADDR" "got '$(rule_epp epp-required-inference)'"
  fi
  check "Accepted"     "$(pool_cond epp-required Accepted)"     "True"
  check "ResolvedRefs" "$(pool_cond epp-required ResolvedRefs)" "True"
  check "Service names the picker" \
    "$(svc_json epp-required-inference | jq -r '.metadata.annotations["loxilb.io/epp-endpoint"]')" "$EPP_ADDR"
  check "rule failure mode" "$(rule_of epp-required-inference | jq -r '.serviceArguments.eppFailureMode')" "FailClose"
  # Any model but the main pool's lands on this catch-all rule. Every answer
  # has to come from the one pod the picker names; the pool's own round-robin
  # would spread them over both. The prompt varies so that nothing keyed on
  # the request body could pin them to one pod either.
  if [[ -z "$EPP_POD" ]]; then
    bad "the pod the picker names exists" "no pod has $EPP_DEST_IP any more; rerun config.sh"
  else
    answers=""
    for i in 1 2 3 4 5 6; do answers+="$(ask "epp-check-$i") "; done
    check "every request served by the pod the picker named" "$(tr ' ' '\n' <<<"$answers" | sort -u | grep -v '^$')" "$EPP_POD"
  fi

  echo "=== 5b. FailClose with the picker down answers 503 ==="
  kubectl -n llm scale deploy epp --replicas=0 >/dev/null
  if poll 60 bash -c "curl -s --max-time 8 -X POST http://$VIP:$GW_PORT/v1/chat/completions \
                        -H 'Content-Type: application/json' -d '{\"model\":\"epp-check\",\"messages\":[]}' |
                      grep -q epp_unavailable"; then
    ok "503 epp_unavailable while the picker is down"
  else
    bad "503 epp_unavailable while the picker is down" "got '$(ask epp-check)'"
  fi
  kubectl -n llm scale deploy epp --replicas=1 >/dev/null
  kubectl -n llm rollout status deploy/epp --timeout=120s >/dev/null
  if poll 60 bash -c "[[ \"\$(curl -s --max-time 8 -X POST http://$VIP:$GW_PORT/v1/chat/completions \
                              -H 'Content-Type: application/json' -d '{\"model\":\"epp-check\",\"messages\":[]}')\" == '$EPP_POD' ]]"; then
    ok "routed by the picker again once it is back"
  else
    bad "routed by the picker again once it is back" "got '$(ask epp-check)'"
  fi

  echo "=== 5c. a picker that never answers is cut off at epp-timeout-ms ==="
  # The deadline has no field in the API and travels as a pool annotation. A
  # picker that hangs shows whether it reached the gateway: at 1000 ms the 503
  # comes back well before the 3000 ms default.
  EPP_ARGS=$(kubectl -n llm get deploy epp -o json | jq -c '.spec.template.spec.containers[0].args')
  kubectl -n llm annotate inferencepool epp-required loxilb.io/epp-timeout-ms=1000 --overwrite >/dev/null
  if poll 60 bash -c "[[ \"\$(curl -s --max-time 5 http://$IGW_ADDR:11111/netlox/v1/config/loadbalancer/all |
                           jq -r '.lbAttr[]? | select(.serviceArguments.name|startswith(\"llm_epp-required-inference\"))
                                  | .serviceArguments.eppTimeoutMs // \"\"')\" == '1000' ]]"; then
    ok "rule carries eppTimeoutMs 1000"
  else
    bad "rule carries eppTimeoutMs 1000" "got '$(rule_of epp-required-inference | jq -r '.serviceArguments.eppTimeoutMs')'"
  fi
  kubectl -n llm patch deploy epp --type json -p \
    "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":$(jq -c '. + ["-mode","hang"]' <<<"$EPP_ARGS")}]" >/dev/null
  kubectl -n llm rollout status deploy/epp --timeout=120s >/dev/null
  if poll 30 bash -c "curl -s --max-time 8 -X POST http://$VIP:$GW_PORT/v1/chat/completions \
                        -H 'Content-Type: application/json' -d '{\"model\":\"epp-check\",\"messages\":[]}' | grep -q epp_unavailable"; then
    T=$(curl -s -o /dev/null -w '%{time_total}' --max-time 8 -X POST "http://$VIP:$GW_PORT/v1/chat/completions" \
          -H 'Content-Type: application/json' -d '{"model":"epp-check","messages":[]}')
    # At least the deadline (a refused dial would answer at once), and well
    # under the default it replaced.
    if awk -v t="$T" 'BEGIN { exit !(t >= 0.9 && t < 2.5) }'; then
      ok "503 at the 1000 ms deadline (${T}s)"
    else
      bad "503 at the 1000 ms deadline" "took ${T}s"
    fi
  else
    bad "503 epp_unavailable from a hanging picker" "got '$(ask epp-check)'"
  fi
  kubectl -n llm patch deploy epp --type json -p \
    "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":$EPP_ARGS}]" >/dev/null
  kubectl -n llm rollout status deploy/epp --timeout=120s >/dev/null
  kubectl -n llm annotate inferencepool epp-required loxilb.io/epp-timeout-ms- >/dev/null
  poll 60 bash -c "[[ \"\$(curl -s --max-time 8 -X POST http://$VIP:$GW_PORT/v1/chat/completions \
                        -H 'Content-Type: application/json' -d '{\"model\":\"epp-check\",\"messages\":[]}')\" == '$EPP_POD' ]]" ||
    bad "routed by the picker after the hang" "got '$(ask epp-check)'"

  echo "=== 6. a FailOpen pool whose picker Service is missing is served without it ==="
  kubectl apply -f "$HERE/fixtures/inferencepool-failopen.yaml" >/dev/null
  if poll 60 bash -c "[[ \"\$(kubectl -n llm get inferencepool epp-required -o json |
                           jq -r '.status.parents[]? | .conditions[]? | select(.type==\"ResolvedRefs\") | .status')\" == 'False' ]]"; then
    ok "ResolvedRefs is False"
  else
    bad "ResolvedRefs is False" "got '$(pool_cond epp-required ResolvedRefs)'"
  fi
  check "ResolvedRefs reason" "$(pool_reason epp-required ResolvedRefs)" "InvalidExtensionRef"
  check "Accepted"            "$(pool_cond epp-required Accepted)"       "True"
  # The rule has to exist and carry no picker; an absent rule is not "no picker".
  if poll 60 bash -c "curl -s --max-time 5 http://$IGW_ADDR:11111/netlox/v1/config/loadbalancer/all |
                      jq -e '[.lbAttr[]? | select(.serviceArguments.name|startswith(\"llm_epp-required-inference\"))]
                             | length == 1 and ((.[0].serviceArguments.eppEndpoint // \"\") == \"\")' >/dev/null"; then
    ok "rule carries no picker"
  else
    bad "rule carries no picker" "got '$(rule_epp epp-required-inference)'"
  fi
  PODS=$(kubectl -n llm get pods -l app=vllm-qwen3 -o jsonpath='{.items[*].metadata.name}')
  ANSWER=$(ask epp-check)
  if grep -qw "${ANSWER:-__none__}" <<<"$PODS"; then ok "served by pod $ANSWER"; else bad "served without the picker" "got '$ANSWER'"; fi

  echo "=== 6b. the picker Service arriving is noticed ==="
  kubectl apply -f "$HERE/fixtures/epp-some-service.yaml" >/dev/null
  poll 30 bash -c "[[ -n \"\$(kubectl -n llm get svc some-epp -o jsonpath='{.spec.clusterIP}')\" ]]"
  SOME_ADDR="$(svc_ip some-epp):9002"
  if poll 60 bash -c "[[ \"\$(curl -s --max-time 5 http://$IGW_ADDR:11111/netlox/v1/config/loadbalancer/all |
                           jq -r '.lbAttr[]? | select(.serviceArguments.name|startswith(\"llm_epp-required-inference\"))
                                  | .serviceArguments.eppEndpoint')\" == '$SOME_ADDR' ]]"; then
    ok "rule picks up $SOME_ADDR"
  else
    bad "rule picks up $SOME_ADDR" "got '$(rule_epp epp-required-inference)'"
  fi
  check "ResolvedRefs" "$(pool_cond epp-required ResolvedRefs)" "True"

  echo "=== 6c. the picker Service recreated at a new address is followed ==="
  OLD_IP=$(svc_ip some-epp)
  kubectl -n llm delete svc some-epp >/dev/null
  kubectl apply -f "$HERE/fixtures/epp-some-service.yaml" >/dev/null
  poll 30 bash -c "[[ -n \"\$(kubectl -n llm get svc some-epp -o jsonpath='{.spec.clusterIP}')\" ]]"
  NEW_ADDR="$(svc_ip some-epp):9002"
  if [[ "$(svc_ip some-epp)" == "$OLD_IP" ]]; then
    echo "  [SKIP] the API server handed back the same cluster IP; nothing to follow"
  elif poll 60 bash -c "[[ \"\$(curl -s --max-time 5 http://$IGW_ADDR:11111/netlox/v1/config/loadbalancer/all |
                             jq -r '.lbAttr[]? | select(.serviceArguments.name|startswith(\"llm_epp-required-inference\"))
                                    | .serviceArguments.eppEndpoint')\" == '$NEW_ADDR' ]]"; then
    ok "rule follows to $NEW_ADDR"
  else
    bad "rule follows to $NEW_ADDR" "got '$(rule_epp epp-required-inference)'"
  fi

  echo "=== 6d. a FailClose pool whose picker Service goes away is answered 503 ==="
  kubectl -n llm patch inferencepool epp-required --type merge \
    -p '{"spec":{"endpointPickerRef":{"failureMode":"FailClose"}}}' >/dev/null
  poll 60 bash -c "[[ \"\$(kubectl -n llm get svc epp-required-inference -o json |
                       jq -r '.metadata.annotations[\"loxilb.io/epp-failure-mode\"]')\" == 'FailClose' ]]" ||
    bad "pool switched to FailClose" "Service still says $(svc_json epp-required-inference | jq -r '.metadata.annotations["loxilb.io/epp-failure-mode"]')"
  kubectl -n llm delete svc some-epp >/dev/null
  # The rule stays, pointed at an address that always refuses, so clients get
  # a 503 from the gateway instead of a refused connection - what the Inference
  # Extension's conformance expects of a pool whose picker Service is missing.
  if poll 60 bash -c "[[ \"\$(curl -s --max-time 5 http://$IGW_ADDR:11111/netlox/v1/config/loadbalancer/all |
                           jq -r '.lbAttr[]? | select(.serviceArguments.name|startswith(\"llm_epp-required-inference\"))
                                  | .serviceArguments.eppEndpoint // \"\"')\" == '127.0.0.1:1' ]]"; then
    ok "rule points at the refusing address"
  else
    bad "rule points at the refusing address" "got '$(rule_epp epp-required-inference)'"
  fi
  check "Service kept"  "$(kubectl -n llm get svc epp-required-inference -o name 2>/dev/null)" "service/epp-required-inference"
  check "ResolvedRefs"  "$(pool_cond epp-required ResolvedRefs)" "False"
  if poll 30 bash -c "curl -s --max-time 8 -X POST http://$VIP:$GW_PORT/v1/chat/completions \
                        -H 'Content-Type: application/json' -d '{\"model\":\"epp-check\",\"messages\":[]}' | grep -q epp_unavailable"; then
    ok "503 epp_unavailable without the picker Service"
  else
    bad "503 epp_unavailable without the picker Service" "got '$(ask epp-check)'"
  fi

  # Back to FailOpen, so check 7 has a pool without a model name to delete,
  # served by loxilb's own selection.
  kubectl -n llm patch inferencepool epp-required --type merge \
    -p '{"spec":{"endpointPickerRef":{"failureMode":"FailOpen"}}}' >/dev/null
  poll 60 bash -c "[[ -z \"\$(curl -s --max-time 5 http://$IGW_ADDR:11111/netlox/v1/config/loadbalancer/all |
                           jq -r '.lbAttr[]? | select(.serviceArguments.name|startswith(\"llm_epp-required-inference\"))
                                  | .serviceArguments.eppEndpoint // \"\"')\" ]]" ||
    bad "FailOpen pool served without the picker again" "got '$(rule_epp epp-required-inference)'"
  if [[ -n "${EPP_ALONE:-}" ]]; then
    # 7 removes the picker's pool; with it gone the main pool can take the
    # listener back for 8-9.
    kubectl -n llm delete httproute epp-route >/dev/null 2>&1
    poll 90 bash -c "! kubectl -n llm get svc epp-required-inference >/dev/null 2>&1"
    kubectl apply -f "$HERE/fixtures/httproute.yaml" >/dev/null
    poll 90 bash -c "curl -s --max-time 5 http://$IGW_ADDR:11111/netlox/v1/config/loadbalancer/all |
      jq -e '.lbAttr[]? | select(.serviceArguments.name|startswith(\"llm_$POOL_SVC\"))' >/dev/null" ||
      bad "main pool back after EPP_ALONE" "no rule after 90s"
  fi
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
# The Gateway's own ingress Service takes the listener back once no pool
# claims it, so its rule is left out: what must not be left is the pool's.
if poll 60 bash -c "[ \"\$(curl -s --max-time 5 http://$IGW_ADDR:11111/netlox/v1/config/loadbalancer/all |
                          jq '[.lbAttr[]? | select(.serviceArguments.externalIP==\"$VIP\" and .serviceArguments.port==$GW_PORT)
                               | select(.serviceArguments.name|startswith(\"llm_inference-gw-ingress-service\")|not)] | length')\" = 0 ]"; then
  ok "no pool rule left on $VIP:$GW_PORT"
else
  bad "no pool rule left on $VIP:$GW_PORT" "$(rules | jq -c '.lbAttr[]?.serviceArguments|{name,model_name}' 2>/dev/null | tr '\n' ' ')"
fi

echo
if [[ $fails -eq 0 ]]; then
  echo "SCENARIO-e2e-inference-gwapi [OK]"
  exit 0
fi
echo "SCENARIO-e2e-inference-gwapi [FAILED] ($fails check(s))"
exit 1
