#!/usr/bin/env bash
# Manual test runner for CyberICEBox laboratory.
# Usage:
#   ./hack/test/run.sh single-node   — scenario 1: two devices on one worker
#   ./hack/test/run.sh multi-node    — scenario 2: four devices on two workers
#   ./hack/test/run.sh vpn           — scenario 3: VPN client connectivity
#   ./hack/test/run.sh cleanup       — delete all test resources

set -euo pipefail

LABGROUP_NAME="team-alpha"
FIXTURE_DIR="$(dirname "$0")/fixtures"
KUBECTL="${KUBECTL:-kubectl}"
TIMEOUT=120

###############################################################################
# Helpers
###############################################################################

die() { echo "ERROR: $*" >&2; exit 1; }

wait_labgroup_ns() {
    echo "Waiting for LabGroup namespace (up to ${TIMEOUT}s)..." >&2
    local deadline=$(( $(date +%s) + TIMEOUT ))
    while true; do
        local ns
        ns=$($KUBECTL get labgroup "$LABGROUP_NAME" \
            -o jsonpath='{.status.namespace}' 2>/dev/null || true)
        if [[ -n "$ns" ]]; then
            echo "  namespace: $ns" >&2
            echo "$ns"
            return
        fi
        [[ $(date +%s) -lt $deadline ]] || die "LabGroup namespace not ready after ${TIMEOUT}s"
        sleep 3
    done
}

wait_lab_ready() {
    local ns="$1" lab="$2"
    echo "Waiting for Lab $lab to reach Ready phase (up to ${TIMEOUT}s)..."
    $KUBECTL wait lab "$lab" -n "$ns" \
        --for=jsonpath='{.status.phase}'=Ready \
        --timeout="${TIMEOUT}s"
}

wait_vpn_client_ip() {
    local ns="$1" name="$2"
    echo "Waiting for LabGroupClient $name to get assigned IP (up to ${TIMEOUT}s)..."
    local deadline=$(( $(date +%s) + TIMEOUT ))
    while true; do
        local ip
        ip=$($KUBECTL get labgroupclient "$name" -n "$ns" \
            -o jsonpath='{.status.assignedIP}' 2>/dev/null || true)
        if [[ -n "$ip" ]]; then
            echo "  assigned IP: $ip" >&2
            return
        fi
        [[ $(date +%s) -lt $deadline ]] || die "VPN client IP not assigned after ${TIMEOUT}s"
        sleep 3
    done
}

ensure_labgroup() {
    if ! $KUBECTL get labgroup "$LABGROUP_NAME" &>/dev/null; then
        echo "Creating LabGroup $LABGROUP_NAME..."
        $KUBECTL apply -f "$FIXTURE_DIR/labgroup.yaml"
    else
        echo "LabGroup $LABGROUP_NAME already exists."
    fi
}

###############################################################################
# Scenario 1 — single node
###############################################################################

scenario_single_node() {
    echo "=== Scenario 1: single-node ==="
    ensure_labgroup
    LAB_NS=$(wait_labgroup_ns)

    echo "Cordoning worker-1 to force pods onto worker-0..."
    WORKER1=$($KUBECTL get nodes --no-headers -o name | grep worker | tail -1)
    $KUBECTL cordon "$WORKER1"
    trap "$KUBECTL uncordon $WORKER1; echo 'worker-1 uncordoned'" EXIT

    echo "Applying Lab ctf-single in $LAB_NS..."
    LAB_NS="$LAB_NS" envsubst < "$FIXTURE_DIR/lab-single-node.yaml" | $KUBECTL apply -f -

    wait_lab_ready "$LAB_NS" "ctf-single"

    echo ""
    echo "--- Device placement ---"
    $KUBECTL get pods -n "$LAB_NS" -o wide

    echo ""
    echo "--- Verify: ping attacker → victim ---"
    ATTACKER_POD=$($KUBECTL get pods -n "$LAB_NS" -l "laboratory.cybericebox.com/device=attacker" \
        -o jsonpath='{.items[0].metadata.name}')
    VICTIM_IP=$($KUBECTL get lab ctf-single -n "$LAB_NS" \
        -o jsonpath='{.status.devices[?(@.name=="victim")].podIP}' 2>/dev/null)
    if [[ -z "$VICTIM_IP" ]]; then
        VICTIM_IP=$($KUBECTL get pod -n "$LAB_NS" -l "laboratory.cybericebox.com/device=victim" \
            -o jsonpath='{.items[0].status.podIP}')
    fi

    echo "  attacker pod: $ATTACKER_POD"
    echo "  victim IP:    $VICTIM_IP"
    $KUBECTL exec -n "$LAB_NS" "$ATTACKER_POD" -- ping -c3 "$VICTIM_IP" && \
        echo "PASS: connectivity OK" || echo "FAIL: no connectivity"

    $KUBECTL uncordon "$WORKER1"
    trap - EXIT
}

###############################################################################
# Scenario 2 — multi-node
###############################################################################

