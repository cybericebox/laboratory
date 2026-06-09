# Dev cluster — step-by-step

Variables filled by `./1-vms.sh` and saved to `.k0s/commands.md`.

```
CTRL_IP        — controller VM IP (Lima VZ network)
WORKER_IP      — worker VM IP
LB_POOL_CIDR   — last /29 of Lima subnet, e.g. 192.168.105.240/29
K0SCTL_CFG     — .k0s/k0sctl.yaml
KUBECONFIG     — .k0s/kubeconfig.yaml
CHART_PATH     — charts/laboratory
```

---

## Step 1 — Lima VMs

```bash
./hack/k0s/1-vms.sh
```

Creates `lab-ctrl` (4CPU/6GiB) and `lab-worker` (4CPU/4GiB) Lima VMs.
Installs: OVS, WireGuard, Helm, kubectl.
Generates `.k0s/k0sctl.yaml` and `.k0s/commands.md` with real IPs.

---

## Step 2 — k0s cluster

Cilium and cert-manager are embedded in `k0sctl.yaml` via `spec.extensions.helm`
and install automatically during `k0sctl apply`.

```bash
k0sctl apply --config $K0SCTL_CFG

k0sctl kubeconfig --config $K0SCTL_CFG > $KUBECONFIG
export KUBECONFIG=$KUBECONFIG

kubectl wait --for=condition=Ready node --all --timeout=300s
kubectl get nodes -o wide
kubectl get pods -n kube-system
kubectl get pods -n cert-manager
```

---

## Step 3 — Gateway API CRDs + LB pool + laboratory chart

Gateway API experimental CRDs (TLSRoute is not in stable channel):

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/experimental-install.yaml
```

Cilium LB IP pool + L2 announcement policy:

```bash
kubectl apply -f - <<YAML
apiVersion: cilium.io/v2alpha1
kind: CiliumLoadBalancerIPPool
metadata:
  name: lab-pool
spec:
  blocks:
    - cidr: "$LB_POOL_CIDR"
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
```

Laboratory chart (cert-manager already installed by k0s → `install=false`):

```bash
helm repo add jetstack https://charts.jetstack.io
helm dependency update $CHART_PATH

helm upgrade --install laboratory $CHART_PATH \
  --namespace laboratory --create-namespace \
  --set operator.baseDomain=lab.test \
  --set operator.publicVPNEndpoint=$CTRL_IP:51820 \
  --set certManager.install=false \
  --set certManager.staging=true \
  --set certManager.email=test@lab.test \
  --wait --timeout=3m
```

> To install cert-manager via the chart instead of k0s:
> remove it from `k0sctl.yaml` extensions and pass `--set certManager.install=true`.

---

## Step 4 — verify

```bash
kubectl get nodes -o wide
kubectl get pods -n kube-system -l app.kubernetes.io/name=cilium
kubectl get pods -n laboratory
kubectl get svc -A

# sharing-key: both services must show the same EXTERNAL-IP
kubectl get svc -n laboratory      laboratory-gateway
kubectl get svc -n laboratory-proxy laboratory-proxy-wg
```

---

## Teardown

```bash
limactl delete -f lab-ctrl
limactl delete -f lab-worker
rm -rf .k0s/
```
