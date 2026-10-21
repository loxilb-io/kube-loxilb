#!/bin/bash
# Credential policy end to end: JWTAuthProfile objects, the rules naming them,
# requests through the VIP, and what kube-loxilb does when a peer misses,
# loses or cannot hold what it was sent.
#
# Runs after validation.sh on the same testbed, which leaves no pool behind.
# igw1 carries the traffic; igw2 is replaced, paused and downgraded, and only
# its REST state is checked.
#
# One sentinel at the end: SCENARIO-e2e-inference-gwapi-auth [OK] or [FAILED].
set -u
source "$(dirname "$0")/env.sh"
HERE="$E2E_HERE"
FIX="$HERE/fixtures"
GW_PORT="${GW_PORT:-8080}"
R1="$(rest "$IGW1_ADDR")"
R2="$(rest "$IGW2_ADDR")"
AUTH_SVC=vllm-auth-inference
OPEN_SVC=vllm-open-inference
DIAG="${DIAG:-$WORK/diag}"
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

# --- gateway state ---------------------------------------------------------
rules_on() { curl -s --max-time 5 "$1/config/loadbalancer/all"; }
# rules_of <rest> <service> - the rules programmed for a Service, one per line
rules_of() { rules_on "$1" | jq -c --arg n "llm_$2" '.lbAttr[]? | select(.serviceArguments.name|startswith($n))'; }
rule_of() { rules_of "$1" "$2" | head -1; }
arg_of() { rule_of "$1" "$2" | jq -r --arg f "$3" '.serviceArguments[$f] // ""'; }
count_of() { rules_of "$1" "$2" | jq -s length; }
no_rule() { [[ "$(count_of "$1" "$2")" == 0 ]]; }
rule_has() { [[ "$(arg_of "$1" "$2" "$3")" == "$4" ]]; } # <rest> <svc> <field> <value>
# rule_open <rest> <svc> - the rule is there, with no credential policy
rule_open() { local r; r="$(rule_of "$1" "$2")"; [[ -n "$r" ]] && [[ "$(jq -r '.serviceArguments.api_key_auth // ""' <<<"$r")" == "" ]]; }
# ids_of <rest> <svc> - "port=id,..." for every rule of the Service
ids_of() { rules_of "$1" "$2" | jq -r '"\(.serviceArguments.port)=\(.serviceArguments.id // "")"' | sort | paste -sd, -; }
is_uuid5() { [[ "$1" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]]; }
has_profile() {
  curl -s --max-time 5 "$1/config/ai/jwtauthprofile" |
    jq -e --arg n "$2" '[.. | objects | select(.name? == $n)] | length > 0' >/dev/null
}
lacks_profile() { ! has_profile "$@"; }

# --- cluster state ---------------------------------------------------------
has_event() { # <kind> <name> <reason>
  kubectl get events -A -o json 2>/dev/null | jq -e --arg k "$1" --arg n "$2" --arg r "$3" \
    '[.items[] | select(.involvedObject.kind==$k and .involvedObject.name==$n and .reason==$r)] | length > 0' >/dev/null
}
profile_json() { kubectl get jwtauthprofile kc -o json 2>/dev/null; }
profile_programmed() {
  [[ "$(profile_json | jq -r '.status.conditions[]? | select(.type=="Programmed") | .status')" == True ]]
}
programmed_peers() { profile_json | jq -r '[.status.peers[]? | select(.programmed) | .host] | sort | join(",")'; }
on_both_peers() { [[ "$(programmed_peers)" == "$IGW1_ADDR,$IGW2_ADDR" ]]; }
profile_gone() { ! kubectl get jwtauthprofile kc >/dev/null 2>&1; }
svc_gone() { ! kubectl -n llm get svc "$1" >/dev/null 2>&1; }

klb_log() { kubectl -n kube-system logs deploy/kube-loxilb --tail=-1 2>/dev/null; }
klb_pod() { kubectl -n kube-system get pods -l app=kube-loxilb-app -o jsonpath='{.items[0].metadata.name}'; }
klb_restarts() { kubectl -n kube-system get pods -l app=kube-loxilb-app -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}'; }
log_count() { klb_log | grep -cF -- "$1"; }
log_count_gt() { [[ "$(log_count "$1")" -gt "$2" ]]; }
# readds <address> <service> - how often a rule was found wrong on read-back
# and re-added on that peer
# (the rule name goes on as ":llb-inst0")
readds() { klb_log | grep -F "loxilb-lb($1): rule llm_$2" | grep -c "deleting it and adding it again"; }
readds_gt() { [[ "$(readds "$1" "$2")" -gt "$3" ]]; }
save_log() { klb_log > "$DIAG/kube-loxilb.$1.log"; }

