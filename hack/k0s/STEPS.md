# Dev cluster — step-by-step

Variables filled by `./1-vms.sh` and saved to `.k0s/commands.md`.

```
CTRL_IP        — controller VM IP (Lima shared network, 192.168.105.x)
WORKER_IP      — worker VM IP
LB_POOL_CIDR   — 192.168.105.240/29 (last /29 of shared subnet)
K0SCTL_CFG     — .k0s/k0sctl.yaml
KUBECONFIG     — .k0s/kubeconfig.yaml
CHART_PATH     — charts/laboratory
```

---

## Step 1 — Lima VMs

```bash
./hack/k0s/1-vms.sh
```

Creates `lab-ctrl` (4CPU/6GiB) and `lab-worker` (4CPU/4GiB) Lima VMs via socket_vmnet (unique IPs).
Installs: OVS, WireGuard on each VM.
Generates `.k0s/k0sctl.yaml` and `.k0s/commands.md` with real IPs.

**Prerequisites on Mac host:**
```bash
brew install socket_vmnet helm kubectl k0sproject/tap/k0sctl
sudo mkdir -p /opt/socket_vmnet/bin
sudo cp $(brew --prefix)/opt/socket_vmnet/bin/socket_vmnet /opt/socket_vmnet/bin/socket_vmnet
sudo chown -R root:wheel /opt/socket_vmnet
limactl sudoers | sudo tee /private/etc/sudoers.d/lima
```

---

## Step 2 — k0s cluster

Cilium and cert-manager are embedded in `k0sctl.yaml` via `spec.extensions.helm`
and install automatically during `k0sctl apply`.

```bash
k0sctl apply --config $K0SCTL_CFG

k0sctl kubeconfig --config $K0SCTL_CFG > $KUBECONFIG
export KUBECONFIG=$KUBECONFIG

kubectl wait --for=condition=Ready node --all --timeout=300s
kubectl rollout status daemonset/cilium -n kube-system --timeout=120s
kubectl rollout status deployment/cilium-operator -n kube-system --timeout=120s
kubectl rollout status deployment/cert-manager -n cert-manager --timeout=120s
```

---

## Step 3 — Gateway API CRDs

Cilium uses Gateway API CRDs but does NOT install them.
Must be installed before the laboratory chart.
`--server-side` avoids annotation-size limit; `--force-conflicts` handles re-apply.

```bash
kubectl apply --server-side --force-conflicts \
  -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/experimental-install.yaml

# Restart Cilium operator to start watching Gateway API resources
kubectl rollout restart deployment/cilium-operator -n kube-system
kubectl rollout status deployment/cilium-operator -n kube-system --timeout=60s
```

---

## Step 4 — LB pool

Cilium LB IP pool + L2 announcement policy:

```bash
kubectl apply -f - <<YAML
apiVersion: cilium.io/v2
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

---

## Step 5 — Import images

`k0sctl reset` wipes containerd storage — re-import after every cluster reset.
Images must be built first: `make docker-build-all` in the laboratory repo root.

Images are imported into the `k8s.io` containerd namespace (the CRI namespace).

```bash
for img in \
  cybericebox/laboratory-controller:latest \
  cybericebox/laboratory-node-agent:latest \
  cybericebox/laboratory-lab:latest \
  cybericebox/laboratory-proxy:latest; do
  docker save "$img" | limactl shell lab-ctrl   -- sudo k0s ctr --namespace k8s.io images import -
  docker save "$img" | limactl shell lab-worker -- sudo k0s ctr --namespace k8s.io images import -
done
```

---

## Step 6 — laboratory chart

The chart creates `lab-platform-secret` automatically when `platform.jwtPublicKey` is passed.
The operator then syncs it to `proxy-credentials` in `laboratory-proxy`.
Without it `proxy-l7` pod will not start.

For local testing, generate a throwaway keypair:

```bash
openssl genrsa -out /tmp/jwt-private.pem 2048
openssl rsa -in /tmp/jwt-private.pem -pubout -out /tmp/jwt-public-key.pem
```

```bash
CTRL_IP=$(limactl shell lab-ctrl -- ip -4 addr show scope global 2>/dev/null \
  | awk '/inet / && !/192\.168\.5\./{gsub(/\/[0-9]+/,"",$2); print $2; exit}')

helm upgrade --install laboratory $CHART_PATH \
  --namespace laboratory-system --create-namespace \
  --set operator.baseDomain=lab.test \
  --set operator.publicVPNEndpoint=$CTRL_IP:51820 \
  --set proxy.wg.externalInterface=lima0 \
  --set certManager.selfSigned=true \
  --set-file platform.jwtPublicKey=/tmp/jwt-public-key.pem \
  --wait --timeout=5m
```

---

## Step 7 — verify

```bash
kubectl get nodes -o wide
kubectl get pods -n kube-system -l app.kubernetes.io/name=cilium
kubectl get pods -n laboratory-system
kubectl get pods -n laboratory-proxy
kubectl get svc -A

# sharing-key: both services must show the same EXTERNAL-IP
kubectl get svc -n laboratory-system laboratory-gateway
kubectl get svc -n laboratory-proxy  laboratory-proxy-wg
```

---

## Rebuild cluster (keep VMs, reset k0s only)

```bash
k0sctl reset --force --config $K0SCTL_CFG
rm -f $KUBECONFIG
# re-run steps 2–6
```

---

## Full teardown

```bash
limactl delete -f lab-ctrl lab-worker
rm -rf .k0s/
```
