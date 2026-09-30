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
    # Lima shared-vmnet interface — the LB pool CIDR lives here. Without it the
    # shared IP is ARP-announced on eth0 (Lima NAT) and unreachable externally.
    - "^lima.*"
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

The chart creates `lab-access-public-key` automatically when `platform.labAccessPublicKey` is passed.
The operator then syncs it to the same-named Secret in `laboratory-proxy`.
Without it `proxy-l7` pod will not start.

For local testing, generate a throwaway keypair:

```bash
make lab-access-keys   # /tmp/lab-access-private.pem (backend) and /tmp/lab-access-public.pem
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
  --set-file platform.labAccessPublicKey=/tmp/lab-access-public.pem \
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

## Step 8 — test scenario

Fixtures in `hack/test/fixtures/`. Run from repo root with `export KUBECONFIG=...`.

All labs run in the same LabGroup (`team-alpha`) but as separate Lab objects.

### 8.1 — Create LabGroup

Namespace = group name (deterministic: `team-alpha` → namespace `team-alpha`).

```bash
kubectl apply -f hack/test/fixtures/labgroup.yaml

kubectl wait labgroup team-alpha \
  --for=jsonpath='{.status.vpn.registered}'=true --timeout=120s

export LAB_NS=team-alpha
```

---

### 8.2 — Test 1: L2 direct (same node)

`attacker` ↔ `victim`, static IPs, single OVS p2p link.

```bash
LAB_NS=$LAB_NS envsubst < hack/test/fixtures/lab-test1-l2.yaml | kubectl apply -f -

kubectl wait lab ctf-test1 -n $LAB_NS \
  --for=jsonpath='{.status.phase}'=Ready --timeout=120s
```

Verify:

```bash
ATTACKER=$(kubectl get pods -n $LAB_NS \
  -l laboratory.cybericebox.com/lab=ctf-test1,laboratory.cybericebox.com/device=attacker \
  -o jsonpath='{.items[0].metadata.name}')

kubectl exec -n $LAB_NS $ATTACKER -- ping -c3 10.10.1.20
```

---

### 8.3 — Test 2: L2 cross-node (VXLAN)

`node-a` ↔ `node-b`, same spec as Test 1 but pods must land on different nodes.
With ctrl + worker cluster the scheduler spreads them — verify before pinging.

```bash
LAB_NS=$LAB_NS envsubst < hack/test/fixtures/lab-test2-crossnode.yaml | kubectl apply -f -

kubectl wait lab ctf-test2 -n $LAB_NS \
  --for=jsonpath='{.status.phase}'=Ready --timeout=120s

# Confirm different nodes
kubectl get pods -n $LAB_NS -l laboratory.cybericebox.com/lab=ctf-test2 -o wide
```

Verify cross-node connectivity:

```bash
NODE_A=$(kubectl get pods -n $LAB_NS \
  -l laboratory.cybericebox.com/lab=ctf-test2,laboratory.cybericebox.com/device=node-a \
  -o jsonpath='{.items[0].metadata.name}')

kubectl exec -n $LAB_NS $NODE_A -- ping -c3 10.10.2.20
```

---

### 8.4 — Test 3: unmanaged-switch

`host-a` and `host-b` connected via `sw1` (OVS bridge shared VNI).

```bash
LAB_NS=$LAB_NS envsubst < hack/test/fixtures/lab-test3-switch.yaml | kubectl apply -f -

kubectl wait lab ctf-test3 -n $LAB_NS \
  --for=jsonpath='{.status.phase}'=Ready --timeout=120s
```

Verify:

```bash
HOST_A=$(kubectl get pods -n $LAB_NS \
  -l laboratory.cybericebox.com/lab=ctf-test3,laboratory.cybericebox.com/device=host-a \
  -o jsonpath='{.items[0].metadata.name}')

kubectl exec -n $LAB_NS $HOST_A -- ping -c3 10.10.3.20
```

---

### 8.5 — Test 4: full (VPN + internet gateway + switch + DHCP)

Topology:

- **Domain 1**: `vpn` singleton ↔ `vpn-client` (VPN segment, DHCP)
- **Domain 2**: `internet` singleton → `sw1` → `gw-client-a`, `gw-client-b` (internet gateway, DHCP)

```bash
LAB_NS=$LAB_NS envsubst < hack/test/fixtures/lab-test4-full.yaml | kubectl apply -f -

kubectl wait lab ctf-test4 -n $LAB_NS \
  --for=jsonpath='{.status.phase}'=Ready --timeout=120s
```

Check DHCP-assigned addresses:

```bash
VPN_CLIENT=$(kubectl get pods -n $LAB_NS \
  -l laboratory.cybericebox.com/lab=ctf-test4,laboratory.cybericebox.com/device=vpn-client \
  -o jsonpath='{.items[0].metadata.name}')
GW_A=$(kubectl get pods -n $LAB_NS \
  -l laboratory.cybericebox.com/lab=ctf-test4,laboratory.cybericebox.com/device=gw-client-a \
  -o jsonpath='{.items[0].metadata.name}')
GW_B=$(kubectl get pods -n $LAB_NS \
  -l laboratory.cybericebox.com/lab=ctf-test4,laboratory.cybericebox.com/device=gw-client-b \
  -o jsonpath='{.items[0].metadata.name}')

kubectl exec -n $LAB_NS $VPN_CLIENT -- ip addr show eth1
kubectl exec -n $LAB_NS $GW_A      -- ip addr show eth1
kubectl exec -n $LAB_NS $GW_B      -- ip addr show eth1
```

Verify gateway-side L2 (gw-client-a → gw-client-b):

```bash
GW_B_IP=$(kubectl exec -n $LAB_NS $GW_B -- \
  ip -4 addr show eth1 | awk '/inet /{gsub(/\/[0-9]+/,"",$2); print $2}')

kubectl exec -n $LAB_NS $GW_A -- ping -c3 $GW_B_IP
```

#### 8.5.1 — Add VPN client and reach vpn-client device

```bash
LAB_NS=$LAB_NS envsubst < hack/test/fixtures/vpn-client.yaml | kubectl apply -f -

kubectl wait labgroupclient tester -n $LAB_NS \
  --for=jsonpath='{.status.assignedIP}'!='' --timeout=60s
```

Get WireGuard config:

```bash
SECRET=$(kubectl get labgroupclient tester -n $LAB_NS \
  -o jsonpath='{.status.secretRef}')

kubectl get secret $SECRET -n $LAB_NS \
  -o jsonpath='{.data.wg\.conf}' | base64 -d > /tmp/wg-tester.conf

cat /tmp/wg-tester.conf
```

Connect and ping `vpn-client` device:

```bash
VPN_CLIENT_IP=$(kubectl exec -n $LAB_NS $VPN_CLIENT -- \
  ip -4 addr show eth1 | awk '/inet /{gsub(/\/[0-9]+/,"",$2); print $2}')

sudo wg-quick up /tmp/wg-tester.conf
ping -c3 $VPN_CLIENT_IP
sudo wg-quick down /tmp/wg-tester.conf
```

---

### 8.6 — Cleanup

```bash
kubectl delete lab ctf-test1 ctf-test2 ctf-test3 ctf-test4 -n $LAB_NS --ignore-not-found
kubectl delete labgroupclient tester -n $LAB_NS --ignore-not-found
kubectl delete labgroup team-alpha
# Operator garbage-collects namespace team-alpha automatically
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
