#!/bin/bash
set -euo pipefail
CLUSTER="${CLUSTER:-wire-compat}"
k3d cluster delete "$CLUSTER"
