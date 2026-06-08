#!/bin/bash
# Run this INSIDE Lima VM after `kind create cluster`.
# Writes ~/.kube/config on Mac host with server address fixed to 127.0.0.1:16443.
set -euo pipefail

MAC_KUBECONFIG="$HOME/.kube/config"
LIMA_MOUNT="/projects/cybericebox"

# Extract kubeconfig and rewrite server address for Mac-side access
kind get kubeconfig --name icebox \
  | sed 's|server: https://.*:6443|server: https://127.0.0.1:16443|' \
  > /tmp/icebox-kubeconfig.yaml

# Copy to mounted Mac home directory
cp /tmp/icebox-kubeconfig.yaml "${LIMA_MOUNT}/../../../.kube/config" 2>/dev/null \
  || cp /tmp/icebox-kubeconfig.yaml ~/icebox-kubeconfig.yaml

echo "kubeconfig written."
echo "On Mac host run:"
echo "  export KUBECONFIG=~/.kube/config"
echo "  kubectl cluster-info"
