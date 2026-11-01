#!/usr/bin/env bash
# End-to-end test of the WireGuard demux on the kind cluster of up.sh, in one mode (userspace, tc or xdp), or the switch back from a
# fast path to userspace (detach):
#
#   make kind-demux-e2e KIND_DEMUX_MODE=tc   # builds the proxy image, then runs hack/kind-demux/e2e.sh tc
#   make kind-demux-e2e-all                  # builds once, then e2e.sh upgrade, userspace, tc, xdp, lbipam.sh and e2e.sh detach
#
# A mode run deploys the demux part of the chart (templates/wg and its network policy, rendered with helm template -s: the rest of the
# chart needs the operator, cert-manager and the Gateway API), two groups whose VPN servers are plain kernel WireGuard pods
# (vpn-server.yaml), one on the demux's node and one on the other worker, each with the operator's VPN pod policy (printed from
# vpnCiliumPolicy by TestPrintVPNCiliumPolicy, not a hand copy), LabGroups whose status says the VPN is registered, and a UDP and TCP
# echo behind a NodePort Service of its own. Two client containers on the kind docker network connect through the node port of the
# demux's Service on the first worker. It checks:
#
#   - both clients handshake and ping their server with no loss, also with 1300-byte packets; in tc and xdp the fast path counters
#     (:8083/metrics on the node's address) take the packets and the demux socket sees next to no transport packets; in userspace the
#     socket sees them all;
#   - tc and xdp: our tcx programs come before Cilium's on the external device; xdp attaches in generic mode (kind's veth);
#   - a client with an unknown key never gets a handshake; a pod outside laboratory-wg cannot reach a VPN server directly;
#   - a restart of Cilium loses at most 2 s of pings, the order is back within DEMUX_BPF_RESYNC + 5 s, the echo still answers;
#   - tc and xdp: a foreign tcx program put at the head of eth0 (headprog) has ours first again within DEMUX_BPF_RESYNC + 5 s;
#   - a restart of the demux pod loses at most 2 s of pings in tc and xdp (userspace may re-handshake);
#   - a client that changes its source port keeps its tunnel, and its server sees the demux's source, never the client's address;
#   - tc and xdp: Cilium holds no NAT entry and no conntrack entry beyond the handshakes for the clients' tunnels, and the transport
#     packets leave its conntrack untouched.
#
# A tc run expects no xdp program on eth0: after an xdp run, run detach first (kind-demux-e2e-all runs userspace, tc, xdp in that order).
#
# upgrade (on a cluster where no release laboratory exists, or one this step made) installs the chart of KIND_DEMUX_UPGRADE_FROM
# (the last release tag) with most components off and the VPN Service on the cloud's fixed node port, checks that a plain helm upgrade to this
# tree fails on that node port, deletes the old Service as DEPLOY.md says and upgrades again, checks the chart's default mode (userspace)
# and policy (Cluster), runs the userspace checks and uninstalls the release.
#
# detach runs after a tc or xdp run: it switches the chart to userspace, runs `proxy-wg detach` in a one-off pod with the demux's tc
# privileges on every worker, checks that our links and pins are gone and that both clients' tunnels work again on the slow path.
#
# Every check fails the run (exit 1). Each run appends its result lines to $KIND_DEMUX_SUMMARY when set. Leaves everything running
# for inspection; the next run starts the test objects afresh. Exit status 0 = PASS.
set -euo pipefail

MODE="${1:-}"
case "$MODE" in upgrade | userspace | tc | xdp | detach) ;; *) echo "usage: $0 upgrade|userspace|tc|xdp|detach" >&2; exit 2 ;; esac
# upgrade installs the chart of KIND_DEMUX_UPGRADE_FROM (the last release tag), upgrades it to this tree with the chart's default mode, then runs
# the userspace checks.
DEMUX_MODE="$MODE"
[[ "$MODE" == upgrade ]] && DEMUX_MODE=userspace

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
CLUSTER="${KIND_DEMUX_CLUSTER:-icebox-demux}"
IMG="${KIND_DEMUX_IMG:-cybericebox/laboratory-proxy:kind-demux}"
WG_IMG="icebox-demux/wg-test:2"
RELEASE_NS="laboratory-system"
WG_NS="laboratory-wg"
ECHO_NS="demux-echo"
ENTRY_NODE="$CLUSTER-worker"
OTHER_NODE="$CLUSTER-worker2"
WORKERS=("$ENTRY_NODE" "$OTHER_NODE")
PIN_PATH="/sys/fs/bpf/cybericebox/wg-demux"
KEEPALIVE=5
ROAM_INTERVAL=5
# DEMUX_BPF_RESYNC: the chart does not set it, so the code default.
RESYNC=10
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