# set_klb <image> - run that kube-loxilb, and wait until it sees both peers
set_klb() {
  kubectl -n kube-system set image deploy/kube-loxilb "kube-loxilb=$1" >/dev/null &&
    kubectl -n kube-system rollout status deploy/kube-loxilb --timeout=180s >/dev/null &&
    poll 60 both_alive
}
both_alive() { log_count_gt "loxilb-lb($IGW1_ADDR) is alive" 0 && log_count_gt "loxilb-lb($IGW2_ADDR) is alive" 0; }

IGW2_DOWN="loxilb-lb($IGW2_ADDR) is down"
IGW2_UP="loxilb-lb($IGW2_ADDR) is alive"
# swap_igw2 <image> - replace the second peer with a fresh container, waiting
# for kube-loxilb to see it go and come back. A replacement faster than its
# health check would not be a peer coming back at all.
swap_igw2() {
  local downs ups
  downs=$(log_count "$IGW2_DOWN")
  ups=$(log_count "$IGW2_UP")
  docker rm -f igw2 >/dev/null
  poll 30 log_count_gt "$IGW2_DOWN" "$downs" || { echo "  kube-loxilb did not see igw2 go"; return 1; }
  run_igw igw2 "$IGW2_ADDR" "$1" || return 1
  poll 60 log_count_gt "$IGW2_UP" "$ups" || { echo "  kube-loxilb did not see igw2 back"; return 1; }
}

# --- requests --------------------------------------------------------------
mint() { python3 "$HERE/oidc-stub.py" mint "$WORK/oidc" "$OIDC_ISSUER" --aud "$OIDC_AUDIENCE" "$@"; }
BODY_FILE="$WORK/auth-body"
# ask <token> <model> - the status of one chat request through the VIP; the
# body, which the stand-in model servers make their pod name, is in BODY_FILE
ask() {
  local auth=()
  [[ -n "$1" ]] && auth=(-H "Authorization: Bearer $1")
  : > "$BODY_FILE"
  curl -s --max-time 8 -o "$BODY_FILE" -w '%{http_code}' "${auth[@]}" -X POST \
    "http://$VIP:$GW_PORT/v1/chat/completions" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$2\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}" 2>/dev/null
}
answers() { [[ "$(ask "$1" "$2")" == "$3" ]]; } # <token> <model> <status>
body() { head -c 200 "$BODY_FILE" | tr '\n' ' '; }

GOOD="$(mint --roles model:qwen3-32b)"
OTHER="$(mint --roles model:another-model)"
FOREIGN="$(mint --roles model:qwen3-32b --foreign)"
ELSEWHERE="$(mint --roles model:qwen3-32b --aud someone-else)"

echo "=== A0. both peers are gateways with the credential features ==="
for r in "$R1" "$R2"; do
  check "$r lists api_key_auth and jwt_auth_profile" \
    "$(curl -s --max-time 5 "$r/version" | jq -c '[.features // [] | .[] | select(. == "api_key_auth" or . == "jwt_auth_profile")] | sort')" \
    '["api_key_auth","jwt_auth_profile"]'
done

echo "=== A1. a kube-loxilb from before rule ids is upgraded: ids bound in place ==="
OLD_IMG="$(klb_image "$KLB_OLD_REV")"
NEW_IMG="$(klb_image "$KLB_REV")"
if ! set_klb "$OLD_IMG"; then
  bad "kube-loxilb $KLB_OLD_REV running" "rollout or peers not seen"
fi
kubectl apply -f "$FIX/two-port.yaml" >/dev/null
two_on() { [[ "$(count_of "$1" two-port)" == 2 ]]; }
two_on_both() { two_on "$R1" && two_on "$R2"; }
expect "kube-loxilb $KLB_OLD_REV programmed both ports on both peers" 90 two_on_both
note "ids before the upgrade: igw1 $(ids_of "$R1" two-port); igw2 $(ids_of "$R2" two-port)"
save_log a1-old

