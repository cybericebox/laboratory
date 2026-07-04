#!/usr/bin/env bash
# Step 1: Create Lima VMs and generate k0sctl.yaml with embedded Helm charts.
# Run: ./1-vms.sh
set -euo pipefail
cd "$(dirname "$0")"

K0S_VERSION="v1.35.4+k0s.0"
CILIUM_VERSION="1.19.4"
CERT_MANAGER_VERSION="v1.20.2"
GATEWAY_API_VERSION="v1.5.1"
SSH_USER="$(id -un)"
SHARE_DIR="$HOME/Projects/My/CyberICEBox/laboratory/.k0s"
CHART_PATH="$HOME/Projects/My/CyberICEBox/laboratory/charts/laboratory"

if ! command -v k0sctl &>/dev/null; then
  echo "→ installing k0sctl"
  brew install k0sproject/tap/k0sctl
fi

# ── start VMs ─────────────────────────────────────────────────────────────────
for vm in lab-ctrl lab-worker; do
  template="lima-ctrl.yaml"
  [[ "$vm" == "lab-worker" ]] && template="lima-worker.yaml"

  if limactl list "$vm" --format '{{.Status}}' 2>/dev/null | grep -q Running; then
    echo "→ $vm already running"
  else
    echo "→ starting $vm (provision ~3 min)"
    limactl start --tty=false --timeout 20m --name="$vm" "$template"
  fi
done

# ── discover IPs and ports ─────────────────────────────────────────────────────
# Skip 192.168.5.x (Lima usernet) — use shared network IP (192.168.105.x)
get_ip()       { limactl shell "$1" -- ip -4 addr show scope global 2>/dev/null | awk '/inet / && !/192\.168\.5\./{gsub(/\/[0-9]+/, "", $2); print $2; exit}'; }
get_ssh_port() { limactl list "$1" --format '{{.SSHLocalPort}}'; }

CTRL_IP=$(get_ip lab-ctrl)
WORKER_IP=$(get_ip lab-worker)
CTRL_SSH_PORT=$(get_ssh_port lab-ctrl)
WORKER_SSH_PORT=$(get_ssh_port lab-worker)

# shared network is always 192.168.105.x; use last /29 for LB pool
LB_POOL_CIDR="192.168.105.240/29"

# ── pre-add SSH host keys ──────────────────────────────────────────────────────
ssh-keyscan -p "$CTRL_SSH_PORT"   -H 127.0.0.1 >> ~/.ssh/known_hosts 2>/dev/null || true
ssh-keyscan -p "$WORKER_SSH_PORT" -H 127.0.0.1 >> ~/.ssh/known_hosts 2>/dev/null || true

# ── write k0sctl.yaml ──────────────────────────────────────────────────────────
mkdir -p "$SHARE_DIR"
K0SCTL_CFG="${SHARE_DIR}/k0sctl.yaml"

cat > "$K0SCTL_CFG" <<EOF
apiVersion: k0sctl.k0sproject.io/v1beta1
kind: Cluster
metadata:
  name: laboratory
spec:
  hosts:
    - ssh:
        address: 127.0.0.1
        port: ${CTRL_SSH_PORT}
        user: ${SSH_USER}
        keyPath: ~/.lima/_config/user
      role: controller+worker
    - ssh:
        address: 127.0.0.1
        port: ${WORKER_SSH_PORT}
        user: ${SSH_USER}
        keyPath: ~/.lima/_config/user
      role: worker
  k0s:
    version: "${K0S_VERSION}"
    config:
      apiVersion: k0s.k0sproject.io/v1beta1
      kind: ClusterConfig
      metadata:
        name: laboratory
      spec:
        network:
          provider: custom
          kubeProxy:
            disabled: true
        api:
          address: ${CTRL_IP}
          sans:
            - ${CTRL_IP}
            - 127.0.0.1
        extensions:
          helm:
            repositories:
              - name: cilium
                url: https://helm.cilium.io/
              - name: jetstack
                url: https://charts.jetstack.io
            charts:
              - name: cert-manager
                chartname: jetstack/cert-manager
                version: "${CERT_MANAGER_VERSION}"
                namespace: cert-manager
                order: 1
                values: |
                  crds:
                    enabled: true
              - name: cilium
                chartname: cilium/cilium
                version: "${CILIUM_VERSION}"
                namespace: kube-system
                order: 2
                values: |
                  kubeProxyReplacement: true
                  k8sServiceHost: "${CTRL_IP}"
                  k8sServicePort: "6443"
                  # Do not let Cilium rename competing CNI conflists (.cilium_bak).
                  # cni-gate must stay the active CNI and delegate to cilium-cni.
                  cni:
                    exclusive: false
                  gatewayAPI:
                    enabled: true
                  l2announcements:
                    enabled: true
                  externalIPs:
                    enabled: true
                  l7Proxy: true
                  ipam:
                    mode: kubernetes
                  operator:
                    replicas: 1
