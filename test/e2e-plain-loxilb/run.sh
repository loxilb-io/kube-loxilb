#!/bin/bash
# Run the whole comparison unattended and keep what it found.
#
#   ./run.sh            # detached: survives the terminal; prints where results go
#   ./run.sh --here     # in the foreground
#
# Stages: teardown of any previous testbed, config.sh, validation.sh for the
# baseline and then the candidate on the same testbed, diagnostics. Results go
# to _work/results/<timestamp>/, also _work/results/latest:
#
#   summary.txt     what was compared, each stage's exit code and time, every
#                   check side by side, and RESULT: PASS|FAIL for the candidate
#   <stage>.log     each stage's full output
#   diag/           kube-loxilb logs per build and phase, loxilb rules, events
#   done            written last; until it exists the run is still going
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

# Pin what is compared before anything runs.
KLB_BASE_REV="$(git -C "$REPO" rev-parse --short=7 "$KLB_BASE_REV")" || exit 1
KLB_CAND_REV="$(git -C "$REPO" rev-parse --short=7 "$KLB_CAND_REV")" || exit 1
export KLB_BASE_REV KLB_CAND_REV

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
say "baseline   $KLB_BASE_REV $(git -C "$REPO" log -1 --format=%s "$KLB_BASE_REV")"
say "candidate  $KLB_CAND_REV $(git -C "$REPO" log -1 --format=%s "$KLB_CAND_REV")"
say "loxilb     $LOXILB_IMAGE $(docker image inspect -f '{{index .RepoDigests 0}}' "$LOXILB_IMAGE" 2>/dev/null | sed 's/.*@//')"
say "k3s        $K3S_IMAGE"

configured=""
stage teardown "$HERE/rmconfig.sh"
stage config "$HERE/config.sh" && configured=yes
if [[ -n "$configured" ]]; then
  stage base "$HERE/validation.sh" "$KLB_BASE_REV"
  stage cand "$HERE/validation.sh" "$KLB_CAND_REV"
fi

collect() {
  kubectl get events -A --sort-by=.lastTimestamp > "$DIAG/events.txt" 2>&1
  kubectl get pods -A -o wide > "$DIAG/pods.txt" 2>&1
  local name addr
  for name in llb1 llb2; do
    addr="$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" "$name" 2>/dev/null)"
    [[ -n "$addr" ]] || continue
    curl -s --max-time 5 "$(rest "$addr")/config/loadbalancer/all" > "$DIAG/$name.rules.json"
    docker logs --tail 2000 "$name" > "$DIAG/$name.docker.log" 2>&1
  done
}
stage diag collect

if [[ -f "$RESULTS/base.log" && -f "$RESULTS/cand.log" ]]; then
  say ""
  python3 "$HERE/compare.py" "$RESULTS/base.log" "$RESULTS/cand.log" "$KLB_BASE_REV" "$KLB_CAND_REV" | tee -a "$SUMMARY"
fi
for log in base cand; do
  [[ -f "$RESULTS/$log.log" ]] || continue
  say ""
  say "--- $log"
  grep -E '\[FAIL\]|^SCENARIO-' "$RESULTS/$log.log" | tee -a "$SUMMARY"
done

say ""
if [[ -n "$configured" ]] && grep -q "^SCENARIO-e2e-plain-loxilb-.*\[OK\]" "$RESULTS/cand.log"; then
  say "RESULT: PASS"
else
  say "RESULT: FAIL"
fi
say "finished   $(date -Is)"
say "testbed left up; KUBECONFIG=$KUBECONFIG; remove with $HERE/rmconfig.sh"
date -Is > "$RESULTS/done"
