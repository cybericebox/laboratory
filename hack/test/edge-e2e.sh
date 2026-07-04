#!/usr/bin/env bash
# End-to-end validation of the merged laboratory edge (single unprivileged
# laboratory-proxy Deployment + shared LB-IPAM external IP for VPN + Gateway)
# on a live k0s cluster (see hack/k0s/STEPS.md for cluster bring-up).
#
# Usage (run from repo root, cluster already up per hack/k0s/STEPS.md):
#   export KUBECONFIG=$PWD/.k0s/kubeconfig.yaml
#   ./hack/test/edge-e2e.sh all
#   ./hack/test/edge-e2e.sh <step1|step2|...|step7|cleanup>
#
# Steps:
#   step1  build + import the laboratory-proxy (and laboratory-agent) images
#   step2  helm upgrade with the merged edge topology
#   step3  confirm the WG LoadBalancer Service and the Gateway share one IP
#   step4  VPN path through the shared IP (WireGuard handshake + intra-lab ping)
#   step5  web device (L7 + JWT cookie) through the Gateway
#   step6  agent reachable through the Gateway's TLSRoute (mTLS allow/deny)
#   step7  lab internet-gateway egress (TCP curl, never ping — see note below)
#
# ---------------------------------------------------------------------------
# KNOWN LIVE-CLUSTER CAVEATS (found during the first live run of this script;
# see .superpowers/sdd/task-4-report.md for full evidence). This script works
# around them; they are NOT fixed in the chart/cluster-setup source because
# fixing them is out of this script's scope:
#
#  1. `helm upgrade --reuse-values` against a release installed with the
#     OLD (pre-redesign) values layout can wipe chart defaults for keys not
#     present in the stored release config (e.g. proxy.image) — Helm's
#     --reuse-values replaces the chart's default values map with the old
#     release's raw config before merging, so anything only in the NEW
#     chart's values.yaml can disappear. First upgrade after this redesign
#     must use --reset-values with explicit --set for every previously
#     customized key (see step2). Subsequent upgrades may use --reuse-values.
#  2. `proxy.networkPolicy.enabled` (default true): the generated
#     NetworkPolicy only allows ingress on the L7 TCP port. Since the l7 and
#     wg-demux containers now share one pod/label (app=laboratory-proxy-l7),
#     the same policy object also governs wg-demux, but has no UDP 51820
#     ingress rule — this silently drops all WireGuard traffic. Disabled
#     here (--set proxy.networkPolicy.enabled=false) until the chart's
#     netpol is extended with a WG ingress rule.
#  3. `agent.networkPolicy.enabled` (default true): the generated
#     NetworkPolicy's ingress selector (kube-system/k8s-app=cilium) does not
#     match how Cilium's embedded Envoy actually sources the TLSRoute
#     passthrough connection to the agent pod — it silently drops it.
#     Disabled here for the same reason as (2).
#  4. Cilium LB-IPAM sharing-key annotations must be `lbipam.cilium.io/
#     sharing-key` + `lbipam.cilium.io/sharing-cross-namespace` (NOT
#     `io.cilium/lb-ipam-sharing-key`, which Cilium 1.19 does not recognize
#     for sharing). Also: sharing an IP with `externalTrafficPolicy: Local`
#     requires BOTH services to select the same pods (Cilium's isCompatible
#     check) — Cilium's own Gateway-generated Service has no selector at
#     all, so it can only share with a peer using `externalTrafficPolicy:
#     Cluster`. `charts/laboratory/templates/proxy/service-wg-lb.yaml` and
#     `.../gateway/gateway.yaml` were patched accordingly (uncommitted —
#     see task-4-report.md for the exact diffs).
#  5. Cluster setup: `hack/k0s/STEPS.md` / `1-vms.sh`'s
#     CiliumL2AnnouncementPolicy only matches `^en.*`/`^eth.*` interfaces,
#     but the Lima shared-vmnet interface where the LB pool CIDR
#     (192.168.105.240/29) actually lives is named `lima0`. Without `^lima.*`
#     in that list, Cilium ARP-announces the shared IP on the wrong
#     interface (eth0, Lima's private NAT network) and it is unreachable
#     from anywhere. This script patches the live CiliumL2AnnouncementPolicy
#     (step2); the cluster-setup scripts should also be fixed for future
#     `k0sctl apply` runs (not done here — out of this script's scope).
#  6. Do not run the WireGuard test client (or any "external" reachability
#     probe) directly inside a Lima VM that is itself a cluster node
#     (lima-lab-ctrl / lima-lab-worker) — Cilium's own host datapath
#     special-cases node-originated traffic to a Service's external/LB IP
#     and does not forward it to a backend pod on a different node, which
#     looks exactly like a dropped packet from the outside. Use a plain pod
#     (step4 does this) or the actual Mac host as the client.
# ---------------------------------------------------------------------------

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

