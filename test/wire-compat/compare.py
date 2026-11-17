#!/usr/bin/env python3
"""Compare two recorded kube-loxilb wire streams.

Keys each POST /config/loadbalancer body by (port, protocol) so the two runs
line up regardless of ordering, masks the pool-allocated external IP (which is
allocation-order dependent), and prints a field-level diff.
"""
import json, sys, copy

def load(path):
    out = {}
    ips = {}
    for line in open(path):
        r = json.loads(line)
        if r["method"] != "POST" or "loadbalancer" not in r["path"]:
            continue
        b = copy.deepcopy(r["body"])
        sa = b.get("serviceArguments", {})
        key = (sa.get("port"), sa.get("protocol"))
        ips[key] = sa.get("externalIP")
        sa["externalIP"] = "<MASKED>"
        # endpoint ordering is not significant
        b["endpoints"] = sorted(b.get("endpoints") or [],
                                key=lambda e: json.dumps(e, sort_keys=True))
        out[key] = b
    return out, ips

def flat(o, p=""):
    if isinstance(o, dict):
        for k, v in o.items():
            yield from flat(v, f"{p}.{k}" if p else k)
    elif isinstance(o, list):
        for i, v in enumerate(o):
            yield from flat(v, f"{p}[{i}]")
    else:
        yield p, o

a, aip = load(sys.argv[1])
b, bip = load(sys.argv[2])
ta, tb = sys.argv[3], sys.argv[4]

print(f"{ta}: {len(a)} rules   {tb}: {len(b)} rules")
only_a = sorted(set(a) - set(b)); only_b = sorted(set(b) - set(a))
if only_a: print(f"  ONLY IN {ta}: {only_a}")
if only_b: print(f"  ONLY IN {tb}: {only_b}")

ndiff = 0
for key in sorted(set(a) & set(b), key=lambda k: (k[0] or 0, k[1] or "")):
    fa, fb = dict(flat(a[key])), dict(flat(b[key]))
    keys = sorted(set(fa) | set(fb))
    d = [(k, fa.get(k, "<absent>"), fb.get(k, "<absent>")) for k in keys if fa.get(k, "<absent>") != fb.get(k, "<absent>")]
    if d:
        ndiff += 1
        print(f"\n  DIFF port={key[0]}/{key[1]}")
        for k, va, vb in d:
            print(f"    {k}: {ta}={va!r}  {tb}={vb!r}")

print(f"\n{'IDENTICAL' if ndiff == 0 else str(ndiff) + ' RULE(S) DIFFER'} "
      f"({len(set(a) & set(b))} rules compared)")
print("\nexternal IP allocation:")
for key in sorted(set(aip) & set(bip), key=lambda k: (k[0] or 0, k[1] or "")):
    mark = "" if aip[key] == bip[key] else "   <-- differs"
    print(f"  {key[0]}/{key[1]}: {ta}={aip[key]}  {tb}={bip[key]}{mark}")
