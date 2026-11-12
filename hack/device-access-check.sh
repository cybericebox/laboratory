#!/usr/bin/env bash
# Real-node check of the device access service. NOT RUN: written on a machine without a Linux node. Run it once against a device on a
# runc node and once against a device on a Kata (micro-VM) node, then read the INFO lines (capabilities, user) next to the PASS/FAIL lines.
#
# The listener of the service is bound to the pod IP and its network policy admits only the L7 proxy, so a port-forward cannot reach it. The
# script therefore builds the checker for linux, copies it into the service pod on the node of the device (needs `sh` and `cat` in the node
# image; if there is none, run the checker from an ephemeral container: kubectl debug --target=device-access, which shares the pod's network
# and, with the secret copied, does the same) and runs it there. It signs its open requests with the secret the pod has mounted.
#
# Needs: kubectl with access to the cluster, go (to build the checker), a lab with a device whose template has terminal.enabled, running on the
# node under test; terminal.enabled on in the chart and the service pod Ready on that node.
#
#   hack/device-access-check.sh <lab-group-namespace> <lab> <device> [user]
#   REFUSE=<ns>/<vpn-lab>/vpn,<ns>/<lab>/gateway hack/device-access-check.sh ...   # also check that these are refused
#   SERVICE_NS=<namespace of the service>   (default laboratory-device-access)
#
# Non-root run (chart values terminal.securityContext): the default is uid 0 with every capability dropped, because the runtime socket is root:root
# 0660. To find out whether it also works non-root where the group of the socket is known, this script prints the owner and group of the socket and
# the user the service runs as. Then: helm upgrade with terminal.securityContext.runAsUser=<uid> runAsGroup=<gid> supplementalGroups={<socket gid>},
# make the FIFO directory writable by that user on the node (chown <uid> <fifoDir>), wait for the pod and run this script again: every check must pass and
# the leftovers listing must be empty. Record the result next to the one for uid 0.
set -euo pipefail

ns=${1:?lab group namespace}
lab=${2:?lab name}
device=${3:?device name}
user=${4:-}
service_ns=${SERVICE_NS:-laboratory-device-access}
here=$(cd "$(dirname "$0")/.." && pwd)

# The node of the device (the Device object records it).
node=$(kubectl -n "$ns" get devices.laboratory.cybericebox.com -o json |
  python3 -c 'import json,sys; d=json.load(sys.stdin); [print(i["status"].get("nodeName","")) for i in d["items"] if i["spec"].get("labRef")==sys.argv[1] and i["spec"].get("name")==sys.argv[2]]' "$lab" "$device" | head -1)
[ -n "$node" ] || { echo "no node recorded for device $device of lab $lab in $ns" >&2; exit 1; }
runtime=runc
if [ -n "$(kubectl get node "$node" -o jsonpath='{.metadata.labels.cybericebox\.com/runtime-kata-clh}')" ]; then
  runtime="kata-capable node (check that THIS device is a microVM profile device)"
fi
echo "device $device of $lab runs on node $node ($runtime)"

pod=$(kubectl -n "$service_ns" get pods -l app=laboratory-device-access --field-selector spec.nodeName="$node" -o jsonpath='{.items[0].metadata.name}')
[ -n "$pod" ] || { echo "no device access service pod on $node" >&2; exit 1; }
echo "service pod: $pod"

echo "--- who runs the service and who owns the runtime socket (for the non-root test)"
kubectl -n "$service_ns" get pod "$pod" -o jsonpath='{.spec.containers[0].securityContext}{"\n"}'
sock=$(kubectl -n "$service_ns" get pod "$pod" -o jsonpath='{.spec.containers[0].env[?(@.name=="CONTAINERD_SOCK")].value}')
kubectl -n "$service_ns" exec "$pod" -- sh -c "id; stat -c 'socket $sock: owner %u group %g mode %a' '$sock'" 2>&1 || echo "(no sh/stat in the image: read the socket's group on the node with: stat -c %g $sock)"

echo "--- building and copying the checker"
arch=$(kubectl get node "$node" -o jsonpath='{.status.nodeInfo.architecture}')
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
(cd "$here" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$tmp/device-access-check" hack/device-access-check/main.go)
kubectl -n "$service_ns" exec -i "$pod" -- sh -c 'cat > /tmp/device-access-check && chmod +x /tmp/device-access-check' < "$tmp/device-access-check"

rc=0
kubectl -n "$service_ns" exec "$pod" -- /tmp/device-access-check -namespace "$ns" -lab "$lab" -device "$device" -user "$user" -refuse "${REFUSE:-}" || rc=$?

echo "--- leftovers in the service pod (must be empty once the checks have ended)"
sleep 3
kubectl -n "$service_ns" exec "$pod" -- sh -c 'ls -A /run/containerd/fifo/cib-device-access/fifo /run/containerd/fifo/cib-device-access/journal 2>&1' ||
  echo "(could not list: look at the node's FIFO directory by hand)"
kubectl -n "$service_ns" exec "$pod" -- rm -f /tmp/device-access-check || true
echo "--- the metrics (the listener is on the pod IP, so ask from inside the pod)"
kubectl -n "$service_ns" exec "$pod" -- sh -c 'wget -qO- "http://$POD_IP:9102/metrics" | grep "^device_access_"' 2>&1 || echo "(no wget in the image: scrape the metrics port from a pod the policy admits)"
echo "--- also look at the node by hand: no exec 'cib-...' left in 'ctr -n k8s.io tasks ps <container>', and the FIFO directory empty"
exit $rc