set_klb "$NEW_IMG" || bad "kube-loxilb $KLB_REV running" "rollout or peers not seen"
ids_bound() {
  local ids
  ids="$(rules_of "$R1" two-port | jq -r '.serviceArguments.id // ""' | sort -u)"
  [[ "$(wc -l <<<"$ids")" == 2 ]] || return 1
  while read -r id; do is_uuid5 "$id" || return 1; done <<<"$ids"
  [[ "$(ids_of "$R1" two-port)" == "$(ids_of "$R2" two-port)" ]]
}
expect "each port has its own UUIDv5 id, the same on both peers" 90 ids_bound
A1_IDS="$(ids_of "$R1" two-port)"
note "ids after: $A1_IDS"
check "still two rules on igw1" "$(count_of "$R1" two-port)" 2
check "still two rules on igw2" "$(count_of "$R2" two-port)" 2
save_log a1-new
check "nothing deleted on the way" \
  "$(klb_log | grep -E 'delete lb|deleteAll lb|deleting it and adding it again|Failed to delete LoxiLB rule' | grep -c two-port)" 0
has_event Service two-port LoxiLBRejected && bad "no add refused" "LoxiLBRejected on two-port" || ok "no add refused"

kubectl -n kube-system rollout restart deploy/kube-loxilb >/dev/null
kubectl -n kube-system rollout status deploy/kube-loxilb --timeout=180s >/dev/null
poll 60 both_alive
sleep 10
check "ids unchanged across a restart" "$(ids_of "$R1" two-port)" "$A1_IDS"

kubectl delete -f "$FIX/two-port.yaml" >/dev/null
two_gone() { no_rule "$R1" two-port && no_rule "$R2" two-port; }
expect "deleting the Service removes both rules from both peers" 60 two_gone

echo "=== A2. a rule naming a JWT auth profile with no JWTAuthProfile is held ==="
kubectl apply -f "$FIX/inferencepool-auth.yaml" >/dev/null
if poll 90 kubectl -n llm get svc "$AUTH_SVC"; then
  SVC="$(kubectl -n llm get svc "$AUTH_SVC" -o json)"
  check "api-key-auth copied to the Service" "$(jq -r '.metadata.annotations["loxilb.io/api-key-auth"]' <<<"$SVC")" jwt
  check "jwt-auth-profile copied to the Service" "$(jq -r '.metadata.annotations["loxilb.io/jwt-auth-profile"]' <<<"$SVC")" kc
else
  bad "service $AUTH_SVC created" "not found after 90s"
fi
expect "ProfileNotFound on the pool" 60 has_event InferencePool vllm-auth ProfileNotFound
expect "ProfileNotFound on the Service" 10 has_event Service "$AUTH_SVC" ProfileNotFound
no_rule "$R1" "$AUTH_SVC" && ok "no rule on igw1" || bad "no rule on igw1" "$(rule_of "$R1" "$AUTH_SVC")"
no_rule "$R2" "$AUTH_SVC" && ok "no rule on igw2" || bad "no rule on igw2" "$(rule_of "$R2" "$AUTH_SVC")"

echo "=== A3. a JWTAuthProfile goes to both peers, and the rule after it ==="
sed -e "s|__ISSUER__|$OIDC_ISSUER|" -e "s|__AUDIENCE__|$OIDC_AUDIENCE|" "$FIX/jwtauthprofile.yaml" |
  kubectl apply -f - >/dev/null
expect "Programmed=True" 60 profile_programmed
expect "programmed on both peers" 30 on_both_peers
check "finalizer held" "$(profile_json | jq -r '.metadata.finalizers // [] | index("jwtauthprofile.loxilb.io/peers") != null')" true
has_profile "$R1" kc && ok "igw1 holds profile kc" || bad "igw1 holds profile kc"
has_profile "$R2" kc && ok "igw2 holds profile kc" || bad "igw2 holds profile kc"
# The gateway refuses a rule naming a profile it does not hold, so a rule
# there is also the proof that the profile went first.
expect "rule on igw1 names kc" 60 rule_has "$R1" "$AUTH_SVC" jwt_auth_profile kc
expect "rule on igw2 names kc" 60 rule_has "$R2" "$AUTH_SVC" jwt_auth_profile kc
check "api_key_auth on igw1" "$(arg_of "$R1" "$AUTH_SVC" api_key_auth)" jwt
check "api_key_auth on igw2" "$(arg_of "$R2" "$AUTH_SVC" api_key_auth)" jwt
check "mode is fullproxy(4)" "$(arg_of "$R1" "$AUTH_SVC" mode)" 4
A_ID1="$(arg_of "$R1" "$AUTH_SVC" id)"
A_ID2="$(arg_of "$R2" "$AUTH_SVC" id)"
is_uuid5 "$A_ID1" && ok "rule id is a UUIDv5 ($A_ID1)" || bad "rule id is a UUIDv5" "got '$A_ID1'"
check "same id on both peers" "$A_ID2" "$A_ID1"

