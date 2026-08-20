#!/bin/bash
# Endpoint churn: scale the backing deployment 2 -> 5 -> 1 while the agent runs,
# and record how often each binary reprograms the rules.
#
#   ./run_scale.sh <binary> <tag>
#
# f11-podnet carries loxilb.io/usepodnetwork so its endpoints are pod IPs -
# without it a single-node cluster keeps the same node-IP endpoint throughout
# and nothing churns.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/_out}"
BIN="$1"; TAG="$2"
KC="$OUT/kubeconfig.yaml"
POOL="${POOL:-defaultPool=123.123.123.1/24}"
k() { kubectl --kubeconfig "$KC" "$@"; }

k -n l1 delete svc --all --wait=true >/dev/null 2>&1
k -n l1 scale deploy/ep --replicas=2 >/dev/null
k apply -f "$HERE/fixtures.yaml" >/dev/null
k -n l1 rollout status deploy/ep --timeout=180s >/dev/null

rm -f "$OUT/rec-$TAG.jsonl"; touch "$OUT/rec-$TAG.jsonl"
FAKE_REC="$OUT/rec-$TAG.jsonl" python3 "$HERE/fake_loxilb.py" > "$OUT/fake-$TAG.log" 2>&1 &
FAKE_PID=$!; sleep 1
"$BIN" --config="$OUT/agent-conf.yaml" --loxiURL=http://127.0.0.1:11111 \
       --cidrPools="$POOL" --v=3 > "$OUT/log-$TAG.txt" 2>&1 &
AGENT_PID=$!

sleep 60; echo "[$TAG] steady: $(wc -l < "$OUT/rec-$TAG.jsonl")"
k -n l1 scale deploy/ep --replicas=5 >/dev/null
k -n l1 rollout status deploy/ep --timeout=180s >/dev/null
sleep 45; echo "[$TAG] after scale-out(5): $(wc -l < "$OUT/rec-$TAG.jsonl")"
k -n l1 scale deploy/ep --replicas=1 >/dev/null
k -n l1 rollout status deploy/ep --timeout=180s >/dev/null
sleep 45; echo "[$TAG] after scale-in(1): $(wc -l < "$OUT/rec-$TAG.jsonl")"

k -n l1 get svc -o json > "$OUT/svc-$TAG.json"
kill "$AGENT_PID" 2>/dev/null; wait "$AGENT_PID" 2>/dev/null
kill "$FAKE_PID" 2>/dev/null; wait "$FAKE_PID" 2>/dev/null