k() { kubectl --context "kind-$CLUSTER" "$@"; }
log() { printf '\n== %s\n' "$*"; }
fail() { printf '\nFAIL (%s): %s\n' "$MODE" "$*" >&2; exit 1; }
summary() {
  printf 'SUMMARY %s: %s\n' "$MODE" "$*"
  if [[ -n "${KIND_DEMUX_SUMMARY:-}" ]]; then printf '%-9s %s\n' "$MODE" "$*" >>"$KIND_DEMUX_SUMMARY"; fi
}
fast() { [[ "$MODE" == tc || "$MODE" == xdp ]]; }
genkey() { docker run --rm "$WG_IMG" sh -c 'k=$(wg genkey); echo "$k $(echo "$k" | wg pubkey)"'; }
client() { docker exec "icebox-demux-client-$1" "${@:2}"; }
client_ip() { docker inspect -f '{{.NetworkSettings.Networks.kind.IPAddress}}' "icebox-demux-client-$1"; }
server_of() { [[ $1 == b ]] && echo 10.99.2.1 || echo 10.99.1.1; }

# received <client> <ping args...>: runs ping in the client and prints the number of replies (0 when ping printed no summary).
received() {
  local out n
  out="$(client "$1" ping "${@:2}" 2>&1 || true)"
  n="$(sed -n 's/.* \([0-9]*\) received.*/\1/p' <<<"$out")"
  [[ -n "$n" ]] || printf 'client %s: ping printed no summary: %s\n' "$1" "$(tail -3 <<<"$out")" >&2
  echo "${n:-0}"
}

# wait_tunnel <client> <server ip> <seconds>: waits until one ping gets through; prints the seconds it took.
wait_tunnel() {
  local start=$SECONDS
  while ((SECONDS - start < $3)); do
    if client "$1" ping -c 1 -W 1 "$2" >/dev/null 2>&1; then echo $((SECONDS - start)); return 0; fi
    sleep 0.2
  done
  return 1
}

# ping_start <client> <server ip> <name>: pings every 0.2 s in the background inside the client (one at a time per client).
ping_start() { client "$1" sh -c "rm -f /tmp/$3.log; ping -i 0.2 -W 1 $2 >/tmp/$3.log 2>&1 &"; }
# ping_stop <client> <name>: stops it and prints "<sent> <received>".
ping_stop() {
  local out
  client "$1" pkill -INT ping || true
  sleep 1
  out="$(client "$1" cat "/tmp/$2.log")"
  grep -q 'packets transmitted' <<<"$out" || fail "client $1: the background ping ($2) printed no summary: $(tail -3 <<<"$out")"
  sed -n 's/^\([0-9]*\) packets transmitted, \([0-9]*\) received.*/\1 \2/p' <<<"$out"
}

cilium_pod() { k -n kube-system get pod -l k8s-app=cilium --field-selector "spec.nodeName=$1" -o jsonpath='{.items[0].metadata.name}'; }
cilium() { k -n kube-system exec "$(cilium_pod "$1")" -c cilium-agent -- "${@:2}"; }

# order_ok: on the entry node's eth0 our tcx programs come first in both directions and Cilium's right after them; sets ORDER.
order_ok() {
  local out ingress egress
  out="$(cilium "$ENTRY_NODE" bpftool net show dev eth0)"
  ingress="$(awk '/tcx\/ingress/ {print $3}' <<<"$out" | head -2 | paste -sd' ' -)"
  egress="$(awk '/tcx\/egress/ {print $3}' <<<"$out" | head -2 | paste -sd' ' -)"
  ORDER="ingress: $ingress; egress: $egress"
  [[ "$ingress" == "wg_tc_ingress cil_from_netdev" && "$egress" == "wg_tc_egress_guard cil_to_netdev" ]]
}

# metrics: the demux's metrics summed over METRICS_URLS (<node>=<url>), each read from its own node (in userspace the pod policy admits
# that node's host only). In userspace the Service's policy is Cluster, so a client may be served by the demux of any node.
metrics() {
  local u
  for u in $METRICS_URLS; do docker exec "${u%%=*}" curl -sf --max-time 5 "${u#*=}" || fail "no metrics at ${u#*=} from ${u%%=*}"; done |
    awk '/^wg_/ {v[$1] += $2} END {for (s in v) printf "%s %d\n", s, v[s]}'
}
# metric <metrics> <series>: the value of one series, 0 when it is absent.
metric() { awk -v s="$2" '$1 == s {print $2; f = 1} END {if (!f) print 0}' <<<"$1"; }
TO_SERVER='wg_demux_fastpath_packets_total{result="forwarded_to_server"}'
TO_CLIENT='wg_demux_fastpath_packets_total{result="forwarded_to_client"}'
TRANSPORT='wg_demux_packets_total{type="transport"}'
RESPONSES='wg_demux_packets_total{type="handshake_response"}'
demux_pod() { k -n "$WG_NS" get pod -l app=laboratory-proxy-wg --field-selector "spec.nodeName=$ENTRY_NODE" -o jsonpath='{.items[0].metadata.name}'; }
demux_ips() { k -n "$WG_NS" get pod -l app=laboratory-proxy-wg -o jsonpath='{.items[*].status.podIP}'; }
set_metrics_url() {
  METRICS_URLS="$ENTRY_NODE=http://$ENTRY_IP:8083/metrics"
  if [[ "$DEMUX_MODE" == userspace || "$MODE" == detach ]]; then
    METRICS_URLS="$(k -n "$WG_NS" get pod -l app=laboratory-proxy-wg \
      -o jsonpath='{range .items[*]}{.spec.nodeName}=http://{.status.podIP}:8083/metrics{" "}{end}')"
  fi
}