echo "=== A4. requests through the VIP are held to the profile ==="
PODS="$(kubectl -n llm get pods -l app=vllm-qwen3 -o jsonpath='{.items[*].metadata.name}')"
# The keyset is fetched after the profile is registered; until then the
# gateway answers 503.
if poll 30 answers "$GOOD" qwen3-32b 200; then
  if grep -qw "$(cat "$BODY_FILE")" <<<"$PODS"; then
    ok "a token with the model's role is served (by $(cat "$BODY_FILE"))"
  else
    bad "a token with the model's role is served by a pool pod" "body '$(body)'"
  fi
else
  bad "a token with the model's role is served" "got $(ask "$GOOD" qwen3-32b) $(body)"
fi
check "no token: 401" "$(ask "" qwen3-32b)" 401
check "a token without the model's role: 403" "$(ask "$OTHER" qwen3-32b)" 403
check "a token signed by another key: 401" "$(ask "$FOREIGN" qwen3-32b)" 401
check "a token for another audience: 401" "$(ask "$ELSEWHERE" qwen3-32b)" 401

echo "=== A5. a rule removed from a peer behind kube-loxilb's back is put back ==="
# kube-loxilb looks at the rules on a reconcile, so the Service is touched to
# get one; nothing about the rule changes.
NAME2="$(rule_of "$R2" "$AUTH_SVC" | jq -r '.serviceArguments.name')"
curl -s --max-time 5 -X DELETE "$R2/config/loadbalancer/name/$NAME2" >/dev/null
expect "removed from igw2 over REST" 10 no_rule "$R2" "$AUTH_SVC"
kubectl -n llm annotate svc "$AUTH_SVC" e2e.loxilb.io/touch="$(date +%s)" --overwrite >/dev/null
expect "put back on igw2" 60 rule_has "$R2" "$AUTH_SVC" jwt_auth_profile kc
check "with its id" "$(arg_of "$R2" "$AUTH_SVC" id)" "$A_ID2"
check "with its api_key_auth" "$(arg_of "$R2" "$AUTH_SVC" api_key_auth)" jwt
expect "RuleMissing on the Service" 10 has_event Service "$AUTH_SVC" RuleMissing
check "igw1 untouched" "$(arg_of "$R1" "$AUTH_SVC" id)" "$A_ID1"

echo "=== A6. a fresh igw2, holding nothing, gets the profile and then the rule ==="
POD_BEFORE="$(klb_pod)"
RESTARTS_BEFORE="$(klb_restarts)"
swap_igw2 "$IGW_IMAGE" || bad "igw2 replaced and seen back"
expect "profile kc registered on the fresh igw2" 90 has_profile "$R2" kc
expect "rule back on the fresh igw2, naming kc" 90 rule_has "$R2" "$AUTH_SVC" jwt_auth_profile kc
check "with its id" "$(arg_of "$R2" "$AUTH_SVC" id)" "$A_ID2"
check "kube-loxilb not restarted" "$(klb_pod)/$(klb_restarts)" "$POD_BEFORE/$RESTARTS_BEFORE"
expect "igw2 programmed in the status" 30 on_both_peers
check "igw1 untouched" "$(arg_of "$R1" "$AUTH_SVC" id)" "$A_ID1"
check "still served on igw1" "$(ask "$GOOD" qwen3-32b)" 200
save_log a6

echo "=== A7. a peer without the credential features is not given the rule ==="
if ! docker image inspect "$IGW_OLD_IMAGE" >/dev/null 2>&1; then
  echo "  [SKIPPED] $IGW_OLD_IMAGE is not a local image"
