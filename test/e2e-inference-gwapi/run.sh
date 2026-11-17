#!/bin/bash
# Run the whole scenario unattended and keep what it found.
#
#   ./run.sh            # detached: survives the terminal; prints where results go
#   ./run.sh --here     # in the foreground
#
# Stages: teardown of any previous testbed, config.sh, validation.sh,
# validation-auth.sh, then diagnostics. A failing validation does not stop the
# next one. Results go to _work/results/<timestamp>/, also _work/results/latest:
#
#   summary.txt  what was tested, each stage's exit code and time, every
#                [FAIL] and [NOTE] line, the sentinels, and RESULT: PASS|FAIL
#   <stage>.log  each stage's full output
#   diag/        kube-loxilb logs, gateway rules and profiles, events, objects
#   done         written last; until it exists the run is still going
#
# The testbed is left up for a look afterwards; ./rmconfig.sh removes it.
set -u
source "$(dirname "$0")/env.sh"
HERE="$E2E_HERE"

if [[ "${1:-}" != "--here" ]]; then
  results="$WORK/results/$(date +%Y%m%d-%H%M%S)"
  mkdir -p "$results"
  RESULTS="$results" setsid nohup "$0" --here > "$results/run.log" 2>&1 < /dev/null &
  echo "running detached (pid $!)"
  echo "results: $results"
  exit 0
fi

RESULTS="${RESULTS:-$WORK/results/$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$RESULTS/diag"
ln -sfn "$RESULTS" "$WORK/results/latest"
export DIAG="$RESULTS/diag"
SUMMARY="$RESULTS/summary.txt"
say() { echo "$*" | tee -a "$SUMMARY"; }

# Pin what is tested before anything runs, so the summary names it even if
# the checkout moves on meanwhile.
KLB_REV="$(git -C "$REPO" rev-parse --short=7 "$KLB_REV")" || exit 1
KLB_OLD_REV="$(git -C "$REPO" rev-parse --short=7 "$KLB_OLD_REV")" || exit 1
export KLB_REV KLB_OLD_REV

# stage <name> <command...> - run one stage into <name>.log
stage() {
  local name=$1 start rc
  shift
  start=$(date +%s)
  "$@" > "$RESULTS/$name.log" 2>&1
  rc=$?
  say "$(printf 'stage %-16s rc=%d  %4ds' "$name" "$rc" $(( $(date +%s) - start )))"
  return $rc
}

say "started    $(date -Is)"
say "kube-loxilb $KLB_REV $(git -C "$REPO" log -1 --format=%s "$KLB_REV")"
say "upgrade from $KLB_OLD_REV $(git -C "$REPO" log -1 --format=%s "$KLB_OLD_REV")"

if [[ -z "$IGW_IMAGE" && -n "${IGW_REPO:-}" ]]; then
  stage build-igw "$HERE/build-igw.sh"
  IGW_IMAGE="$(cat "$WORK/igw-image" 2>/dev/null)"
fi
export IGW_IMAGE
say "gateway    ${IGW_IMAGE:-<none>}"
say "old build  $IGW_OLD_IMAGE"
say "k3s        $K3S_IMAGE"
say "key store  $IGW_KEYSTORE${EPP_SRC:+; Endpoint Picker from $EPP_SRC}${GW_LBMODE:+; Gateway lbmode $GW_LBMODE}"

configured=""
auth_skipped=""
stage teardown "$HERE/rmconfig.sh"
stage config "$HERE/config.sh" && configured=yes
if [[ -n "$configured" ]]; then
  stage validation "$HERE/validation.sh"
  # The credential checks need a gateway that lists both credential features.
  features="$(curl -s --max-time 5 "$(rest "$IGW1_ADDR")/version" | jq -c '.features // []')"
  if jq -e 'index("api_key_auth") and index("jwt_auth_profile")' <<<"$features" >/dev/null; then
    stage validation-auth "$HERE/validation-auth.sh"
  else
    auth_skipped="the gateway lists $features, not api_key_auth and jwt_auth_profile"
    say "stage validation-auth    SKIPPED: $auth_skipped"
  fi
fi

collect() {
  kubectl -n kube-system logs deploy/kube-loxilb --tail=-1 > "$DIAG/kube-loxilb.log" 2>&1
  kubectl -n kube-system logs deploy/kube-loxilb --previous > "$DIAG/kube-loxilb.previous.log" 2>&1
  kubectl get events -A --sort-by=.lastTimestamp > "$DIAG/events.txt" 2>&1
  kubectl get svc,inferencepool,httproute -A -o yaml > "$DIAG/objects.yaml" 2>&1
  kubectl get jwtauthprofiles -o yaml > "$DIAG/jwtauthprofiles.yaml" 2>&1
  kubectl get pods -A -o wide > "$DIAG/pods.txt" 2>&1
  local name addr
  for name in igw1 igw2; do
    addr="$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" "$name" 2>/dev/null)"
    [[ -n "$addr" ]] || continue
    curl -s --max-time 5 "$(rest "$addr")/version" > "$DIAG/$name.version.json"
    curl -s --max-time 5 "$(rest "$addr")/config/loadbalancer/all" > "$DIAG/$name.rules.json"
    curl -s --max-time 5 "$(rest "$addr")/config/ai/jwtauthprofile" > "$DIAG/$name.profiles.json"
    docker logs --tail 2000 "$name" > "$DIAG/$name.docker.log" 2>&1
  done
}
stage diag collect

for log in validation validation-auth; do
  [[ -f "$RESULTS/$log.log" ]] || continue
  say ""
  say "--- $log"
  grep -E '\[FAIL\]|\[NOTE\]|\[SKIPPED\]|^SCENARIO-' "$RESULTS/$log.log" | tee -a "$SUMMARY"
done

say ""
if [[ -n "$configured" ]] &&
  grep -q '^SCENARIO-e2e-inference-gwapi \[OK\]' "$RESULTS/validation.log" &&
  { [[ -n "$auth_skipped" ]] || grep -q '^SCENARIO-e2e-inference-gwapi-auth \[OK\]' "$RESULTS/validation-auth.log"; }; then
  say "RESULT: PASS"
else
  say "RESULT: FAIL"
fi
say "finished   $(date -Is)"
say "testbed left up; KUBECONFIG=$KUBECONFIG; remove with $HERE/rmconfig.sh"
date -Is > "$RESULTS/done"
