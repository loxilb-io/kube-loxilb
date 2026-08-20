#!/bin/bash
# Full A/B: run the same fixtures through a baseline and a candidate binary and
# report what changed on the wire.
#
#   ./run_ab.sh <baseline-binary> <candidate-binary>
#
# Build the baseline from whatever ref you are comparing against, e.g.
#   git worktree add /tmp/wt-main main
#   (cd /tmp/wt-main && go build -o /tmp/kube-loxilb-main ./cmd/loxilb-agent)
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/_out}"
BASE="$1"; CAND="$2"
export OUT

"$HERE/setup.sh"

echo
echo "############ 1. steady-state payload ############"
"$HERE/run_case.sh" "$BASE" base normal plain 70
"$HERE/run_case.sh" "$CAND" cand normal plain 70
python3 "$HERE/compare.py" "$OUT/rec-base.jsonl" "$OUT/rec-cand.jsonl" base cand

echo
echo "############ 2. loxilb rejection handling ############"
# 409 is the steady state of re-creating an existing rule; 404 is a rule that
# is really gone. A client that cannot tell them apart silently drops one case.
for m in conflict notfound; do
  "$HERE/run_case.sh" "$BASE" "base-$m" "$m" plain 45
  "$HERE/run_case.sh" "$CAND" "cand-$m" "$m" plain 45
done
echo "############ 3. peer without /version ############"
"$HERE/run_case.sh" "$BASE" base-noversion normal missing 45
"$HERE/run_case.sh" "$CAND" cand-noversion normal missing 45

echo
echo "############ 4. endpoint churn ############"
"$HERE/run_scale.sh" "$BASE" base-scale
"$HERE/run_scale.sh" "$CAND" cand-scale
python3 "$HERE/compare.py" "$OUT/rec-base-scale.jsonl" "$OUT/rec-cand-scale.jsonl" base cand

echo
echo "############ summary ############"
python3 "$HERE/summarize.py" "$OUT" \
  base cand base-conflict cand-conflict base-notfound cand-notfound \
  base-noversion cand-noversion base-scale cand-scale