# echo_ok <when>: the echo Service still answers on the entry node's node ports, UDP and TCP, from a container outside the tunnels.
echo_ok() {
  local u t
  u="$(client bad sh -c "echo udp-$1 | socat -t 2 -T 3 - UDP4:$ENTRY_IP:$ECHO_UDP" 2>&1 || true)"
  t="$(client bad sh -c "echo tcp-$1 | socat -T 3 - TCP4:$ENTRY_IP:$ECHO_TCP" 2>&1 || true)"
  [[ "$u" == "udp-$1" ]] || fail "the echo Service did not answer on UDP $ENTRY_IP:$ECHO_UDP ($1): $u"
  [[ "$t" == "tcp-$1" ]] || fail "the echo Service did not answer on TCP $ENTRY_IP:$ECHO_TCP ($1): $t"
  echo "echo ($1): UDP and TCP answer"
}

# deploy_render <mode>: the demux part of the chart in that mode, into $WORK/demux.yaml.
# deploy <mode>: applies it, restarted so the pods run the image just loaded.
deploy_render() {
  local templates show=()
  templates=(priorityclasses.yaml wg/namespace.yaml wg/serviceaccount.yaml wg/clusterrole.yaml wg/clusterrolebinding.yaml wg/role.yaml
    wg/service.yaml wg/daemonset.yaml)
  # The demux's pod policy renders in userspace only (on the host network pod policies do not apply), and helm template -s refuses a
  # template that renders nothing.
  [[ "$1" == userspace ]] && templates+=(netpol/wg-demux.yaml)
  for t in "${templates[@]}"; do show+=(-s "templates/$t"); done
  helm template laboratory "$ROOT/charts/laboratory" --namespace "$RELEASE_NS" \
    --set proxy.wg.mode="$1" --set proxy.l7.enabled=false --set proxy.wg.limits.roamInterval="${ROAM_INTERVAL}s" \
    --set proxy.image.repository="${IMG%:*}" --set proxy.image.tag="${IMG##*:}" \
    --set operator.baseDomain=demux.test --set operator.publicVPNEndpoint=vpn.demux.test --set operator.supportEmail=demux@demux.test \
    "${show[@]}" >"$WORK/demux.yaml"
}
deploy() {
  deploy_render "$1"
  if [[ "$1" != userspace ]]; then
    k -n "$WG_NS" delete networkpolicy,ciliumnetworkpolicy laboratory-proxy-wg --ignore-not-found >/dev/null
  fi
  k apply -f "$WORK/demux.yaml" >/dev/null
  k -n "$WG_NS" rollout restart ds/laboratory-proxy-wg >/dev/null
  k -n "$WG_NS" rollout status ds/laboratory-proxy-wg --timeout=5m
}

# The chart values of the upgrade step: everything that needs a controller kind does not have (Gateway API, cert-manager, the lab
# scheduler) is off, the images of the other components point at a tag that does not exist (their pods only wait), and the VPN Service has
# the fixed node port of the cloud.
UPGRADE_NODE_PORT=31820
upgrade_values=(--set operator.baseDomain=demux.test --set operator.publicVPNEndpoint=vpn.demux.test --set operator.supportEmail=demux@demux.test
  --set gateway.enabled=false --set certManager.enabled=false --set labScheduler.enabled=false --set scheduler.enabled=false
  --set image.tag=kind-demux-absent --set proxy.wg.nodePort="$UPGRADE_NODE_PORT")
helm_k() { helm --kube-context "kind-$CLUSTER" "$@"; }