KUBECTL="${KUBECTL:-kubectl}"
HELM="${HELM:-helm}"
NAMESPACE_SYSTEM="${NAMESPACE_SYSTEM:-laboratory-system}"
NAMESPACE_PROXY="${NAMESPACE_PROXY:-laboratory-proxy}"
NAMESPACE_AGENT="${NAMESPACE_AGENT:-laboratory-agent}"
RELEASE="${RELEASE:-laboratory}"
CHART_PATH="${CHART_PATH:-charts/laboratory}"
LAB_NODES="${LAB_NODES:-lab-ctrl lab-worker}"
GROUP_NAME="${GROUP_NAME:-team-alpha}"
BASE_DOMAIN="${BASE_DOMAIN:-lab.test}"
AGENT_DOMAIN="${AGENT_DOMAIN:-agent.lab.test}"
SCRATCH_DIR="${SCRATCH_DIR:-$(mktemp -d /tmp/edge-e2e.XXXXXX)}"
TIMEOUT="${TIMEOUT:-120}"

die() { echo "ERROR: $*" >&2; exit 1; }
info() { echo "--- $* ---"; }

require_kubeconfig() {
    [[ -n "${KUBECONFIG:-}" ]] || die "export KUBECONFIG=\$PWD/.k0s/kubeconfig.yaml first"
    $KUBECTL get nodes >/dev/null || die "cannot reach cluster with current KUBECONFIG"
}

shared_ip() {
    $KUBECTL get svc laboratory-proxy-wg -n "$NAMESPACE_PROXY" \
        -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
}

###############################################################################
# Step 1: build + import proxy (and agent) images on both nodes
###############################################################################
step1() {
    info "Step 1: build + import laboratory-proxy image"
    docker build -q -t cybericebox/laboratory-proxy:latest -f Dockerfile.proxy .
    for n in $LAB_NODES; do
        echo "  importing into $n..."
        docker save cybericebox/laboratory-proxy:latest | \
            limactl shell "$n" -- sudo k0s ctr --namespace k8s.io images import -
    done

    if [[ "${WITH_AGENT:-1}" == "1" ]]; then
        info "Step 1b: build + import laboratory-agent image (needed for step6)"
        docker build -q -t cybericebox/laboratory-agent:latest -f Dockerfile.agent .
        for n in $LAB_NODES; do
            echo "  importing into $n..."
            docker save cybericebox/laboratory-agent:latest | \
                limactl shell "$n" -- sudo k0s ctr --namespace k8s.io images import -
        done
    fi
}

