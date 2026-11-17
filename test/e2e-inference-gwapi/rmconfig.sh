#!/bin/bash
# Tear down what config.sh created. Images are kept: they are tagged by commit
# and reused by the next run.
set -u
source "$(dirname "$0")/env.sh"

k3d cluster delete "$CLUSTER" >/dev/null 2>&1
docker rm -f igw1 igw2 e2e-oidc e2e-pg >/dev/null 2>&1
docker network rm "$NET" >/dev/null 2>&1
rm -f "$KUBECONFIG"
echo "testbed removed"
