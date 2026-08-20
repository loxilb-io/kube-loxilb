#!/bin/bash
# Record one kube-loxilb binary's wire traffic against a fake loxilb.
#
#   ./run_case.sh <binary> <tag> [FAKE_MODE] [FAKE_VERSION] [SETTLE_SECS]
#
#   FAKE_MODE     normal (default) | conflict (409) | notfound (404)
#   FAKE_VERSION  plain (default)  | missing (/version 404) | gateway
#
# Writes _out/rec-<tag>.jsonl (requests), _out/log-<tag>.txt (agent log) and
# _out/svc-<tag>.json (Service objects after settling).
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/_out}"
BIN="$1"; TAG="$2"; MODE="${3:-normal}"; VMODE="${4:-plain}"; SETTLE="${5:-70}"
KC="$OUT/kubeconfig.yaml"
POOL="${POOL:-defaultPool=123.123.123.1/24}"
k() { kubectl --kubeconfig "$KC" "$@"; }

echo "=== [$TAG] reset fixtures ==="
# Services are deleted and re-created so each run starts from an unassigned
# status: a Service that already carries an ingress IP short-circuits the
# allocation path we want to compare.
k -n l1 delete svc --all --wait=true >/dev/null 2>&1
k -n l1 scale deploy/ep --replicas=2 >/dev/null
k apply -f "$HERE/fixtures.yaml" >/dev/null
k -n l1 rollout status deploy/ep --timeout=180s >/dev/null

rm -f "$OUT/rec-$TAG.jsonl"; touch "$OUT/rec-$TAG.jsonl"
echo "=== [$TAG] start fake loxilb (mode=$MODE version=$VMODE) ==="
FAKE_REC="$OUT/rec-$TAG.jsonl" FAKE_MODE="$MODE" FAKE_VERSION="$VMODE" \
  python3 "$HERE/fake_loxilb.py" > "$OUT/fake-$TAG.log" 2>&1 &
FAKE_PID=$!
sleep 1

echo "=== [$TAG] start kube-loxilb ($BIN) ==="
"$BIN" --config="$OUT/agent-conf.yaml" \
       --loxiURL=http://127.0.0.1:11111 \
       --cidrPools="$POOL" \
       --v=3 > "$OUT/log-$TAG.txt" 2>&1 &
AGENT_PID=$!

sleep "$SETTLE"
k -n l1 get svc -o json > "$OUT/svc-$TAG.json"
kill "$AGENT_PID" 2>/dev/null; wait "$AGENT_PID" 2>/dev/null
kill "$FAKE_PID"  2>/dev/null; wait "$FAKE_PID"  2>/dev/null

echo "=== [$TAG] done: $(wc -l < "$OUT/rec-$TAG.jsonl") recorded requests ==="