###############################################################################
# Step 2: helm upgrade with the merged edge topology
###############################################################################
step2() {
    info "Step 2: helm upgrade laboratory with the merged edge"

    # Fresh JWT keypair for platform.jwtPublicKey (regenerate if lost — the
    # private key never lives in-cluster; keep it if you need to mint test
    # JWTs against a value already deployed via `helm get values`).
    mkdir -p "$SCRATCH_DIR"
    if [[ ! -f "$SCRATCH_DIR/jwt-private.pem" ]]; then
        openssl genrsa -out "$SCRATCH_DIR/jwt-private.pem" 2048 2>/dev/null
        openssl rsa -in "$SCRATCH_DIR/jwt-private.pem" -pubout -out "$SCRATCH_DIR/jwt-public-key.pem" 2>/dev/null
    fi

    # See caveat (1) above: use --reset-values + explicit --set for the
    # first upgrade off the old values layout. If you know this release was
    # already installed from the NEW chart layout, --reuse-values is fine.
    $HELM upgrade "$RELEASE" "$CHART_PATH" -n "$NAMESPACE_SYSTEM" --reset-values \
        --set certManager.selfSigned=true \
        --set nodeAgent.excludeControlPlane=false \
        --set operator.baseDomain="$BASE_DOMAIN" \
        --set operator.publicVPNEndpoint="$(shared_ip 2>/dev/null || echo 0.0.0.0):51820" \
        --set-file platform.jwtPublicKey="$SCRATCH_DIR/jwt-public-key.pem" \
        --set proxy.enabled=true --set gateway.enabled=true \
        --set proxy.networkPolicy.enabled=false \
        --set agent.enabled="${WITH_AGENT:-1}" --set agent.domain="$AGENT_DOMAIN" \
        --set agent.networkPolicy.enabled=false \
        --wait --timeout="${TIMEOUT}s"

    # GatewayClass didn't exist yet on a fresh cluster (gateway.createGatewayClass
    # defaults false, assuming the class pre-exists) — create it, then re-point
    # operator.publicVPNEndpoint at the now-known shared IP.
    $HELM upgrade "$RELEASE" "$CHART_PATH" -n "$NAMESPACE_SYSTEM" --reuse-values \
        --set gateway.createGatewayClass=true \
        --wait --timeout="${TIMEOUT}s"

    # Caveat (5): patch the live L2 announcement policy to also announce on
    # the Lima shared-vmnet interface, not just eth0/en*.
    if $KUBECTL get ciliuml2announcementpolicy default >/dev/null 2>&1; then
        $KUBECTL patch ciliuml2announcementpolicy default --type merge \
            -p '{"spec":{"interfaces":["^en.*","^eth.*","^lima.*"]}}'
    fi

    $KUBECTL rollout status deploy/laboratory-proxy -n "$NAMESPACE_PROXY" --timeout="${TIMEOUT}s"

    echo "  Expect: one laboratory-proxy Deployment (2/2 containers), no DaemonSet, no hostNetwork:"
    $KUBECTL get deploy,ds -n "$NAMESPACE_PROXY"
    [[ -z "$($KUBECTL get ds -n "$NAMESPACE_PROXY" -o name)" ]] || die "DaemonSet still present in $NAMESPACE_PROXY"
    [[ "$($KUBECTL get pod -n "$NAMESPACE_PROXY" -o jsonpath='{.items[0].spec.hostNetwork}')" != "true" ]] || die "proxy pod still hostNetwork"

    # Re-set the real shared IP into publicVPNEndpoint now that it's stable.
    $HELM upgrade "$RELEASE" "$CHART_PATH" -n "$NAMESPACE_SYSTEM" --reuse-values \
        --set operator.publicVPNEndpoint="$(shared_ip):51820" \
        --wait --timeout="${TIMEOUT}s"
}

###############################################################################
# Step 3: confirm the single shared external IP
###############################################################################
step3() {
    info "Step 3: confirm shared external IP (WG service == Gateway)"
    local wg_ip gw_ip
    wg_ip="$(shared_ip)"
    gw_ip="$($KUBECTL get gateway laboratory-gateway -n "$NAMESPACE_SYSTEM" -o jsonpath='{.status.addresses[0].value}')"
    echo "  laboratory-proxy-wg external IP: $wg_ip"
    echo "  laboratory-gateway address:      $gw_ip"
    [[ -n "$wg_ip" && "$wg_ip" == "$gw_ip" ]] || die "IPs differ (wg=$wg_ip gw=$gw_ip) — LB-IPAM sharing-key not working, see caveat (4)"
    echo "PASS: single shared external IP = $wg_ip"
}