# upgrade: a release of the chart of KIND_DEMUX_UPGRADE_FROM, then helm upgrade to this tree with no proxy.wg.mode. From a base that still has
# the VPN Service in laboratory-proxy a plain upgrade must fail while the old Service holds the fixed node port, and after the documented step
# (DEPLOY.md, "Upgrade order") it must succeed; from a base that has it in laboratory-wg already (every release tag so far) the plain upgrade
# must succeed. Either way the result has the chart's default mode (userspace) and policy (Cluster).
upgrade() {
  # The last RELEASE (the harness handles a base on either side of the move of the Service, see below).
  local from="${KIND_DEMUX_UPGRADE_FROM:-$(git -C "$ROOT" tag --list 'v*' | grep -v -- '-rc' | sort -V | tail -1)}" out np
  [[ -n "$from" ]] || fail "no release tag to upgrade from (set KIND_DEMUX_UPGRADE_FROM)"
  log "clean slate: the demux of earlier steps was applied without Helm"
  helm_k -n "$RELEASE_NS" uninstall laboratory --ignore-not-found --wait >/dev/null
  deploy_render userspace
  k delete -f "$WORK/demux.yaml" --ignore-not-found --wait >/dev/null
  k delete namespace laboratory-proxy --ignore-not-found --wait >/dev/null

  log "helm install the chart of $from"
  mkdir -p "$WORK/from"
  git -C "$ROOT" archive "$from" charts/laboratory | tar -x -C "$WORK/from"
  # CRDs are applied by kubectl, as DEPLOY.md says (helm never updates crds/): up.sh applied the CRDs of this tree with server-side apply,
  # so put the CRDs of the old chart in their place first (forced: the field manager is kubectl either way) and tell helm to leave them.
  k apply --server-side --force-conflicts -f "$WORK/from/charts/laboratory/crds" >/dev/null
  helm_k install laboratory "$WORK/from/charts/laboratory" -n "$RELEASE_NS" --create-namespace --skip-crds "${upgrade_values[@]}" >/dev/null
  # Where the old chart put the VPN Service: every release tag so far already has it in laboratory-wg (the move out of laboratory-proxy is
  # older than all of them), so a release shows no move; a base that still has it in laboratory-proxy (KIND_DEMUX_UPGRADE_FROM may name
  # one) shows the move that the documented step is about.
  local old_ns=""
  for ns in laboratory-proxy "$WG_NS"; do
    if np="$(k -n "$ns" get svc laboratory-proxy-wg -o jsonpath='{.spec.ports[0].nodePort}' 2>/dev/null)" && [[ -n "$np" ]]; then old_ns="$ns"; break; fi
  done
  [[ -n "$old_ns" ]] || fail "the chart of $from has no VPN Service in laboratory-proxy or $WG_NS"
  [[ "$np" == "$UPGRADE_NODE_PORT" ]] || fail "the old VPN Service has node port $np"
  summary "installed the chart of $from ($(git -C "$ROOT" rev-parse --short "$from")): VPN Service $old_ns/laboratory-proxy-wg on node port $np"

  local up=(upgrade laboratory "$ROOT/charts/laboratory" -n "$RELEASE_NS" "${upgrade_values[@]}" --set proxy.wg.limits.roamInterval="${ROAM_INTERVAL}s"
    --set proxy.image.repository="${IMG%:*}" --set proxy.image.tag="${IMG##*:}")
  if [[ "$old_ns" == laboratory-proxy ]]; then
    log "helm upgrade to this tree without the step"
    if out="$(helm_k "${up[@]}" 2>&1)"; then fail "the plain upgrade succeeded while the old Service held the node port (DEPLOY.md says it fails)"; fi
    grep -q "already allocated" <<<"$out" || fail "the plain upgrade failed for another reason: $out"
    summary "plain helm upgrade fails as documented: $(grep -o '[^:]*already allocated' <<<"$out" | head -1)"
    log "the documented step, then helm upgrade"
    k -n laboratory-proxy delete svc laboratory-proxy-wg --ignore-not-found >/dev/null
  else
    log "the Service is already in $WG_NS (no move, no step): the plain helm upgrade must succeed"
  fi
  out="$(helm_k "${up[@]}" 2>&1)" || fail "helm upgrade failed: $out"
  k -n "$WG_NS" rollout status ds/laboratory-proxy-wg --timeout=5m
  local mode etp
  mode="$(k -n "$WG_NS" get ds laboratory-proxy-wg -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="DEMUX_MODE")].value}')"
  etp="$(k -n "$WG_NS" get svc laboratory-proxy-wg -o jsonpath='{.spec.externalTrafficPolicy}')"
  np="$(k -n "$WG_NS" get svc laboratory-proxy-wg -o jsonpath='{.spec.ports[0].nodePort}')"
  [[ "$mode" == userspace && "$etp" == Cluster && "$np" == "$UPGRADE_NODE_PORT" ]] ||
    fail "after the upgrade: mode $mode, externalTrafficPolicy $etp, node port $np (want userspace, Cluster, $UPGRADE_NODE_PORT)"
  ! k -n laboratory-proxy get svc laboratory-proxy-wg >/dev/null 2>&1 || fail "the old VPN Service is back"
  log "the CRDs of this tree (helm upgrade does not update crds/)"
  k apply --server-side --force-conflicts -f "$ROOT/charts/laboratory/crds" >/dev/null
  k wait --for=condition=Established crd --all --timeout=2m >/dev/null
  summary "after the step helm upgrade succeeds: $(helm_k -n "$RELEASE_NS" status laboratory -o json | jq -r '"revision \(.version) \(.info.status)"'), mode $mode, externalTrafficPolicy $etp, node port $np"
}

