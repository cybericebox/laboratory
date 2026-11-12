#!/usr/bin/env bash
# Real-node check of the device access service. NOT RUN: written on a machine without a Linux node. Run it once against a device on a
# runc node and once against a device on a Kata (micro-VM) node, then read the INFO lines (capabilities, user) next to the PASS/FAIL lines.
#
# Needs: kubectl with access to the cluster, go (to run the checker against the repo's wire package), a lab with a device whose template has
# terminal.enabled, running on the node under test. terminal.enabled must be on in the chart and the service pod Ready on that node.
#
#   hack/device-access-check.sh <lab-group-namespace> <lab> <device> [user]
#   REFUSE=<ns>/<vpn-lab>/vpn,<ns>/<lab>/gateway hack/device-access-check.sh ...   # also check that these are refused
set -euo pipefail

ns=${1:?lab group namespace}
lab=${2:?lab name}
device=${3:?device name}
user=${4:-}
release_ns=${RELEASE_NS:-laboratory-system}
port=${LOCAL_PORT:-18080}
here=$(cd "$(dirname "$0")/.." && pwd)

# The node of the device (the Device object records it).
node=$(kubectl -n "$ns" get devices.laboratory.cybericebox.com -o json |
  python3 -c 'import json,sys; d=json.load(sys.stdin); [print(i["status"].get("nodeName","")) for i in d["items"] if i["spec"].get("labRef")==sys.argv[1] and i["spec"].get("name")==sys.argv[2]]' "$lab" "$device" | head -1)
[ -n "$node" ] || { echo "no node recorded for device $device of lab $lab in $ns" >&2; exit 1; }
runtime=runc
if [ "$(kubectl get node "$node" -o jsonpath='{.metadata.labels.cybericebox\.com/runtime-kata-clh}')" != "" ]; then runtime="kata (the node can run micro-VMs; check that THIS device is a microVM profile device)"; fi
echo "device $device of $lab runs on node $node ($runtime)"

pod=$(kubectl -n "$release_ns" get pods -l app=laboratory-device-access --field-selector spec.nodeName="$node" -o jsonpath='{.items[0].metadata.name}')
[ -n "$pod" ] || { echo "no device access service pod on $node" >&2; exit 1; }
echo "service pod: $pod"

kubectl -n "$release_ns" port-forward "pod/$pod" "$port:8080" >/dev/null &
pf=$!
trap 'kill $pf 2>/dev/null || true' EXIT
sleep 2

rc=0
(cd "$here" && GOWORK=off go run hack/device-access-check/main.go -addr "127.0.0.1:$port" -namespace "$ns" -lab "$lab" -device "$device" -user "$user" -refuse "${REFUSE:-}") || rc=$?

echo "--- leftovers in the service pod (must be empty once the checks have ended)"
sleep 3
kubectl -n "$release_ns" exec "$pod" -- sh -c 'ls -A /run/containerd/fifo/cib-device-access/fifo /run/containerd/fifo/cib-device-access/journal 2>&1' ||
  kubectl -n "$release_ns" exec "$pod" -- /node device-access-health || true
echo "--- metrics"
kubectl -n "$release_ns" get --raw "/api/v1/namespaces/$release_ns/pods/$pod:9102/proxy/metrics" 2>/dev/null | grep '^device_access_' || echo "(the metrics port is not reachable through the API server proxy; use a port-forward to 9102)"
echo "--- also look at the node by hand: no process 'cib-' exec left (ctr -n k8s.io tasks ls / ps), and the FIFO directory empty"
exit $rc
