#!/bin/bash
# Tear down what config.sh created. Images are kept: they are tagged by commit
# and reused by the next run.
set -u
source "$(dirname "$0")/env.sh"

k3d cluster delete "$CLUSTER" >/dev/null 2>&1
docker rm -f llb1 llb2 >/dev/null 2>&1
docker network rm "$NET" >/dev/null 2>&1
sudo ip route del "$VIP_CIDR" via "$LLB1_ADDR" 2>/dev/null
rm -f "$KUBECONFIG"
echo "testbed removed"