###############################################################################
# Step 4: VPN path through the shared IP
###############################################################################
step4() {
    info "Step 4: VPN path through the shared IP"
    local ip="$(shared_ip)"

    $KUBECTL apply -f hack/test/fixtures/labgroup.yaml
    $KUBECTL wait labgroup "$GROUP_NAME" --for=jsonpath='{.status.vpn.registered}'=true --timeout="${TIMEOUT}s"
    local lab_ns="$GROUP_NAME"

    $KUBECTL get labgroupclient tester -n "$lab_ns" >/dev/null 2>&1 || \
        LAB_NS="$lab_ns" envsubst < hack/test/fixtures/vpn-client.yaml | $KUBECTL apply -f -

    local secret deadline
    deadline=$(( $(date +%s) + TIMEOUT ))
    while true; do
        secret=$($KUBECTL get labgroupclient tester -n "$lab_ns" -o jsonpath='{.status.secretRef}' 2>/dev/null || true)
        [[ -n "$secret" ]] && break
        [[ $(date +%s) -lt $deadline ]] || die "LabGroupClient tester never got a secretRef"
        sleep 2
    done
    $KUBECTL get secret "$secret" -n "$lab_ns" -o jsonpath='{.data.wg\.conf}' | base64 -d > "$SCRATCH_DIR/wg-tester.conf"
    sed -i.bak "s/Endpoint = .*/Endpoint = ${ip}:51820/" "$SCRATCH_DIR/wg-tester.conf"

    # IMPORTANT (caveat 6): run the WireGuard client from a plain pod, NOT
    # from lima-lab-ctrl/lima-lab-worker themselves.
    $KUBECTL run wg-test-client --image=cybericebox/laboratory-lab:latest \
        --overrides='{"spec":{"containers":[{"name":"wg-test-client","image":"cybericebox/laboratory-lab:latest","imagePullPolicy":"IfNotPresent","command":["sleep","3600"],"securityContext":{"capabilities":{"add":["NET_ADMIN"]}}}]}}' \
        --restart=Never -n default 2>/dev/null || true
    $KUBECTL wait pod wg-test-client -n default --for=condition=Ready --timeout="${TIMEOUT}s"
    $KUBECTL exec wg-test-client -n default -- sh -c 'command -v wg-quick >/dev/null || (apt-get update -qq && apt-get install -y -qq wireguard-tools iputils-ping curl)' >/dev/null

    $KUBECTL cp "$SCRATCH_DIR/wg-tester.conf" default/wg-test-client:/tmp/wg-tester.conf
    $KUBECTL exec wg-test-client -n default -- wg-quick down /tmp/wg-tester.conf 2>/dev/null || true
    $KUBECTL exec wg-test-client -n default -- wg-quick up /tmp/wg-tester.conf
    sleep 2

    echo "  Ping the lab's VPN gateway (10.128.<N>.1) — set VPN_GW_IP if not the first lab:"
    local vpn_gw_ip="${VPN_GW_IP:-10.128.1.1}"
    $KUBECTL exec wg-test-client -n default -- ping -c4 -W2 "$vpn_gw_ip"

    echo "  wg show:"
    $KUBECTL exec wg-test-client -n default -- wg show
    $KUBECTL exec wg-test-client -n default -- wg show | grep -q "latest handshake" || die "no WireGuard handshake"
    echo "PASS: WireGuard handshake completed through shared IP $ip:51820"

    $KUBECTL exec wg-test-client -n default -- wg-quick down /tmp/wg-tester.conf
}

