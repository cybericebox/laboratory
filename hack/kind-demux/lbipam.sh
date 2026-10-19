#!/usr/bin/env bash
# Cilium LB-IPAM and the demux Service on the kind cluster of up.sh, after an e2e.sh tc or xdp run:
#
#   hack/kind-demux/lbipam.sh
#
# Gives the cluster an LB-IPAM pool and a second UDP Service in the release namespace that shares the demux Service's sharing key
# (the layout of the cloud, where laboratory-edge also carries a UDP 443 HTTP/3 Service), and records which addresses LB-IPAM hands
# out with the demux Service's externalTrafficPolicy Local (the chart default) and with Cluster. With a shared address it checks that
# the demux found the LoadBalancer address as an entry address (the fast path's wg_entries on the entry node). Puts everything back
# at the end (the policy it had, no pool, no second Service). Findings only: the chart defaults are not changed here.
set -euo pipefail

CLUSTER="${KIND_DEMUX_CLUSTER:-icebox-demux}"
WG_NS="laboratory-wg"
RELEASE_NS="laboratory-system"
ENTRY_NODE="$CLUSTER-worker"
POOL="192.168.31.240/29"
ENTRIES="/sys/fs/bpf/cybericebox/wg-demux/wg_entries"

k() { kubectl --context "kind-$CLUSTER" "$@"; }
log() { printf '\n== %s\n' "$*"; }
fail() { printf '\nFAIL (lbipam): %s\n' "$*" >&2; exit 1; }
summary() {
  printf 'SUMMARY lbipam: %s\n' "$*"
  if [[ -n "${KIND_DEMUX_SUMMARY:-}" ]]; then printf '%-9s %s\n' lbipam "$*" >>"$KIND_DEMUX_SUMMARY"; fi
}
cilium() {
  k -n kube-system exec "$(k -n kube-system get pod -l k8s-app=cilium --field-selector "spec.nodeName=$ENTRY_NODE" \
    -o jsonpath='{.items[0].metadata.name}')" -c cilium-agent -- "$@"
}
lb_ip() { k -n "$1" get svc "$2" -o jsonpath='{.status.loadBalancer.ingress[*].ip}'; }
# wait_ips: waits up to 30 s until both Services have an address (or until it stops changing), then prints "<demux> <edge>".
wait_ips() {
  local start=$SECONDS d e
  while ((SECONDS - start < 30)); do
    d="$(lb_ip "$WG_NS" laboratory-proxy-wg)"
    e="$(lb_ip "$RELEASE_NS" demux-edge-h3)"
    [[ -n "$d" && -n "$e" ]] && break
    sleep 1
  done
  echo "${d:-none} ${e:-none}"
}
# entries: the entry addresses in the fast path of the entry node, as ip:port (the map holds them in network byte order).
entries() {
  cilium bpftool -j map dump pinned "$ENTRIES" | python3 -c '
import json, socket, struct, sys
for e in json.load(sys.stdin):
    k = e["formatted"]["key"]
    print("%s:%d" % (socket.inet_ntoa(struct.pack("<I", k["ip"])), socket.ntohs(k["port"])))
'
}
# entries_ok: sets ENTRIES_NOW to the entries; fails when they cannot be read or lost the node's own entry (node:nodePort).
entries_ok() {
  ENTRIES_NOW="$(entries)" || fail "cannot read the fast path's entries on $ENTRY_NODE"
  grep -qx "$NODE_ENTRY" <<<"$ENTRIES_NOW" || fail "the fast path's entries on $ENTRY_NODE lack $NODE_ENTRY: $(paste -sd' ' - <<<"$ENTRIES_NOW")"
}
# restore: the policy it had, no UDP 443 Service, no pool, and no LoadBalancer address on the demux Service (Cilium keeps a handed-out address
# after its pool is gone; a type change clears status.loadBalancer).
restore() {
  k -n "$WG_NS" patch svc laboratory-proxy-wg --type=merge -p "{\"spec\":{\"externalTrafficPolicy\":\"$ORIG_ETP\"}}" >/dev/null || true
  k -n "$RELEASE_NS" delete svc demux-edge-h3 --ignore-not-found >/dev/null || true
  k delete ciliumloadbalancerippools.cilium.io demux-e2e --ignore-not-found >/dev/null || true
  k -n "$WG_NS" patch svc laboratory-proxy-wg --type=merge -p '{"spec":{"type":"NodePort"}}' >/dev/null || true
  k -n "$WG_NS" patch svc laboratory-proxy-wg --type=merge -p '{"spec":{"type":"LoadBalancer"}}' >/dev/null || true
}
ORIG_ETP=Local
trap restore EXIT
edge() {
  k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: demux-edge-h3
  namespace: $RELEASE_NS
  annotations:
    lbipam.cilium.io/sharing-key: "$key"
    lbipam.cilium.io/sharing-cross-namespace: "$WG_NS"
spec:
  type: LoadBalancer
  externalTrafficPolicy: Cluster
  ports:
    - name: h3
      protocol: UDP
      port: 443
EOF
}