EOF

# ── generate commands.md ───────────────────────────────────────────────────────
cat > "${SHARE_DIR}/commands.md" <<EOF
# Lab cluster — manual commands
# ctrl=${CTRL_IP}  worker=${WORKER_IP}  lb-pool=${LB_POOL_CIDR}


## Step 2 — deploy k0s (Cilium + cert-manager install automatically via k0s extensions)

k0sctl apply --config ${K0SCTL_CFG}

# get kubeconfig
k0sctl kubeconfig --config ${K0SCTL_CFG} > ${SHARE_DIR}/kubeconfig.yaml
export KUBECONFIG=${SHARE_DIR}/kubeconfig.yaml

# wait for nodes + Cilium + cert-manager
kubectl wait --for=condition=Ready node --all --timeout=300s
kubectl rollout status daemonset/cilium -n kube-system --timeout=120s
kubectl rollout status deployment/cilium-operator -n kube-system --timeout=120s
kubectl rollout status deployment/cert-manager -n cert-manager --timeout=120s


## Step 3 — Gateway API CRDs

# Must be installed BEFORE laboratory chart (Cilium picks them up automatically)
# --server-side avoids annotation-size limit on large CRD schemas
# --force-conflicts handles re-apply over existing state
kubectl apply --server-side --force-conflicts \\
  -f https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/experimental-install.yaml

# Restart Cilium operator so it starts watching Gateway API resources immediately
kubectl rollout restart deployment/cilium-operator -n kube-system
kubectl rollout status deployment/cilium-operator -n kube-system --timeout=60s


## Step 4 — LB pool + laboratory chart

# Cilium L2 LB pool (MetalLB-style, via socket_vmnet shared subnet)
kubectl apply -f - <<YAML
apiVersion: cilium.io/v2
kind: CiliumLoadBalancerIPPool
metadata:
  name: lab-pool
spec:
  blocks:
    - cidr: "${LB_POOL_CIDR}"
---
apiVersion: cilium.io/v2alpha1
kind: CiliumL2AnnouncementPolicy
metadata:
  name: default
spec:
  interfaces:
    - "^en.*"
    - "^eth.*"
    # Lima shared-vmnet interface — the LB pool CIDR lives here. Without it the
    # shared IP is ARP-announced on eth0 (Lima NAT) and unreachable externally.
    - "^lima.*"
  loadBalancerIPs: true
YAML

# laboratory chart
helm upgrade --install laboratory ${CHART_PATH} \\
  --namespace laboratory --create-namespace \\
  --set operator.baseDomain=lab.test \\
  --set operator.publicVPNEndpoint=${CTRL_IP}:51820 \\
  --set certManager.staging=true \\
  --set certManager.email=test@lab.test \\
  --wait --timeout=5m


## Step 5 — verify

kubectl get nodes -o wide
kubectl get pods -n kube-system -l app.kubernetes.io/name=cilium
kubectl get pods -n laboratory
kubectl get svc -A

# sharing-key check: both must show the same EXTERNAL-IP
kubectl get svc -n laboratory       laboratory-gateway
kubectl get svc -n laboratory-proxy laboratory-proxy-wg


## Rebuild cluster (keep VMs, reset k0s only)

k0sctl reset --config ${K0SCTL_CFG}
rm -f ${SHARE_DIR}/kubeconfig.yaml
# then re-run steps 2–5

EOF

# ── print ──────────────────────────────────────────────────────────────────────
echo ""
echo "=== VMs ready ==="
echo "  ctrl   ${CTRL_IP}   SSH :${CTRL_SSH_PORT}"
echo "  worker ${WORKER_IP}  SSH :${WORKER_SSH_PORT}"
echo "  LB pool ${LB_POOL_CIDR}"
echo ""
echo "k0sctl config : ${K0SCTL_CFG}"
echo "commands      : ${SHARE_DIR}/commands.md"
echo ""
cat "${SHARE_DIR}/commands.md"