scenario_multi_node() {
    echo "=== Scenario 2: multi-node ==="
    ensure_labgroup
    LAB_NS=$(wait_labgroup_ns)

    WORKER_COUNT=$($KUBECTL get nodes --no-headers -o name | grep -c worker || true)
    [[ $WORKER_COUNT -ge 2 ]] || die "Need at least 2 worker nodes; found $WORKER_COUNT"

    echo "Applying Lab ctf-multi in $LAB_NS..."
    LAB_NS="$LAB_NS" envsubst < "$FIXTURE_DIR/lab-multi-node.yaml" | $KUBECTL apply -f -

    wait_lab_ready "$LAB_NS" "ctf-multi"

    echo ""
    echo "--- Device placement (expect pods on multiple nodes) ---"
    $KUBECTL get pods -n "$LAB_NS" -o wide

    echo ""
    echo "--- Node distribution ---"
    $KUBECTL get pods -n "$LAB_NS" -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.nodeName}{"\n"}{end}'

    NODES=$($KUBECTL get pods -n "$LAB_NS" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | sort -u | wc -l)
    echo ""
    [[ $NODES -ge 2 ]] && echo "PASS: pods on $NODES nodes (cross-node VXLAN active)" || \
        echo "INFO: pods on $NODES node(s) — increase workload or check scheduler"

    echo ""
    echo "--- Verify: client-a → server-b (crosses VXLAN) ---"
    CLIENT_POD=$($KUBECTL get pods -n "$LAB_NS" -l "laboratory.cybericebox.com/device=client-a" \
        -o jsonpath='{.items[0].metadata.name}')
    SERVER_IP=$($KUBECTL get pod -n "$LAB_NS" -l "laboratory.cybericebox.com/device=server-b" \
        -o jsonpath='{.items[0].status.podIP}')
    $KUBECTL exec -n "$LAB_NS" "$CLIENT_POD" -- ping -c3 "$SERVER_IP" && \
        echo "PASS: cross-node connectivity OK" || echo "FAIL: cross-node connectivity failed"
}

###############################################################################
# Scenario 3 — VPN
###############################################################################

scenario_vpn() {
    echo "=== Scenario 3: VPN client ==="
    ensure_labgroup
    LAB_NS=$(wait_labgroup_ns)

    echo "Creating LabGroupClient tester in $LAB_NS..."
    LAB_NS="$LAB_NS" envsubst < "$FIXTURE_DIR/vpn-client.yaml" | $KUBECTL apply -f -

    wait_vpn_client_ip "$LAB_NS" "tester"

    echo ""
    echo "--- WireGuard config (Secret client-tester) ---"
    SECRET=$($KUBECTL get labgroupclient tester -n "$LAB_NS" \
        -o jsonpath='{.status.secretRef}')
    echo "  Secret name: $SECRET"
    $KUBECTL get secret "$SECRET" -n "$LAB_NS" \
        -o jsonpath='{.data.wg\.conf}' | base64 -d

    echo ""
    echo "--- VPN endpoint ---"
    $KUBECTL get labgroup "$LABGROUP_NAME" -o jsonpath='{.status.vpn.endpoint}'
    echo ""
    echo ""
    echo "To connect manually:"
    echo "  kubectl get secret -n $LAB_NS $SECRET -o jsonpath='{.data.wg\\.conf}' | base64 -d > /tmp/wg0.conf"
    echo "  sudo wg-quick up /tmp/wg0.conf"
    echo "  ping <device-ip-from-lab>"
    echo "  sudo wg-quick down /tmp/wg0.conf"

    echo ""
    echo "--- Check stats after connecting ---"
    echo "  kubectl get labgroupclient tester -n $LAB_NS -o yaml"
}

###############################################################################
# Cleanup
###############################################################################

cleanup() {
    echo "=== Cleanup ==="

    if $KUBECTL get labgroup "$LABGROUP_NAME" &>/dev/null; then
        LAB_NS=$($KUBECTL get labgroup "$LABGROUP_NAME" \
            -o jsonpath='{.status.namespace}' 2>/dev/null || true)

        for lab in ctf-single ctf-multi; do
            $KUBECTL delete lab "$lab" -n "$LAB_NS" --ignore-not-found
        done
        $KUBECTL delete labgroupclient tester -n "$LAB_NS" --ignore-not-found
        $KUBECTL delete labgroup "$LABGROUP_NAME" --ignore-not-found
    fi

    echo "Done. Namespace labgroup-* will be garbage collected by the operator."
}

###############################################################################
# Main
###############################################################################

case "${1:-}" in
    single-node) scenario_single_node ;;
    multi-node)  scenario_multi_node ;;
    vpn)         scenario_vpn ;;
    cleanup)     cleanup ;;
    *)
        echo "Usage: $0 <single-node|multi-node|vpn|cleanup>"
        echo ""
        echo "Prerequisites:"
        echo "  make cluster-up       — create Kind cluster"
        echo "  make kind-deploy      — build images + deploy operator + node-agent"
        echo ""
        echo "Scenarios (run in order):"
        echo "  single-node  — 2 devices, forced onto 1 node, verify L2 ping"
        echo "  multi-node   — 4 devices, spread across 2 nodes, verify cross-VXLAN ping"
        echo "  vpn          — create WireGuard client, print config for manual connect"
        echo "  cleanup      — remove all test resources"
        exit 1
        ;;
esac
