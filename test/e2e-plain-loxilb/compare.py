#!/usr/bin/env python3
"""Side by side: each check's outcome for the baseline and the candidate.

  compare.py <baseline.log> <candidate.log> <baseline-name> <candidate-name>

Checks are matched by section and description, with numbers masked - the
addresses and timings in a description differ between runs.
"""
import re, sys

def outcomes(path):
    out, section = {}, ""
    for line in open(path, errors="replace"):
        line = line.rstrip("\n")
        if line.startswith("=== "):
            section = line.strip("= ").split(".")[0]
            continue
        m = re.match(r"\s+\[(OK|FAIL|NOTE)\]\s+(.*)$", line)
        if not m or m.group(1) == "NOTE":
            continue
        desc = m.group(2).split(" - ")[0]
        key = (section, re.sub(r"\d+(\.\d+)*", "#", desc))
        out.setdefault(key, (m.group(1), m.group(2)))
    return out

a, b = outcomes(sys.argv[1]), outcomes(sys.argv[2])
na, nb = sys.argv[3], sys.argv[4]
keys = list(dict.fromkeys(list(a) + list(b)))
print(f"{'':4} {na:>8} {nb:>8}  check")
for k in keys:
    ra, rb = a.get(k, ("-", ""))[0], b.get(k, ("-", ""))[0]
    mark = "  " if ra == rb else "<>"
    detail = (b.get(k) or a.get(k))[1]
    print(f"{k[0]:4} {ra:>8} {rb:>8}{mark} {detail}")
fixed = [k for k in keys if a.get(k, ("",))[0] == "FAIL" and b.get(k, ("",))[0] == "OK"]
broke = [k for k in keys if a.get(k, ("",))[0] == "OK" and b.get(k, ("",))[0] == "FAIL"]
print(f"\nfailing only on {na}: {len(fixed)}   failing only on {nb}: {len(broke)}")