else
  # A plain Service alongside, to see what the old build does take.
  kubectl apply -f "$FIX/two-port.yaml" >/dev/null
  poll 60 two_on_both
  swap_igw2 "$IGW_OLD_IMAGE" || bad "igw2 downgraded and seen back"
  V="$(curl -s --max-time 5 "$R2/version")"
  check "igw2 is a gateway build" "$(jq -r '.product // ""' <<<"$V")" loxilb-inference-gateway
  check "igw2 lists no credential features" \
    "$(jq -c '[.features // [] | .[] | select(. == "api_key_auth" or . == "jwt_auth_profile")]' <<<"$V")" "[]"
  expect "AuthNotSupported on the pool" 60 has_event InferencePool vllm-auth AuthNotSupported
  expect "AuthNotSupported on the Service" 10 has_event Service "$AUTH_SVC" AuthNotSupported
  sleep 5
  no_rule "$R2" "$AUTH_SVC" && ok "the rule is not sent to igw2" || bad "the rule is not sent to igw2" "$(rule_of "$R2" "$AUTH_SVC")"
  expect "plain rules still go to igw2" 60 two_on "$R2"
  check "igw1 still serves a token" "$(ask "$GOOD" qwen3-32b)" 200
  check "igw1 still refuses no token" "$(ask "" qwen3-32b)" 401
  # Whether the old build keeps the id it is sent decides whether the rule
  # is found when read back; one that does not is re-added on every install.
  sleep 5
  n="$(readds "$IGW2_ADDR" two-port)"
  if [[ "$n" == 0 ]]; then
    ok "the old build keeps the ids it is sent: nothing re-added"
  else
    note "the old build does not keep the ids it is sent: two-port re-added $n time(s) on install"
  fi
  save_log a7-old

  swap_igw2 "$IGW_IMAGE" || bad "igw2 upgraded and seen back"
  expect "upgraded igw2 gets profile kc" 90 has_profile "$R2" kc
  expect "and the rule, naming kc" 90 rule_has "$R2" "$AUTH_SVC" jwt_auth_profile kc
  check "with its id" "$(arg_of "$R2" "$AUTH_SVC" id)" "$A_ID2"
  kubectl delete -f "$FIX/two-port.yaml" >/dev/null
  expect "two-port removed from both peers" 60 two_gone
fi

echo "=== A8. a peer that missed a policy change is put right when it is back ==="
# igw2's REST port is dropped, not paused: a paused container's kernel still
# takes connections, and what was sent meanwhile runs the moment it resumes,
# so it would miss nothing.
downs=$(log_count "$IGW2_DOWN")
ups=$(log_count "$IGW2_UP")
in_netns igw2 iptables -I INPUT -p tcp --dport 11111 -j DROP
expect "kube-loxilb saw igw2 go" 30 log_count_gt "$IGW2_DOWN" "$downs"
kubectl -n llm annotate inferencepool vllm-auth loxilb.io/api-key-auth- loxilb.io/jwt-auth-profile- >/dev/null
expect "igw1 rule loses the policy" 60 rule_open "$R1" "$AUTH_SVC"
expect "served on igw1 without a token" 30 answers "" qwen3-32b 200
check "igw1 id kept" "$(arg_of "$R1" "$AUTH_SVC" id)" "$A_ID1"
# igw2 still holds the rule with the policy. Sent again without it once it is
# back, the gateway keeps the stored api_key_auth, and only reading the rule
# back finds that.
readds2=$(readds "$IGW2_ADDR" "$AUTH_SVC")
in_netns igw2 iptables -D INPUT -p tcp --dport 11111 -j DROP
expect "kube-loxilb saw igw2 back" 60 log_count_gt "$IGW2_UP" "$ups"
expect "igw2 put right" 60 rule_open "$R2" "$AUTH_SVC"
readds_gt "$IGW2_ADDR" "$AUTH_SVC" "$readds2" && ok "igw2: found on read-back and re-added" ||
  bad "igw2: found on read-back and re-added" "no read-back line"
check "igw2 id kept" "$(arg_of "$R2" "$AUTH_SVC" id)" "$A_ID2"
# How long a sync of this Service took while igw2 did not answer.
slowest="$(klb_log | grep -oE "Finished syncing LoadBalancer service $AUTH_SVC\. \([0-9.]+s\)" |
  grep -oE '[0-9.]+s' | sort -n | tail -1)"
