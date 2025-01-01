#!/usr/bin/env bash

# Copyright 2017 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
echo $SCRIPT_ROOT
GOPATH=$(go env GOPATH)
echo $GOPATH
CODEGEN_PKG=${CODEGEN_PKG:-$(cd "${SCRIPT_ROOT}"; ls -d -1 $GOPATH/src/k8s.io/code-generator 2>/dev/null || echo ../code-generator)}
echo $CODEGEN_PKG

source "${CODEGEN_PKG}/kube_codegen.sh"

THIS_PKG="github.com/loxilb-io/kube-loxilb"

kube::codegen::gen_helpers \
    --boilerplate "${SCRIPT_ROOT}/hack/boilerplate.go.txt" \
    "${SCRIPT_ROOT}/pkg/crds"

kube::codegen::gen_client \
    --with-watch \
    --output-dir "${SCRIPT_ROOT}/pkg/egress-client" \
    --output-pkg "${THIS_PKG}/pkg/egress-client" \
    --boilerplate "${SCRIPT_ROOT}/hack/boilerplate.go.txt" \
    --one-input-api "egress" \
    "${SCRIPT_ROOT}/pkg/crds"
