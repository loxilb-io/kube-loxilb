#!/usr/bin/env python3
"""Rewrite the shipped gateway-api manifest for this testbed.

Generated from the manifest in the repository rather than kept as a copy, so a
change to RBAC or the deployment shape is picked up instead of drifting.
"""
import os, sys, yaml

tag = os.environ["KLB_TAG"]
args = [
    "--loxiURL=" + os.environ["LOXIURL"],
    "--cidrPools=" + os.environ["VIP_POOL"],
    "--gatewayAPI",
    "--inferenceExtension",
    "--v=4",
]

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
for doc in docs:
    if doc.get("kind") != "Deployment":
        continue
    container = doc["spec"]["template"]["spec"]["containers"][0]
    container["image"] = f"ghcr.io/loxilb-io/kube-loxilb:{tag}"
    container["imagePullPolicy"] = "IfNotPresent"
    container["args"] = args

yaml.safe_dump_all(docs, sys.stdout, default_flow_style=False, sort_keys=False)
