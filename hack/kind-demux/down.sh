#!/usr/bin/env bash
# Deletes the kind cluster of the WireGuard demux harness and its client containers (see up.sh).
set -euo pipefail
CLUSTER="${KIND_DEMUX_CLUSTER:-icebox-demux}"
for c in $(docker ps -a --filter "label=icebox-demux-client" --format '{{.Names}}'); do docker rm -f "$c" >/dev/null; done
kind delete cluster --name "$CLUSTER"
