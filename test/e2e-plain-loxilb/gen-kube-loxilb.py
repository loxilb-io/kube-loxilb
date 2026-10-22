#!/usr/bin/env python3
"""Rewrite the shipped ext-cluster manifest for this testbed: the image under
test and the testbed's arguments, everything else as shipped."""
import os, sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
for doc in docs:
    if doc.get("kind") != "Deployment":
        continue
    container = doc["spec"]["template"]["spec"]["containers"][0]
    container["image"] = os.environ["KLB_IMAGE"]
    container["imagePullPolicy"] = "IfNotPresent"
    container["args"] = os.environ["KLB_ARGS"].split()

yaml.safe_dump_all(docs, sys.stdout, default_flow_style=False, sort_keys=False)
