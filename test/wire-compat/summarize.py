#!/usr/bin/env python3
"""One-line summary per recorded case: how many requests went out, how many
Services ended up with an external IP, and how many create failures were logged.

  ./summarize.py <out-dir> <tag> [<tag>...]
"""
import json, os, sys

out = sys.argv[1]
print(f"{'case':<22} {'requests':>8} {'svc w/ extIP':>13} {'create errors':>14}")
for tag in sys.argv[2:]:
    rec = os.path.join(out, f"rec-{tag}.jsonl")
    svc = os.path.join(out, f"svc-{tag}.json")
    log = os.path.join(out, f"log-{tag}.txt")

    n = sum(1 for _ in open(rec)) if os.path.exists(rec) else 0
    ing = total = 0
    if os.path.exists(svc):
        items = json.load(open(svc))["items"]
        total = len(items)
        ing = sum(1 for i in items
                  if (i["status"].get("loadBalancer") or {}).get("ingress"))
    errs = 0
    if os.path.exists(log):
        errs = sum(1 for line in open(log, errors="replace")
                   if "failed to create load-balancer" in line)
    print(f"{tag:<22} {n:>8} {f'{ing}/{total}':>13} {errs:>14}")
