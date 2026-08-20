#!/bin/bash
# Tear down what config.sh created. k3s is left installed unless PURGE_K3S=1.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"

kubectl delete -f "$HERE/fixtures/httproute.yaml" -f "$HERE/fixtures/inferencepool.yaml" >/dev/null 2>&1
kubectl -n llm delete inferencepool epp-required >/dev/null 2>&1
kubectl -n llm delete httproute epp-route >/dev/null 2>&1
kubectl delete -f "$HERE/fixtures/gateway.yaml" -f "$HERE/fixtures/gatewayclass.yaml" >/dev/null 2>&1
kubectl delete -f "$HERE/fixtures/model-servers.yaml" >/dev/null 2>&1
kubectl -n kube-system delete deploy kube-loxilb >/dev/null 2>&1

docker rm -f igw1 >/dev/null 2>&1
sudo ip link del igwbr 2>/dev/null
sudo ip link del eigw1h 2>/dev/null
sudo rm -f /var/run/netns/igw1

if [[ -n "${PURGE_K3S:-}" && -f /usr/local/bin/k3s-uninstall.sh ]]; then
  sudo /usr/local/bin/k3s-uninstall.sh >/dev/null 2>&1
fi
echo "testbed removed"
