#!/usr/bin/env bash
# End-to-end test of the WireGuard demux on the kind cluster of up.sh, in one mode (userspace, tc or xdp):
#
#   make kind-demux-e2e MODE=userspace      # builds the proxy image, then runs hack/kind-demux/e2e.sh userspace
#
# It deploys the demux part of the chart (templates/wg and its network policy, rendered with helm template -s: the rest of the chart
# needs the operator, cert-manager and the Gateway API), two groups whose VPN servers are plain kernel WireGuard pods (vpn-server.yaml),
# one on the demux's node and one on the other worker, each with the operator's VPN pod policy (printed from vpnCiliumPolicy by
# TestPrintVPNCiliumPolicy, not a hand copy), and LabGroups whose status says the VPN is registered. Two client containers on the kind
# docker network connect through the node port of the demux's Service on the first worker. It checks: both clients handshake and ping
# their server with no loss, also with 1300-byte packets; a client with an unknown key never gets a handshake; a restart of the demux pod
# loses at most 2 s of pings in tc and xdp (userspace may re-handshake); a client that changes its source port keeps its tunnel.
#
# Leaves everything running for inspection; the next run starts the test objects afresh. Exit status 0 = PASS.
set -euo pipefail

MODE="${1:-${MODE:-userspace}}"
case "$MODE" in userspace | tc | xdp) ;; *) echo "usage: $0 userspace|tc|xdp" >&2; exit 2 ;; esac

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
CLUSTER="${KIND_DEMUX_CLUSTER:-icebox-demux}"
IMG="${KIND_DEMUX_IMG:-cybericebox/laboratory-proxy:kind-demux}"
WG_IMG="icebox-demux/wg-test:1"
RELEASE_NS="laboratory-system"
WG_NS="laboratory-wg"
ENTRY_NODE="$CLUSTER-worker"
OTHER_NODE="$CLUSTER-worker2"
KEEPALIVE=5
ROAM_INTERVAL=5
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