###############################################################################
# Step 5: web device (L7 + JWT) through the Gateway
###############################################################################
step5() {
    info "Step 5: web device (L7 + JWT) through the Gateway"
    local ip="$(shared_ip)"
    local lab_ns="$GROUP_NAME"
    local device="e2ewebapp"

    if ! $KUBECTL get lab ctf-web -n "$lab_ns" >/dev/null 2>&1; then
        cat <<EOF | $KUBECTL apply -f -
apiVersion: laboratory.cybericebox.com/v1alpha1
kind: Lab
metadata:
  name: ctf-web
  namespace: $lab_ns
spec:
  devices:
    - name: $device
      type: container
      image: nginx:alpine
      interfaces:
        - name: eth1
          addr:
            type: dhcp
      exposure:
        web:
          port: 80
          protocol: http
EOF
    fi
    $KUBECTL wait lab ctf-web -n "$lab_ns" --for=jsonpath='{.status.phase}'=Ready --timeout="${TIMEOUT}s"

    # Mint an RS256 JWT for group_id=$GROUP_NAME using the private key from step2.
    [[ -f "$SCRATCH_DIR/jwt-private.pem" ]] || die "run step2 first (need the matching JWT private key)"
    local token
    token=$(python3 - "$SCRATCH_DIR/jwt-private.pem" "$GROUP_NAME" <<'PYEOF'
import base64, json, subprocess, sys, time
def b64url(b): return base64.urlsafe_b64encode(b).rstrip(b"=").decode()
priv, group = sys.argv[1], sys.argv[2]
header = {"alg": "RS256", "typ": "JWT"}
now = int(time.time())
payload = {"user_id": "e2e-tester", "group_id": group, "iat": now, "exp": now + 3600}
signing_input = b64url(json.dumps(header, separators=(",", ":")).encode()) + "." + \
                b64url(json.dumps(payload, separators=(",", ":")).encode())
sig = subprocess.run(["openssl", "dgst", "-sha256", "-sign", priv], input=signing_input.encode(),
                      stdout=subprocess.PIPE, check=True).stdout
print(signing_input + "." + b64url(sig))
PYEOF
)

    echo "  without cookie (expect 401):"
    local code_401
    code_401=$(curl -sk -m8 --resolve "${device}.${BASE_DOMAIN}:443:${ip}" \
        "https://${device}.${BASE_DOMAIN}/" -o /dev/null -w '%{http_code}')
    echo "    -> $code_401"
    [[ "$code_401" == "401" ]] || die "expected 401 without cookie, got $code_401"

    echo "  with valid JWT cookie (expect 200):"
    local code_200
    code_200=$(curl -sk -m8 --resolve "${device}.${BASE_DOMAIN}:443:${ip}" \
        "https://${device}.${BASE_DOMAIN}/" -H "Cookie: challenge=$token" -o /dev/null -w '%{http_code}')
    echo "    -> $code_200"
    [[ "$code_200" == "200" ]] || die "expected 200 with valid cookie, got $code_200"

    echo "PASS: L7 + JWT gate working through the Gateway (401 -> 200)"
}

