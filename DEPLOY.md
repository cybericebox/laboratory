# Deployment Guide

CyberICEBox Laboratory installs via Helm. All configuration lives in `values.yaml` — no manual editing of YAML
manifests.

## Prerequisites

- `kubectl` ≥ 1.28
- `helm` ≥ 3.12
- OVS installed on every worker node (required by node-agent)

---

## 1. Create the Cluster (Kind example)

```bash
cat > kind-config.yaml <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
EOF
kind create cluster --name lab --config kind-config.yaml
```

For a real cluster, skip this step and point `KUBECONFIG` at the existing config.

---

## 2. Configure

Create your `values.yaml` override file. Required: `operator.publicVPNEndpoint`, `operator.baseDomain`, and the lab access public key (below):

```yaml
# my-values.yaml
operator:
  publicVPNEndpoint: "vpn.example.com:51820"   # REQUIRED
  baseDomain: "lab.example.com"                 # REQUIRED
  supportEmail: "support@example.com"           # shown on the VPN probe page; defaults to support@cybericebox.com
```

For Kind, use the node IP:

```bash
NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
cat > my-values.yaml <<EOF
operator:
  publicVPNEndpoint: "${NODE_IP}:51820"
  baseDomain: "lab.local"
  proxySourceCIDRs: "${NODE_IP}/32"
EOF
```

The proxy verifies the platform's lab access tokens (Ed25519) with a public key. Generate the pair once,
give the private key to the backend (`LAB_ACCESS_PRIVATE_KEY`) and pass the public one to the chart:

```bash
make lab-access-keys   # /tmp/lab-access-private.pem and /tmp/lab-access-public.pem
# install with: --set-file platform.labAccessPublicKey=/tmp/lab-access-public.pem
```

The proxy's own session-cookie key (`SESSION_SECRET`) is generated once and kept across upgrades; to supply it
yourself set `proxy.l7.sessionSecret.existingSecret` (a Secret with key `sessionSecret`, 32+ bytes) or
`proxy.l7.sessionSecret.value`.

Full list of available values: see `charts/laboratory/values.yaml`.

---

## 3. Install

```bash
helm install laboratory ./charts/laboratory \
  --namespace laboratory-system \
  --create-namespace \
  -f my-values.yaml \
  --set-file platform.labAccessPublicKey=/tmp/lab-access-public.pem
```

What gets installed:

- CRDs (Pools, LabGroups, LabGroupClients, Labs, Devices, Connections)
- `laboratory-system` namespace
- Operator (Deployment + RBAC)
- Node-agent (DaemonSet + RBAC) on every node
- `laboratory-config` ConfigMap with your settings

Verify:

```bash
kubectl -n laboratory-system get pods
kubectl -n laboratory-system get configmap laboratory-config -o yaml
```

---

## 4. Upgrade (change config)

Edit `my-values.yaml`, then:

```bash
helm upgrade laboratory ./charts/laboratory \
  --namespace laboratory-system \
  -f my-values.yaml
```

The ConfigMap is re-rendered from values. Operator pod restarts automatically (Deployment update triggers rollout).

---

## 5. Use

### Create a LabGroup (one per team/tenant)

```yaml
# labgroup-alpha.yaml
apiVersion: laboratory.cybericebox.com/v1alpha1
kind: LabGroup
metadata:
  name: team-alpha
spec:
  vpn: { }
```

```bash
kubectl apply -f labgroup-alpha.yaml
kubectl get labgroup team-alpha
# Wait for STATUS=Ready, VPN.REGISTERED=true
```

### Add VPN clients

```yaml
# lgc-alice.yaml
apiVersion: laboratory.cybericebox.com/v1alpha1
kind: LabGroupClient
metadata:
  name: alice
  namespace: team-alpha-ns
spec: { }
```

```bash
kubectl apply -f lgc-alice.yaml
# Get WireGuard config:
kubectl -n team-alpha-ns get secret vpn-client-alice -o jsonpath='{.data.wg\.conf}' | base64 -d
```

### Create a Lab

```yaml
# lab-01.yaml
apiVersion: laboratory.cybericebox.com/v1alpha1
kind: Lab
metadata:
  name: lab-01
  namespace: team-alpha-ns
spec:
  internet:
    enabled: false
  devices:
    - name: attacker
      image: kalilinux/kali-rolling:latest
      type: Device
    - name: target
      image: ubuntu:22.04
      type: Device
    - name: sw1
      type: Switch
  connections:
    - endpoints: [ attacker, sw1 ]
    - endpoints: [ target, sw1 ]
```

```bash
kubectl apply -f lab-01.yaml
kubectl -n team-alpha-ns get lab,devices,connections
```

---

## 6. Uninstall

```bash
helm uninstall laboratory --namespace laboratory-system
kubectl delete namespace laboratory-system
# CRDs are NOT removed by helm uninstall — remove manually if needed:
kubectl delete crd \
  pools.allocation.cybericebox.com \
  labgroups.laboratory.cybericebox.com \
  labgroupclients.laboratory.cybericebox.com \
  labs.laboratory.cybericebox.com \
  devices.laboratory.cybericebox.com \
  connections.laboratory.cybericebox.com
```

---

## Network Layout

| Network             | Default       | Usage                                                              |
|---------------------|---------------|--------------------------------------------------------------------|
| `vpnBaseNetwork`    | `10.8.0.0/10` | Full VPN supernet; advertised as AllowedIPs to WireGuard clients   |
| Client subnet       | `10.8.0.0/24` | First /24 of vpnBaseNetwork; VPN gateway at .1, clients at .2–.254 |
| Per-lab VPN subnet  | `10.8.N.0/24` | N=1..253 carved from vpnBaseNetwork                                |
| `inetBaseNetwork`   | `10.9.0.0/10` | Internet-gateway subnets                                           |
| Per-lab inet subnet | `10.9.N.0/24` | Mirrors VPN lab index                                              |

---

## Troubleshooting

```bash
# Operator logs
kubectl -n laboratory-system logs deployment/laboratory-controller-manager

# Node-agent logs (specific node)
kubectl -n laboratory-system logs -l app=node-agent -c node-agent --tail=50

# Inspect applied config
kubectl -n laboratory-system get configmap laboratory-config -o yaml

# Render chart without installing (dry-run)
helm template laboratory ./charts/laboratory -f my-values.yaml | less
```
