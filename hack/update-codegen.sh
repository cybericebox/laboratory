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

SCRIPT_DIR="$(dirname "${BASH_SOURCE[0]}")"
SCRIPT_ROOT="${SCRIPT_DIR}/.."
K8S_VERSION="$(go list -m -f '{{.Version}}' k8s.io/client-go)"
CODEGEN_PKG="$(go env GOPATH)/pkg/mod/k8s.io/code-generator@${K8S_VERSION}"

# Keep generated clients ABI-compatible with the Kubernetes libraries used by the
# operator.  A sibling checkout may be from a different Kubernetes release.
if [[ ! -f "${CODEGEN_PKG}/kube_codegen.sh" ]]; then
    go mod download "k8s.io/code-generator@${K8S_VERSION}"
fi

source "${CODEGEN_PKG}/kube_codegen.sh"

THIS_PKG="github.com/cybericebox/laboratory"

kube::codegen::gen_client \
    --with-watch \
    --with-applyconfig \
    --prefers-protobuf \
    --clientset-name "client" \
    --output-dir "${SCRIPT_ROOT}/clientset" \
    --output-pkg "${THIS_PKG}/clientset" \
    --boilerplate "${SCRIPT_ROOT}/hack/boilerplate.go.txt" \
    "${SCRIPT_ROOT}/api"