log "images"
printf 'FROM alpine:3.22\nRUN apk add --no-cache wireguard-tools iproute2 iputils socat\n' | docker build -q -t "$WG_IMG" - >/dev/null
kind load docker-image "$IMG" "$WG_IMG" --name "$CLUSTER" >/dev/null
if fast; then
  case "$(docker exec "$ENTRY_NODE" uname -m)" in aarch64) arch=arm64 ;; x86_64) arch=amd64 ;; *) fail "unknown node architecture" ;; esac
  (cd "$ROOT" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$WORK/headprog" ./hack/kind-demux/headprog)
  docker cp "$WORK/headprog" "$ENTRY_NODE:/usr/local/bin/demux-e2e-headprog" >/dev/null
  docker exec "$ENTRY_NODE" demux-e2e-headprog remove eth0
fi
NODE_PORT="$(k -n "$WG_NS" get svc laboratory-proxy-wg -o jsonpath='{.spec.ports[0].nodePort}' 2>/dev/null || true)"
ENTRY_IP="$(k get node "$ENTRY_NODE" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"

if [[ "$MODE" == detach ]]; then
  for c in a b; do docker inspect "icebox-demux-client-$c" >/dev/null 2>&1 || fail "no client $c: run e2e.sh tc or xdp first"; done
  for node in "${WORKERS[@]}"; do
    docker exec "$node" test -d "$PIN_PATH" || fail "nothing pinned on $node: run e2e.sh tc or xdp first"
  done

  log "switch the chart to userspace"
  deploy userspace
  set_metrics_url

  for node in "${WORKERS[@]}"; do
    log "proxy-wg detach on $node"
    k -n default delete pod "demux-detach-$node" --ignore-not-found --wait >/dev/null
    k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: demux-detach-$node
  namespace: default
spec:
  nodeName: $node
  restartPolicy: Never
  hostNetwork: true
  securityContext:
    runAsNonRoot: false
    runAsUser: 0
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: detach
      image: $IMG
      imagePullPolicy: Never
      command: [ /proxy, proxy-wg, detach ]
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: [ ALL ]
          add: [ BPF, NET_ADMIN, PERFMON ]
      volumeMounts:
        - name: bpffs
          mountPath: /sys/fs/bpf
  volumes:
    - name: bpffs
      hostPath:
        path: /sys/fs/bpf
        type: Directory
EOF
    k -n default wait --for=jsonpath='{.status.phase}'=Succeeded "pod/demux-detach-$node" --timeout=2m >/dev/null ||
      fail "proxy-wg detach on $node did not succeed: $(k -n default logs "demux-detach-$node" 2>&1 | tail -5)"
    k -n default logs "demux-detach-$node"
    k -n default delete pod "demux-detach-$node" --wait=false >/dev/null
    left="$(cilium "$node" bpftool net show | grep -E 'wg_|^eth0\([0-9]+\) (generic|driver|offload)' || true)"
    [[ -z "$left" ]] || fail "our programs are still attached on $node: $left"
    ! docker exec "$node" test -e "$PIN_PATH" || fail "$PIN_PATH is still there on $node"
    echo "$node: no links, no pins"
  done
  summary "proxy-wg detach removed every link and pin on ${WORKERS[*]}"

  before="$(metrics)"
  for g in a b; do
    log "client $g: tunnel on the slow path"
    # The new userspace pod knows no session: the client re-handshakes once its data gets no answer.
    t="$(wait_tunnel "$g" "$(server_of "$g")" 60)" || fail "client $g: no tunnel 60 s after the detach"
    n="$(received "$g" -c 20 -i 0.2 -W 1 "$(server_of "$g")")"
    [[ "$n" == 20 ]] || fail "client $g after the detach: lost $((20 - n)) of 20 pings"
    echo "tunnel after ${t}s, ping 20/20"
  done
  after="$(metrics)"
  d=$(($(metric "$after" "$TRANSPORT") - $(metric "$before" "$TRANSPORT")))
  ((d >= 80)) || fail "the userspace demux saw only $d transport packets for 2 x 20 pings"
  summary "slow path after the detach: both tunnels back, ping 20/20 each, userspace transport +$d"
  printf '\nPASS (detach)\n'
  exit 0
fi

log "fresh test objects"
for c in $(docker ps -a --filter "label=icebox-demux-client" --format '{{.Names}}'); do docker rm -f "$c" >/dev/null; done
k delete labgroups.laboratory.cybericebox.com demux-a demux-b --ignore-not-found --wait
k delete namespace lg-demux-a lg-demux-b "$ECHO_NS" --ignore-not-found --wait
k create namespace "$RELEASE_NS" --dry-run=client -o yaml | k apply -f - >/dev/null

# group <letter> <node> <server tunnel ip> <client tunnel ip>: the VPN server, its policy and the LabGroup of one group.
group() {
  local g="$1" ns="lg-demux-$1" spriv spub cpriv cpub
  read -r spriv spub <<<"$(genkey)"
  read -r cpriv cpub <<<"$(genkey)"
  printf -v "SERVER_PUB_$g" %s "$spub"
  printf -v "CLIENT_PRIV_$g" %s "$cpriv"
  sed -e "s|__NS__|$ns|g" -e "s|__GROUP__|demux-$g|g" -e "s|__NODE__|$2|g" -e "s|__PRIVATE_KEY__|$spriv|g" -e "s|__IMAGE__|$WG_IMG|g" \
    -e "s|__PEER_KEY__|$cpub|g" -e "s|__SERVER_IP__|$3|g" -e "s|__CLIENT_IP__|$4|g" "$HERE/vpn-server.yaml" | k apply -f - >/dev/null
  (cd "$ROOT" && VPN_POLICY_NS="$ns" VPN_POLICY_MODE="$DEMUX_MODE" VPN_POLICY_OUT="$WORK/policy-$g.yaml" GOWORK=off \
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

log "echo Service (UDP and TCP) on $OTHER_NODE"
k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $ECHO_NS
---
apiVersion: v1
kind: Pod
metadata:
  name: echo
  namespace: $ECHO_NS
  labels:
    app: echo
spec:
  nodeName: $OTHER_NODE
  terminationGracePeriodSeconds: 1
  containers:
    - name: echo
      image: $WG_IMG
      imagePullPolicy: Never
      # A UDP child never sees an end of input: timeout ends its cat (with a plain cat the first child hangs and takes the socket).
      command: [ sh, -ec, 'socat UDP4-RECVFROM:8080,fork SYSTEM:"timeout 1 cat" & exec socat TCP4-LISTEN:8080,fork,reuseaddr EXEC:cat' ]
---
apiVersion: v1
kind: Service
metadata:
  name: echo
  namespace: $ECHO_NS
spec:
  type: NodePort
  selector:
    app: echo
  ports:
    - name: udp
      protocol: UDP
      port: 8080
    - name: tcp
      protocol: TCP
      port: 8080
EOF
k -n "$ECHO_NS" wait --for=condition=Ready pod/echo --timeout=2m >/dev/null
ECHO_UDP="$(k -n "$ECHO_NS" get svc echo -o jsonpath='{.spec.ports[?(@.name=="udp")].nodePort}')"
ECHO_TCP="$(k -n "$ECHO_NS" get svc echo -o jsonpath='{.spec.ports[?(@.name=="tcp")].nodePort}')"

if [[ "$MODE" == upgrade ]]; then
  upgrade
else
  log "demux (mode $MODE)"
  deploy "$MODE"
fi
NODE_PORT="$(k -n "$WG_NS" get svc laboratory-proxy-wg -o jsonpath='{.spec.ports[0].nodePort}')"
set_metrics_url
echo "entry: $ENTRY_IP:$NODE_PORT, metrics: $METRICS_URLS"

if fast; then
  log "attachment on $ENTRY_NODE"
  order_ok || fail "our tcx programs are not first on eth0 ($ORDER)"
  echo "$ORDER"
  xdp="$(cilium "$ENTRY_NODE" bpftool net show dev eth0 | grep -E '^eth0\([0-9]+\) (generic|driver|offload)' || true)"
  if [[ "$MODE" == xdp ]]; then
    grep -q ' generic ' <<<"$xdp" || fail "xdp is not attached in generic mode on eth0: ${xdp:-none}"
    summary "xdp on eth0: $xdp"
  else
    [[ -z "$xdp" ]] || fail "an xdp program is attached on eth0 in tc mode: $xdp"
  fi
  summary "tcx order on eth0 at the start: $ORDER"
fi

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
echo_ok start

for g in a b; do
  log "client $g: handshake and ping"
  t="$(wait_tunnel "$g" "$(server_of "$g")" 30)" || fail "client $g: no tunnel within 30 s"
  echo "tunnel up after ${t}s"
  [[ "$(client "$g" wg show wg0 latest-handshakes | awk '{print $2}')" != 0 ]] || fail "client $g: no handshake"
  before="$(metrics)"
  n="$(received "$g" -c 50 -i 0.2 -W 1 "$(server_of "$g")")"
  after="$(metrics)"
  echo "ping: $n/50"
  [[ "$n" == 50 ]] || fail "client $g: lost $((50 - n)) of 50 pings"
  dt=$(($(metric "$after" "$TRANSPORT") - $(metric "$before" "$TRANSPORT")))
  if fast; then
    ds=$(($(metric "$after" "$TO_SERVER") - $(metric "$before" "$TO_SERVER")))
    dc=$(($(metric "$after" "$TO_CLIENT") - $(metric "$before" "$TO_CLIENT")))
    ((ds >= 50 && dc >= 50)) || fail "client $g: the fast path forwarded only $ds to the server and $dc to the client for 50 pings"
    ((dt <= 5)) || fail "client $g: the demux socket still saw $dt transport packets in $MODE mode"
    summary "client $g ping 50/50: forwarded_to_server +$ds, forwarded_to_client +$dc, userspace transport +$dt"
  else
    ((dt >= 100)) || fail "client $g: the demux socket saw only $dt transport packets for 50 pings"
    summary "client $g ping 50/50: userspace transport +$dt"
  fi
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
if [[ "$n" != 0 || "$hs" != 0 ]]; then
  # What decides between a dirty harness and a leak: who knows the key (the server's peers), what the client has, what the demux counted.
  echo "--- client bad: wg show"; client bad wg show wg0 || true
  echo "--- server a: wg show"; k -n lg-demux-a exec vpn -- wg show wg0 || true
  echo "--- server b: wg show"; k -n lg-demux-b exec vpn -- wg show wg0 || true
  echo "--- demux metrics"; metrics 2>/dev/null | grep -E 'wg_demux_(sessions|fastpath_packets_total|packets_total)' || true
  fail "the client with an unknown key got through"
fi

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
echo "replies: ${n:-0}, latest handshake: $hs"
[[ "${n:-0}" == 0 && "$hs" == 0 ]] || fail "a pod outside laboratory-wg reached the VPN server directly"
k delete pod demux-intruder --wait=false >/dev/null
k -n lg-demux-a exec vpn -- wg set wg0 peer "$intruder_pub" remove

log "restart of Cilium"
ping_start a 10.99.1.1 cilium
ping_start b 10.99.2.1 cilium
k -n kube-system rollout restart ds/cilium >/dev/null
k -n kube-system rollout status ds/cilium --timeout=5m >/dev/null
if fast; then
  start=$SECONDS
  until order_ok; do
    ((SECONDS - start <= RESYNC + 5)) || fail "the order on eth0 is not back $((RESYNC + 5)) s after the Cilium restart ($ORDER)"
    sleep 1
  done
  echo "order after the Cilium restart (after $((SECONDS - start)) s): $ORDER"
  # Cilium updates its links in place, so this alone does not exercise the demux's resync; the foreign program below does.
  summary "tcx order on eth0 after the Cilium restart (+$((SECONDS - start)) s): $ORDER"
fi
sleep 2
for g in a b; do
  res="$(ping_stop "$g" cilium)"
  read -r sent got <<<"$res"
  lost=$((sent - got))
  echo "client $g across the Cilium restart: $got/$sent (lost $lost)"
  ((lost <= 10)) || fail "client $g lost $lost pings (more than 2 s) across the Cilium restart"
  summary "client $g across the Cilium restart: $got/$sent pings, lost $lost"
done
echo_ok after-cilium-restart

if fast; then
  log "a foreign tcx program at the head of eth0, in front of ours (ingress and egress)"
  first() { cilium "$ENTRY_NODE" bpftool net show dev eth0 | awk '/tcx\/ingress/ && !i {i = $3} /tcx\/egress/ && !e {e = $3} END {print i, e}'; }
  ping_start a 10.99.1.1 head
  # A failed run must not leave the foreign program in front of ours.
  trap 'docker exec "$ENTRY_NODE" demux-e2e-headprog remove eth0 >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT
  # headprog checks itself that it is first right after each attach, before the demux's resync can act.
  docker exec "$ENTRY_NODE" demux-e2e-headprog attach eth0 || fail "the foreign program did not go first"
  start=$SECONDS
  until [[ "$(first)" == "wg_tc_ingress wg_tc_egress_guard" ]]; do
    ((SECONDS - start <= RESYNC + 5)) || fail "the demux is not first again $((RESYNC + 5)) s after a foreign program went to the head ($(first))"
    sleep 1
  done
  took=$((SECONDS - start))
  echo "ours first again after ${took}s: $(cilium "$ENTRY_NODE" bpftool net show dev eth0 | awk '/tcx\// {print $2, $3}' | paste -sd, -)"
  docker exec "$ENTRY_NODE" demux-e2e-headprog remove eth0
  trap 'rm -rf "$WORK"' EXIT
  order_ok || fail "the order on eth0 after the foreign program left: $ORDER"
  sleep 1
  res="$(ping_stop a head)"
  read -r sent got <<<"$res"
  lost=$((sent - got))
  ((lost <= 10)) || fail "client a lost $lost pings (more than 2 s) while the demux re-anchored"
  summary "foreign tcx program at the head: ours first again after ${took} s (resync ${RESYNC} s), client a $got/$sent pings, lost $lost; after its removal: $ORDER"
fi

log "restart of the demux pod on $ENTRY_NODE"
before="$(metrics)"
pod="$(demux_pod)"
ping_start a 10.99.1.1 restart
start=$SECONDS
k -n "$WG_NS" delete pod "$pod" --wait >/dev/null
k -n "$WG_NS" rollout status ds/laboratory-proxy-wg --timeout=5m >/dev/null
echo "new pod ready after $((SECONDS - start))s"
set_metrics_url
t="$(wait_tunnel a 10.99.1.1 90)" || fail "client a: no tunnel 90 s after the restart"
sleep 2
res="$(ping_stop a restart)"
read -r sent got <<<"$res"
lost=$((sent - got))
echo "restart: $got/$sent (lost $lost pings = $((lost / 5)) s)"
if [[ "$DEMUX_MODE" == userspace ]]; then
  echo "userspace: the client re-handshakes with the new pod (allowed)"
else
  ((lost <= 10)) || fail "lost $lost pings (more than 2 s) across the restart"
fi
n="$(received a -c 20 -i 0.2 -W 1 10.99.1.1)"
[[ "$n" == 20 ]] || fail "client a after the restart: lost $((20 - n)) of 20 pings"
if [[ "$DEMUX_MODE" == userspace ]]; then
  # An idle client (keepalives only) re-handshakes only once it sends data and gets no answer (15 s).
  t="$(wait_tunnel b 10.99.2.1 60)" || fail "client b: no tunnel 60 s after the restart"
  echo "client b: tunnel again after ${t}s of pinging"
fi
n="$(received b -c 20 -i 0.2 -W 1 10.99.2.1)"
[[ "$n" == 20 ]] || fail "client b after the restart: lost $((20 - n)) of 20 pings"
after="$(metrics)"
summary "demux restart: client a $got/$sent pings, lost $lost; the new pod: sessions $(metric "$after" wg_demux_sessions), handshake responses $(metric "$after" "$RESPONSES")"
echo_ok after-demux-restart

for g in a b; do
  log "client $g: roam (new source port)"
  sleep "$ROAM_INTERVAL"
  if fast; then before="$(metrics)"; fi
  client "$g" wg set wg0 listen-port 41000
  # In userspace the Service's policy is Cluster: the new source port is a new flow for Cilium, which may hand it to the demux of another
  # node, which knows no session; the client then re-handshakes (keepalive + rekey timeout, about 15 s), as with the replicas before the
  # modes. In tc and xdp (Local) the entry node keeps the session.
  limit=$((ROAM_INTERVAL + KEEPALIVE + 5))
  [[ "$DEMUX_MODE" == userspace ]] && limit=40
  t="$(wait_tunnel "$g" "$(server_of "$g")" "$limit")" || fail "client $g: no tunnel within $limit s after the roam"
  echo "tunnel after the roam: ${t}s"
  n="$(received "$g" -c 20 -i 0.2 -W 1 "$(server_of "$g")")"
  [[ "$n" == 20 ]] || fail "client $g after the roam: lost $((20 - n)) of 20 pings"
  roam=""
  if fast; then
    # The new port first misses the fast path (passed_source: the demux socket takes the roam), then the session moves to it.
    after="$(metrics)"
    dp=$(($(metric "$after" 'wg_demux_fastpath_packets_total{result="passed_source"}') - $(metric "$before" 'wg_demux_fastpath_packets_total{result="passed_source"}')))
    ds=$(($(metric "$after" "$TO_SERVER") - $(metric "$before" "$TO_SERVER")))
    ((dp > 0)) || fail "client $g: the roam never reached the demux socket (passed_source +0)"
    ((ds >= 10)) || fail "client $g: the fast path forwarded only $ds packets after the roam"
    roam=", passed_source +$dp, forwarded_to_server +$ds"
  fi
  # The server's peer is the demux: it sees the source the demux socket uses towards it (in tc and xdp the kernel's preferred source
  # on the entry node, which the fast path uses too; in userspace the address of the demux pod that serves the client, on any node with the
  # policy Cluster), never the client's address.
  if fast; then
    srcs="$(docker exec "$ENTRY_NODE" ip -4 route get "$(k -n "lg-demux-$g" get pod vpn -o jsonpath='{.status.podIP}')" |
      sed -n 's/.* src \([0-9.]*\).*/\1/p')"
  else
    srcs="$(demux_ips)"
  fi
  endpoint="$(k -n "lg-demux-$g" exec vpn -- wg show wg0 endpoints | awk '{print $2}')"
  [[ " $srcs " == *" ${endpoint%:51820} "* && "$endpoint" == *:51820 ]] || fail "server $g sees $endpoint, not a demux source ($srcs, port 51820)"
  [[ "$endpoint" != "$(client_ip "$g"):"* ]] || fail "server $g sees the client's address $endpoint"
  summary "client $g roam: tunnel after ${t}s, ping 20/20$roam, server endpoint $endpoint"
done

# ct_expiry <addr:port>: the expiry of each of Cilium's conntrack entries from that source on the entry node.
ct_expiry() {
  { cilium "$ENTRY_NODE" cilium-dbg bpf ct list global | grep -F " $1 " || true; } | sed -n 's/.* expires=\([0-9]*\).*/\1/p' | sort |
    paste -sd, -
}
if fast; then
  for g in a b; do
    log "client $g: Cilium NAT and conntrack"
    cip="$(client_ip "$g")"
    re="${cip//./\\.}"
    nat="$(cilium "$ENTRY_NODE" cilium-dbg bpf nat list | grep -F "$cip:" || true)"
    [[ -z "$nat" ]] || fail "Cilium holds NAT entries for client $g: $nat"
    ct="$(cilium "$ENTRY_NODE" cilium-dbg bpf ct list global | grep -F "$cip:" || true)"
    # The handshakes pass through Cilium's node port (the service lookup and the flow to the demux socket); nothing else may be there.
    other="$(grep -Ev "^UDP (SVC|OUT) $re:[0-9]+ -> ${ENTRY_IP//./\\.}:($NODE_PORT|51820) " <<<"$ct" || true)"
    [[ -z "$other" ]] || fail "Cilium conntrack holds more than the handshakes of client $g: $other"
    # The transport packets of the current tunnel do not touch its conntrack entries (they would push their expiry on).
    port="$(client "$g" wg show wg0 listen-port)"
    for try in 1 2; do
      hs="$(client "$g" wg show wg0 latest-handshakes | awk '{print $2}')"
      e1="$(ct_expiry "$cip:$port")"
      n="$(received "$g" -c 20 -i 0.2 -W 1 "$(server_of "$g")")"
      e2="$(ct_expiry "$cip:$port")"
      [[ "$e1" == "$e2" ]] && break
      # A rekey in the window passes through Cilium and moves the expiry: measure again.
      [[ "$try" == 1 && "$(client "$g" wg show wg0 latest-handshakes | awk '{print $2}')" != "$hs" ]] ||
        fail "client $g: 20 transport pings ($n received) moved Cilium's conntrack expiry ($e1 -> $e2)"
    done
    summary "client $g Cilium state: NAT 0, conntrack $(grep -c . <<<"$ct") handshake entries, untouched by 20 transport pings"
  done
fi
echo_ok end

if [[ "$MODE" == upgrade ]]; then
  # The next steps apply the demux without Helm.
  helm_k -n "$RELEASE_NS" uninstall laboratory --wait >/dev/null
fi
printf '\nPASS (mode %s)\n' "$MODE"
