#!/bin/bash
# Build a loxilb-inference-gateway image from one ref of a local checkout,
# without touching that checkout's working tree.
#
#   IGW_REPO=~/go/src/loxilb-io/loxilb-inference-gateway ./build-igw.sh
#   IGW_REPO=... IGW_REF=origin/feat/helm-chart-core ./build-igw.sh
#
# The ref is cloned into $WORK/igw-src with its submodules (loxilb-ebpf, and
# libbpf inside it) taken from the checkout's own submodules, so nothing is
# fetched for them. An image already built for the same commit is reused.
# The image name is written to $WORK/igw-image and printed last, and the API-key
# store's bootstrap script of the same commit to $WORK/igw-db-bootstrap.sql.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="${WORK:-$HERE/_work}"
IGW_REPO="${IGW_REPO:?set IGW_REPO to a loxilb-inference-gateway checkout}"
IGW_REF="${IGW_REF:-origin/feat/helm-chart-core}"

mkdir -p "$WORK"
sha="$(git -C "$IGW_REPO" rev-parse --short=8 "$IGW_REF")"
IMAGE="${IGW_BUILD_IMAGE:-ghcr.io/loxilb-io/loxilb-inference-gateway:e2e-$sha}"

git -C "$IGW_REPO" show "$IGW_REF:scripts/aigw-db-bootstrap.sql" > "$WORK/igw-db-bootstrap.sql"

if docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "$IMAGE" > "$WORK/igw-image"
  echo "reusing $IMAGE"
  echo "$IMAGE"
  exit 0
fi

SRC="$WORK/igw-src"
rm -rf "$SRC"
git clone -q --no-checkout "$IGW_REPO" "$SRC"
ref="$(git -C "$IGW_REPO" rev-parse --symbolic-full-name "$IGW_REF")"
git -C "$SRC" fetch -q "$IGW_REPO" "+$ref:refs/heads/e2e-build"
git -C "$SRC" checkout -q e2e-build

# Submodules from the checkout's own copies: the pinned commits are there.
# Git refuses local-path submodule clones unless told to, for this call only.
git -C "$SRC" config submodule.loxilb-ebpf.url "$IGW_REPO/loxilb-ebpf"
git -c protocol.file.allow=always -C "$SRC" submodule update --init -q loxilb-ebpf
git -C "$SRC/loxilb-ebpf" config submodule.libbpf.url "$IGW_REPO/loxilb-ebpf/libbpf"
git -c protocol.file.allow=always -C "$SRC/loxilb-ebpf" submodule update --init -q libbpf

echo "building $IMAGE from $IGW_REF ($sha)"
docker build --build-arg VERSION="e2e-$sha" -t "$IMAGE" "$SRC"
echo "$IMAGE" > "$WORK/igw-image"
echo "$IMAGE"