k -n "$WG_NS" get svc laboratory-proxy-wg >/dev/null || fail "no demux Service: run e2e.sh tc or xdp first"
cilium test -e "$ENTRIES" || fail "no fast path on $ENTRY_NODE: run e2e.sh tc or xdp first"
NODE_ENTRY="$(k get node "$ENTRY_NODE" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}'):$(k -n "$WG_NS" get svc \
  laboratory-proxy-wg -o jsonpath='{.spec.ports[0].nodePort}')"
ORIG_ETP="$(k -n "$WG_NS" get svc laboratory-proxy-wg -o jsonpath='{.spec.externalTrafficPolicy}')"
key="$(k -n "$WG_NS" get svc laboratory-proxy-wg -o jsonpath='{.metadata.annotations.lbipam\.cilium\.io/sharing-key}')"

log "pool $POOL and a UDP 443 Service in $RELEASE_NS sharing the key $key"
k apply -f - >/dev/null <<EOF
apiVersion: cilium.io/v2
kind: CiliumLoadBalancerIPPool
metadata:
  name: demux-e2e
spec:
  blocks:
    - cidr: $POOL
EOF
edge

read -r d e <<<"$(wait_ips)"
echo "externalTrafficPolicy Local: demux $d, edge $e"
summary "demux Service with externalTrafficPolicy Local: demux $d, UDP 443 Service (Cluster) $e"

log "demux Service with externalTrafficPolicy Cluster"
k -n "$WG_NS" patch svc laboratory-proxy-wg --type=merge -p '{"spec":{"externalTrafficPolicy":"Cluster"}}' >/dev/null
# LB-IPAM does not move an address it handed out: the UDP 443 Service asks again, now that the policies match.
k -n "$RELEASE_NS" delete svc demux-edge-h3 --wait >/dev/null
edge
start=$SECONDS
while ((SECONDS - start < 30)); do
  d="$(lb_ip "$WG_NS" laboratory-proxy-wg)"
  e="$(lb_ip "$RELEASE_NS" demux-edge-h3)"
  [[ -n "$d" && "$d" == "$e" ]] && break
  sleep 1
done
echo "externalTrafficPolicy Cluster: demux ${d:-none}, edge ${e:-none}"
summary "demux Service with externalTrafficPolicy Cluster: demux ${d:-none}, UDP 443 Service $e"
[[ -n "$d" ]] || fail "the demux Service got no LoadBalancer address with policy Cluster"

log "the demux finds the LoadBalancer address $d"
start=$SECONDS
while entries_ok && ! grep -qx "$d:51820" <<<"$ENTRIES_NOW"; do
  ((SECONDS - start <= 30)) || fail "the fast path's entries on $ENTRY_NODE do not hold $d:51820: $(paste -sd' ' - <<<"$ENTRIES_NOW")"
  sleep 1
done
echo "entries: $(paste -sd' ' - <<<"$ENTRIES_NOW")"
summary "entry addresses in the fast path after $((SECONDS - start)) s: $(paste -sd' ' - <<<"$ENTRIES_NOW")"

restore
trap - EXIT
log "put back: the policy it had, no pool, no UDP 443 Service, the demux Service without its LoadBalancer address"
[[ -z "$(lb_ip "$WG_NS" laboratory-proxy-wg)" ]] || fail "the demux Service still has $(lb_ip "$WG_NS" laboratory-proxy-wg)"
start=$SECONDS
while entries_ok && grep -q "^$d:" <<<"$ENTRIES_NOW"; do
  ((SECONDS - start <= 30)) || fail "the fast path still holds $d after the Service lost it: $(paste -sd' ' - <<<"$ENTRIES_NOW")"
  sleep 1
done
summary "after the Service lost the address (+$((SECONDS - start)) s) the entries are: $(paste -sd' ' - <<<"$ENTRIES_NOW")"
printf '\nDONE (lbipam)\n'