k() { kubectl --context "kind-$CLUSTER" "$@"; }
log() { printf '\n== %s\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
genkey() { docker run --rm "$WG_IMG" sh -c 'k=$(wg genkey); echo "$k $(echo "$k" | wg pubkey)"'; }
client() { docker exec "icebox-demux-client-$1" "${@:2}"; }

# received <client> <ping args...>: runs ping in the client and prints the number of replies.
received() {
  local out
  out="$(client "$1" ping "${@:2}" 2>&1 || true)"
  sed -n 's/.* \([0-9]*\) received.*/\1/p' <<<"$out"
}

# wait_tunnel <client> <server ip> <seconds>: waits until one ping gets through; prints the seconds it took.
wait_tunnel() {
  local start=$SECONDS
  while ((SECONDS - start < $3)); do
    if client "$1" ping -c 1 -W 1 "$2" >/dev/null 2>&1; then echo $((SECONDS - start)); return 0; fi
  done
  return 1
}

log "images"
printf 'FROM alpine:3.22\nRUN apk add --no-cache wireguard-tools iproute2 iputils\n' | docker build -q -t "$WG_IMG" - >/dev/null
kind load docker-image "$IMG" "$WG_IMG" --name "$CLUSTER" >/dev/null

log "fresh test objects"
for c in $(docker ps -a --filter "label=icebox-demux-client" --format '{{.Names}}'); do docker rm -f "$c" >/dev/null; done
k delete labgroups.laboratory.cybericebox.com demux-a demux-b --ignore-not-found --wait
k delete namespace lg-demux-a lg-demux-b --ignore-not-found --wait
k create namespace "$RELEASE_NS" --dry-run=client -o yaml | k apply -f - >/dev/null

# group <letter> <node> <server tunnel ip> <client tunnel ip>: the VPN server, its policy and the LabGroup of one group.
group() {
  local g="$1" ns="lg-demux-$1" spriv spub cpriv cpub
  read -r spriv spub <<<"$(genkey)"
  read -r cpriv cpub <<<"$(genkey)"
  printf -v "SERVER_PUB_$g" %s "$spub"
  printf -v "CLIENT_PRIV_$g" %s "$cpriv"
  sed -e "s|__NS__|$ns|g" -e "s|__GROUP__|demux-$g|g" -e "s|__NODE__|$2|g" -e "s|__PRIVATE_KEY__|$spriv|g" \
    -e "s|__PEER_KEY__|$cpub|g" -e "s|__SERVER_IP__|$3|g" -e "s|__CLIENT_IP__|$4|g" "$HERE/vpn-server.yaml" | k apply -f - >/dev/null
  (cd "$ROOT" && VPN_POLICY_NS="$ns" VPN_POLICY_MODE="$MODE" VPN_POLICY_OUT="$WORK/policy-$g.yaml" GOWORK=off \
    go test ./internal/controller/laboratory -run '^TestPrintVPNCiliumPolicy$' -count=1 >/dev/null)
  k apply -f "$WORK/policy-$g.yaml" >/dev/null
  printf 'apiVersion: laboratory.cybericebox.com/v1alpha1\nkind: LabGroup\nmetadata:\n  name: demux-%s\nspec: {}\n' "$g" | k apply -f - >/dev/null
  k patch labgroups.laboratory.cybericebox.com "demux-$g" --subresource=status --type=merge \
    -p "{\"status\":{\"namespace\":\"$ns\",\"vpn\":{\"publicKey\":\"$spub\",\"registered\":true}}}" >/dev/null
  k -n "$ns" wait --for=condition=Ready pod/vpn --timeout=2m >/dev/null
}
log "groups: a on $ENTRY_NODE (the demux's node), b on $OTHER_NODE"
group a "$ENTRY_NODE" 10.99.1.1 10.99.1.2
group b "$OTHER_NODE" 10.99.2.1 10.99.2.2

log "demux (mode $MODE)"
templates=(priorityclasses.yaml wg/namespace.yaml wg/serviceaccount.yaml wg/clusterrole.yaml wg/clusterrolebinding.yaml wg/role.yaml wg/service.yaml
  wg/daemonset.yaml)
# The demux's pod policy renders in userspace only (on the host network pod policies do not apply), and helm template -s refuses a
# template that renders nothing.
[[ "$MODE" == userspace ]] && templates+=(netpol/wg-demux.yaml)
show=()
for t in "${templates[@]}"; do show+=(-s "templates/$t"); done
helm template laboratory "$ROOT/charts/laboratory" --namespace "$RELEASE_NS" \
  --set proxy.wg.mode="$MODE" --set proxy.l7.enabled=false \
  --set proxy.image.repository="${IMG%:*}" --set proxy.image.tag="${IMG##*:}" \
  --set operator.baseDomain=demux.test --set operator.publicVPNEndpoint=vpn.demux.test --set operator.supportEmail=demux@demux.test \
  "${show[@]}" >"$WORK/demux.yaml"
if [[ "$MODE" != userspace ]]; then
  k -n "$WG_NS" delete networkpolicy,ciliumnetworkpolicy laboratory-proxy-wg --ignore-not-found >/dev/null
fi
k apply -f "$WORK/demux.yaml" >/dev/null
# The image tag does not change between builds: restart so the pods run the image just loaded.
k -n "$WG_NS" rollout restart ds/laboratory-proxy-wg >/dev/null
k -n "$WG_NS" rollout status ds/laboratory-proxy-wg --timeout=5m

NODE_PORT="$(k -n "$WG_NS" get svc laboratory-proxy-wg -o jsonpath='{.spec.ports[0].nodePort}')"
ENTRY_IP="$(k get node "$ENTRY_NODE" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
echo "entry: $ENTRY_IP:$NODE_PORT"

# start_client <name> <private key> <tunnel ip> <server public key> <server subnet>
start_client() {
  docker run -d --name "icebox-demux-client-$1" --label icebox-demux-client --network kind --cap-add NET_ADMIN "$WG_IMG" \
    sleep infinity >/dev/null
  docker exec -i "icebox-demux-client-$1" sh -c "mkdir -p /etc/wireguard && cat >/etc/wireguard/wg0.conf" <<EOF
[Interface]
PrivateKey = $2
Address = $3/24

[Peer]
PublicKey = $4
Endpoint = $ENTRY_IP:$NODE_PORT
AllowedIPs = $5
PersistentKeepalive = $KEEPALIVE
EOF
  client "$1" wg-quick up wg0 >/dev/null 2>&1
}
log "clients"
# shellcheck disable=SC2154 # set by group through printf -v
start_client a "$CLIENT_PRIV_a" 10.99.1.2 "$SERVER_PUB_a" 10.99.1.0/24
# shellcheck disable=SC2154
start_client b "$CLIENT_PRIV_b" 10.99.2.2 "$SERVER_PUB_b" 10.99.2.0/24
read -r bad_priv _ <<<"$(genkey)"
start_client bad "$bad_priv" 10.99.1.3 "$SERVER_PUB_a" 10.99.1.0/24

server_of() { [[ $1 == b ]] && echo 10.99.2.1 || echo 10.99.1.1; }

for g in a b; do
  log "client $g: handshake and ping"
  t="$(wait_tunnel "$g" "$(server_of "$g")" 30)" || fail "client $g: no tunnel within 30 s"
  echo "tunnel up after ${t}s"
  [[ "$(client "$g" wg show wg0 latest-handshakes | awk '{print $2}')" != 0 ]] || fail "client $g: no handshake"
  n="$(received "$g" -c 50 -i 0.2 -W 1 "$(server_of "$g")")"
  echo "ping: $n/50"
  [[ "$n" == 50 ]] || fail "client $g: lost $((50 - n)) of 50 pings"
done

for g in a b; do
  log "client $g: 1300-byte pings"
  n="$(received "$g" -c 50 -i 0.2 -W 1 -s 1300 "$(server_of "$g")")"
  echo "ping -s 1300: $n/50"
  [[ "$n" == 50 ]] || fail "client $g: lost $((50 - n)) of 50 large pings"
done

log "client with an unknown key"
n="$(received bad -c 10 -i 0.5 -W 1 10.99.1.1)"
hs="$(client bad wg show wg0 latest-handshakes | awk '{print $2}')"
echo "replies: $n, latest handshake: $hs"
[[ "$n" == 0 && "$hs" == 0 ]] || fail "the client with an unknown key got through"

log "the VPN pod policy: a pod outside laboratory-wg that the server knows cannot reach it directly"
read -r intruder_priv intruder_pub <<<"$(genkey)"
k -n lg-demux-a exec vpn -- wg set wg0 peer "$intruder_pub" allowed-ips 10.99.1.4/32
server_pod_ip="$(k -n lg-demux-a get pod vpn -o jsonpath='{.status.podIP}')"
k delete pod demux-intruder --ignore-not-found --wait >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: demux-intruder
spec:
  nodeName: $ENTRY_NODE
  terminationGracePeriodSeconds: 1
  containers:
    - name: wg
      image: $WG_IMG
      imagePullPolicy: Never
      command: [ sh, -ec, 'echo "\$KEY" >/tmp/key; ip link add wg0 type wireguard; wg set wg0 private-key /tmp/key peer $SERVER_PUB_a endpoint $server_pod_ip:51820 allowed-ips 10.99.1.0/24; ip address add 10.99.1.4/24 dev wg0; ip link set wg0 up; exec sleep infinity' ]
      env:
        - name: KEY
          value: "$intruder_priv"
      securityContext:
        capabilities:
          add: [ NET_ADMIN ]
EOF
k wait --for=condition=Ready pod/demux-intruder --timeout=2m >/dev/null
n="$(k exec demux-intruder -- ping -c 10 -i 0.5 -W 1 10.99.1.1 2>&1 | sed -n 's/.* \([0-9]*\) received.*/\1/p' || true)"
hs="$(k exec demux-intruder -- wg show wg0 latest-handshakes | awk '{print $2}')"
echo "replies: $n, latest handshake: $hs"
[[ "$n" == 0 && "$hs" == 0 ]] || fail "a pod outside laboratory-wg reached the VPN server directly"
k delete pod demux-intruder --wait=false >/dev/null
k -n lg-demux-a exec vpn -- wg set wg0 peer "$intruder_pub" remove

log "restart of the demux pod on $ENTRY_NODE"
pod="$(k -n "$WG_NS" get pod -l app=laboratory-proxy-wg --field-selector "spec.nodeName=$ENTRY_NODE" -o jsonpath='{.items[0].metadata.name}')"
client a sh -c 'rm -f /tmp/restart.log; ping -i 0.2 -W 1 10.99.1.1 >/tmp/restart.log 2>&1 &'
start=$SECONDS
k -n "$WG_NS" delete pod "$pod" --wait >/dev/null
k -n "$WG_NS" rollout status ds/laboratory-proxy-wg --timeout=5m >/dev/null
echo "new pod ready after $((SECONDS - start))s"
t="$(wait_tunnel a 10.99.1.1 90)" || fail "client a: no tunnel 90 s after the restart"
sleep 2
client a pkill -INT ping || true
sleep 1
summary="$(client a sh -c 'grep "packets transmitted" /tmp/restart.log')"
sent="$(sed -n 's/^\([0-9]*\) packets transmitted.*/\1/p' <<<"$summary")"
got="$(sed -n 's/.* \([0-9]*\) received.*/\1/p' <<<"$summary")"
lost=$((sent - got))
echo "restart: $summary (lost $lost pings = $((lost / 5)) s)"
if [[ "$MODE" == userspace ]]; then
  echo "userspace: the client re-handshakes with the new pod (allowed)"
else
  ((lost <= 10)) || fail "lost $lost pings (more than 2 s) across the restart"
fi
n="$(received a -c 20 -i 0.2 -W 1 10.99.1.1)"
[[ "$n" == 20 ]] || fail "client a after the restart: lost $((20 - n)) of 20 pings"
if [[ "$MODE" == userspace ]]; then
  # An idle client (keepalives only) re-handshakes only once it sends data and gets no answer (15 s).
  t="$(wait_tunnel b 10.99.2.1 60)" || fail "client b: no tunnel 60 s after the restart"
  echo "client b: tunnel again after ${t}s of pinging"
fi
n="$(received b -c 20 -i 0.2 -W 1 10.99.2.1)"
[[ "$n" == 20 ]] || fail "client b after the restart: lost $((20 - n)) of 20 pings"

for g in a b; do
  log "client $g: roam (new source port)"
  sleep "$ROAM_INTERVAL"
  client "$g" wg set wg0 listen-port 41000
  t="$(wait_tunnel "$g" "$(server_of "$g")" $((ROAM_INTERVAL + KEEPALIVE + 5)))" ||
    fail "client $g: no tunnel within roamInterval + keepalive after the roam"
  echo "tunnel after the roam: ${t}s"
  n="$(received "$g" -c 20 -i 0.2 -W 1 "$(server_of "$g")")"
  [[ "$n" == 20 ]] || fail "client $g after the roam: lost $((20 - n)) of 20 pings"
done

printf '\nPASS (mode %s)\n' "$MODE"
