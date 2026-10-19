#!/usr/bin/env bash
# Creates the kind cluster of the WireGuard demux harness (icebox-demux: a control plane and two workers, no kube-proxy) with
# Cilium 1.20.2 as the only CNI, in the datapath setup of the cloud (kube-proxy replacement, VXLAN tunnel, tcx), and installs the
# laboratory CRDs. Idempotent: an existing cluster is reused.
#
#   make kind-demux-up              # or hack/kind-demux/up.sh
#   make kind-demux-e2e KIND_DEMUX_MODE=userspace   # or kind-demux-e2e-all: userspace, tc, xdp and detach
#   make kind-demux-down
#
# Needs docker, kind, kubectl and helm. The kubectl context is kind-icebox-demux; other kind clusters are not touched.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
CLUSTER="${KIND_DEMUX_CLUSTER:-icebox-demux}"
CILIUM_VERSION="1.20.2"
KUBECTL=(kubectl --context "kind-$CLUSTER")

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --config "$HERE/kind.yaml" --wait 0s
fi

# --repo instead of helm repo add: the user's helm repositories stay as they are.
helm upgrade --install cilium cilium --repo https://helm.cilium.io --version "$CILIUM_VERSION" --kube-context "kind-$CLUSTER" \
  --namespace kube-system --values "$HERE/cilium-values.yaml" --wait --timeout 10m

"${KUBECTL[@]}" -n kube-system rollout status ds/cilium --timeout=5m
"${KUBECTL[@]}" wait --for=condition=Ready nodes --all --timeout=5m

"${KUBECTL[@]}" apply --server-side -f "$ROOT/config/crd/bases"
"${KUBECTL[@]}" wait --for=condition=Established crd --all --timeout=2m
echo "kind-demux: cluster $CLUSTER is up (Cilium $CILIUM_VERSION)"