note "slowest sync of $AUTH_SVC so far: ${slowest:-under 1s}"
save_log a8

kubectl -n llm annotate inferencepool vllm-auth loxilb.io/api-key-auth=jwt loxilb.io/jwt-auth-profile=kc --overwrite >/dev/null
expect "policy back on igw1" 60 rule_has "$R1" "$AUTH_SVC" jwt_auth_profile kc
expect "policy back on igw2" 60 rule_has "$R2" "$AUTH_SVC" jwt_auth_profile kc
expect "no token refused again" 30 answers "" qwen3-32b 401

echo "=== A9. two credential policies on one listener: the older pool keeps it ==="
kubectl apply -f "$FIX/inferencepool-open.yaml" >/dev/null
poll 90 kubectl -n llm get svc "$OPEN_SVC" || bad "service $OPEN_SVC created" "not found after 90s"
expect "ListenerAuthConflict on the younger pool" 60 has_event InferencePool vllm-open ListenerAuthConflict
no_rule "$R1" "$OPEN_SVC" && ok "no rule for it on igw1" || bad "no rule for it on igw1" "$(rule_of "$R1" "$OPEN_SVC")"
no_rule "$R2" "$OPEN_SVC" && ok "no rule for it on igw2" || bad "no rule for it on igw2" "$(rule_of "$R2" "$OPEN_SVC")"
check "the older pool's rule stays" "$(arg_of "$R1" "$AUTH_SVC" jwt_auth_profile)" kc
check "and is served" "$(ask "$GOOD" qwen3-32b)" 200

echo "=== A10. a JWTAuthProfile still named by a rule is kept ==="
kubectl delete jwtauthprofile kc --wait=false >/dev/null
expect "ProfileInUse on the JWTAuthProfile" 60 has_event JWTAuthProfile kc ProfileInUse
check "kept, marked for deletion" "$(profile_json | jq -r '.metadata.deletionTimestamp != null')" true
has_profile "$R1" kc && ok "still on igw1" || bad "still on igw1"
has_profile "$R2" kc && ok "still on igw2" || bad "still on igw2"
check "still served" "$(ask "$GOOD" qwen3-32b)" 200

echo "=== A9b. the older pool goes; the younger one takes the listener ==="
kubectl delete -f "$FIX/inferencepool-auth.yaml" >/dev/null
expect "older pool's Service removed" 90 svc_gone "$AUTH_SVC"
auth_gone() { no_rule "$R1" "$AUTH_SVC" && no_rule "$R2" "$AUTH_SVC"; }
expect "and its rules, from both peers" 60 auth_gone
expect "younger pool installed on igw1" 60 rule_open "$R1" "$OPEN_SVC"
expect "and on igw2" 60 rule_open "$R2" "$OPEN_SVC"
expect "served without a token" 30 answers "" qwen3-open 200

echo "=== A10b. once nothing names it, the JWTAuthProfile goes, from both peers ==="
# The delete is retried with backoff while the profile is named, up to 120s
# apart.
expect "JWTAuthProfile deleted" 180 profile_gone
expect "profile kc gone from igw1" 30 lacks_profile "$R1" kc
expect "profile kc gone from igw2" 30 lacks_profile "$R2" kc

echo "=== A11. cleanup ==="
kubectl delete -f "$FIX/inferencepool-open.yaml" >/dev/null
# The Gateway's own ingress Service takes the listener back; its rule is not
# one of these.
listener_free() {
  for r in "$R1" "$R2"; do
    [[ "$(rules_on "$r" | jq --arg ip "$VIP" "[.lbAttr[]? | select(.serviceArguments.externalIP==\$ip and .serviceArguments.port==$GW_PORT)
          | select(.serviceArguments.name|startswith(\"llm_inference-gw-ingress-service\")|not)] | length")" == 0 ]] || return 1
  done
}
expect "no pool rule left on $VIP:$GW_PORT on either peer" 90 listener_free
save_log end

echo
if [[ $fails -eq 0 ]]; then
  echo "SCENARIO-e2e-inference-gwapi-auth [OK]"
  exit 0
fi
echo "SCENARIO-e2e-inference-gwapi-auth [FAILED] ($fails check(s))"
exit 1
