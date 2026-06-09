#!/usr/bin/env bash
# Step 1: Create Lima VMs and generate k0sctl.yaml with embedded Helm charts.
# Run: ./1-vms.sh
set -euo pipefail
cd "$(dirname "$0")"

K0S_VERSION="v1.32.5+k0s.0"
CILIUM_VERSION="1.17.0"
CERT_MANAGER_VERSION="v1.17.1"
GATEWAY_API_VERSION="v1.2.1"
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
    limactl start --name="$vm" "$template"
  fi
done

# ── discover IPs and ports ─────────────────────────────────────────────────────
get_ip()       { limactl shell "$1" -- ip route get 8.8.8.8 2>/dev/null | awk '/src/{print $7; exit}'; }
get_ssh_port() { limactl list "$1" --format '{{.SSHLocalPort}}'; }

CTRL_IP=$(get_ip lab-ctrl)
WORKER_IP=$(get_ip lab-worker)
CTRL_SSH_PORT=$(get_ssh_port lab-ctrl)
WORKER_SSH_PORT=$(get_ssh_port lab-worker)

LIMA_GW=$(limactl shell lab-ctrl -- ip route show default | awk '/default/{print $3}')
LB_BASE=$(echo "$LIMA_GW" | sed 's/\.[0-9]*$//')
LB_POOL_CIDR="${LB_BASE}.240/29"

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
                  gatewayAPI:
                    enabled: true
                  l2announcements:
                    enabled: true
                  externalIPs:
                    enabled: true
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

# wait for nodes, Cilium, cert-manager
kubectl wait --for=condition=Ready node --all --timeout=300s
kubectl get nodes -o wide
kubectl get pods -n kube-system
kubectl get pods -n cert-manager


## Step 3 — Gateway API CRDs + LB pool + laboratory chart

# Gateway API experimental CRDs (TLSRoute; not installable via k0s extensions)
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/experimental-install.yaml

# Cilium LB IP pool
kubectl apply -f - <<YAML
apiVersion: cilium.io/v2alpha1
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
  loadBalancerIPs: true
YAML

# resolve cert-manager subchart (needed once; fetches chart archive into charts/)
helm repo add jetstack https://charts.jetstack.io 2>/dev/null || true
helm dependency update ${CHART_PATH}

# laboratory chart
# certManager.install=false  → cert-manager already installed by k0s above
# certManager.install=true   → chart installs cert-manager itself (no k0s needed)
helm upgrade --install laboratory ${CHART_PATH} \\
  --namespace laboratory --create-namespace \\
  --set operator.baseDomain=lab.test \\
  --set operator.publicVPNEndpoint=${CTRL_IP}:51820 \\
  --set certManager.install=false \\
  --set certManager.staging=true \\
  --set certManager.email=test@lab.test \\
  --wait --timeout=3m


## Step 4 — verify

kubectl get nodes -o wide
kubectl get pods -n kube-system -l app.kubernetes.io/name=cilium
kubectl get pods -n laboratory
kubectl get svc -A

# sharing-key check: both must have the same EXTERNAL-IP
kubectl get svc -n laboratory     laboratory-gateway
kubectl get svc -n laboratory-proxy laboratory-proxy-wg
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