###############################################################################
# Step 6: agent reachable through the Gateway's TLSRoute (mTLS)
###############################################################################
step6() {
    info "Step 6: agent through the Gateway (TLSRoute + mTLS)"
    local ip="$(shared_ip)"

    $KUBECTL get secret laboratory-agent-client-platform-tls -n "$NAMESPACE_AGENT" \
        -o jsonpath='{.data.tls\.crt}' | base64 -d > "$SCRATCH_DIR/agent-client.crt"
    $KUBECTL get secret laboratory-agent-client-platform-tls -n "$NAMESPACE_AGENT" \
        -o jsonpath='{.data.tls\.key}' | base64 -d > "$SCRATCH_DIR/agent-client.key"
    $KUBECTL get secret laboratory-agent-ca -n "$NAMESPACE_AGENT" \
        -o jsonpath='{.data.tls\.crt}' | base64 -d > "$SCRATCH_DIR/agent-ca.crt"

    echo "  without client cert, sending app data (expect TLS 'certificate required' alert):"
    local without_cert_out
    without_cert_out=$( (printf 'PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n' | \
        openssl s_client -connect "${ip}:443" -servername "$AGENT_DOMAIN" -quiet 2>&1 &
        pid=$!; sleep 5; kill "$pid" 2>/dev/null || true) )
    echo "$without_cert_out" | grep -q "certificate required" || \
        die "expected a 'certificate required' TLS alert without a client cert"
    echo "  PASS: rejected without client cert"

    echo "  with valid client cert (CN=platform), same probe (expect no rejection):"
    local with_cert_out
    with_cert_out=$( (printf 'PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n' | \
        openssl s_client -connect "${ip}:443" -servername "$AGENT_DOMAIN" \
        -cert "$SCRATCH_DIR/agent-client.crt" -key "$SCRATCH_DIR/agent-client.key" \
        -CAfile "$SCRATCH_DIR/agent-ca.crt" -quiet 2>&1 &
        pid=$!; sleep 5; kill "$pid" 2>/dev/null || true) )
    echo "$with_cert_out" | grep -q "certificate required" && \
        die "valid client cert (CN in allowlist) was still rejected"
    echo "  PASS: accepted with a valid, allow-listed client cert"
}

###############################################################################
# Step 7: lab internet-gateway egress — TCP, never ping
###############################################################################
step7() {
    info "Step 7: lab internet-gateway egress (TCP curl, not ping — Lima vmnet drops outbound ICMP)"
    local lab_ns="$GROUP_NAME"
    local pod device

    pod=$($KUBECTL get pods -n "$lab_ns" -l "laboratory.cybericebox.com/device" \
        -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.labels.laboratory\.cybericebox\.com/device}{"\n"}{end}' \
        | awk '/gw-client/{print $1; exit}')
    [[ -n "$pod" ]] || die "no gw-client-* device pod found in namespace $lab_ns (need a lab with an internet-gateway domain, e.g. hack/test/fixtures/lab-dhcp-modes.yaml)"

    echo "  curl https://1.1.1.1 from $pod (expect 301):"
    local code
    code=$($KUBECTL exec -n "$lab_ns" "$pod" -- curl -sS -m8 -o /dev/null -w '%{http_code}' https://1.1.1.1)
    echo "    -> $code"
    [[ "$code" == "301" ]] || die "expected 301, got $code"
    echo "PASS: internet egress via the lab gateway works (TCP)"
}

###############################################################################
# Cleanup
###############################################################################
cleanup() {
    info "Cleanup"
    $KUBECTL delete pod wg-test-client -n default --ignore-not-found
    $KUBECTL delete lab ctf-web -n "$GROUP_NAME" --ignore-not-found
    $KUBECTL delete labgroupclient tester -n "$GROUP_NAME" --ignore-not-found
    rm -rf "$SCRATCH_DIR"
    echo "Note: LabGroup $GROUP_NAME and the helm release are left in place (shared with other manual testing)."
}

###############################################################################
# Main
###############################################################################
main() {
    require_kubeconfig
    case "${1:-}" in
        step1) step1 ;;
        step2) step2 ;;
        step3) step3 ;;
        step4) step4 ;;
        step5) step5 ;;
        step6) step6 ;;
        step7) step7 ;;
        cleanup) cleanup ;;
        all)
            step1; step2; step3; step4; step5; step6; step7
            echo ""
            echo "ALL STEPS PASSED. Scratch dir (JWT keys, wg configs): $SCRATCH_DIR"
            ;;
        *)
            echo "Usage: $0 <step1|step2|step3|step4|step5|step6|step7|all|cleanup>"
            exit 1
            ;;
    esac
}

main "$@"
