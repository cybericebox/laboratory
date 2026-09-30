# Deployment Guide

CyberICEBox Laboratory installs via Helm. All configuration lives in `values.yaml` — no manual editing of YAML
manifests.

## Prerequisites

- `kubectl` ≥ 1.28
- `helm` ≥ 3.12
- Worker nodes need only k0s and a Linux kernel with the `openvswitch`, `geneve`, `wireguard`, `br_netfilter`, `nf_conntrack` and `nf_conntrack_netlink` modules (cgroup v2). Open vSwitch runs in the node-agent DaemonSet, which also loads the modules and sets the sysctls (`nodeAgent.hostPrep`).

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

> A complete local k0s cluster (Lima VMs, Cilium Gateway, this chart, test lab and checklist) is in
> `$(LOCAL_K0S)/README.md` in the infrastructure repo (default `../infra/local/cluster`); `make cluster-up` uses the Kind config from `$(LOCAL_K0S)/kind/`.

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

The chart never receives secret values: create these Secrets before installing.

The proxy verifies the platform's lab access tokens (Ed25519) with a public key. Generate the pair once,
give the private key to the backend (`LAB_ACCESS_PRIVATE_KEY`) and put the public one in a Secret:

```bash
make lab-access-keys   # /tmp/lab-access-private.pem and /tmp/lab-access-public.pem
kubectl create namespace laboratory-system
kubectl -n laboratory-system create secret generic lab-access-public-key \
  --from-file=public.pem=/tmp/lab-access-public.pem
```

The proxy's own session-cookie key (`SESSION_SECRET`, 32+ bytes, not the access key):

```bash
kubectl create namespace laboratory-proxy
kubectl -n laboratory-proxy create secret generic proxy-session \
  --from-literal=sessionSecret="$(openssl rand -base64 48)"
```

After rotating it run `kubectl -n laboratory-proxy rollout restart deploy/laboratory-proxy`.

With an ACME issuer (`certManager.selfSigned: false`) the wildcard proxy certificate needs DNS-01: create a
Cloudflare API token (Zone:Zone:Read + Zone:DNS:Edit on the zone) Secret in the cert-manager namespace and
name it in `certManager.dns01.cloudflare.apiTokenSecretRef`.

Full list of available values: see `charts/laboratory/values.yaml`.

---

## 3. Install

```bash
helm install laboratory ./charts/laboratory \
  --namespace laboratory-system \
  --create-namespace \
  -f my-values.yaml
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

## Launch pacing

When an event starts, the platform may create hundreds of Labs at once. A new Lab is
created at once but stays in phase `Queued`; the operator admits labs so the cluster is
brought up smoothly instead of all at the same moment.

**Order.** Labs are admitted by *launch class* first, then by creation time. The class is
the lab type: `spec.launchClass` (for example an exercise version or variant id, set by
the platform through the agent's `spec_json`). If it is empty the operator derives it from
a hash of the lab topology and images (`auto-<hash>`), so labs built from one template
share a class. A class is admitted completely before the next one starts, and the class
with the oldest queued lab goes first, so one lab type appears for all teams at about the
same time.

**Admission.** The head of the queue is admitted only when all of these hold; otherwise
it waits (it never fails) and the queue reports why:

| Condition | Reason while waiting |
|---|---|
| fewer than `launch.maxInFlight` labs are provisioning (a lab holds its slot from admission until it is Ready or `launch.waveTimeout` passes) | `InFlightLimit` |
| the images of its class are on the nodes | `PreparingImages` |
| the schedulable nodes have free CPU and memory for the lab's requests, and `launch.headroomPercent` of their allocatable CPU and memory stays free | `InsufficientResources` |
| there is at least one schedulable node | `NoSchedulableNodes` |

A lab larger than the whole cluster (minus headroom) cannot ever fit; it steps aside and
does not block the labs behind it, but it stays `Queued` with `InsufficientResources`.

**Image prepull.** Before the first lab of a class is admitted the operator creates a
short-lived DaemonSet `prepull-<hash>` in `laboratory-system`: one container per image of
the class, with the command replaced by a sleep (the lab service never starts), on the
nodes matching `labWorkloads.nodeSelector` / `tolerations`, using `imagePullSecrets`. It
waits until every pod holds all images, or `launch.prepull.timeout`, then deletes the
DaemonSet (a missing image must not stop the queue). Images that run as a shell-less
container still count as pulled: the kubelet reports the image ID either way.

**Free resources** are the allocatable CPU and memory of the schedulable nodes (Ready,
not cordoned, matching the lab node selector, taints tolerated) minus the requests of all
pods scheduled there. The requests of labs that were just admitted, whose pods are not
scheduled yet, are held back.

**Guaranteed resources.** Every device container gets requests equal to limits, so its pod
is Guaranteed and the scheduler sees its real load. Per resource the limit wins, then the
request, then `launch.deviceDefaults` (250m CPU and 256Mi memory). A declared request and
limit that differ collapse to the limit. Set a default to `""` to leave a device without
that resource (best effort, the behavior before launch pacing). The resources are applied
when a device is created; running devices are not changed. Each lab group namespace has
one PodDisruptionBudget `lab-group` (`maxUnavailable: 0`, all pods of the namespace), so
node drains and the autoscaler do not evict running labs; the per-device budgets of older
versions are removed because a pod under two budgets cannot be evicted.

**Status.** `kubectl get lab` shows the phase; `status.launch` has the class,
`admittedAt`, and, while `Queued`, the place in the queue:

```bash
kubectl get labs -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase,POS:.status.launch.position,OF:.status.launch.length,WHY:.status.launch.reason
```

The position is refreshed at a limited rate (at most 50 writes per 2 s tick, one per lab
per 10 s), so in a long queue it can lag by a few seconds. The agent's gRPC `LabStatus`
carries the same in `queue` (`position`, `length`, `reason`, `launch_class`,
`admitted_at_unix_ms`), also on the `Monitoring` stream, so a UI can show
"in queue: position of length".

**Values** (chart `launch.*`):

| Value | Default | Meaning |
|---|---|---|
| `enabled` | `true` | `false` provisions every lab as soon as it is created |
| `maxInFlight` | `20` | labs provisioning at once; `0` = no limit |
| `waveTimeout` | `3m` | how long an admitted lab holds its slot if not Ready |
| `headroomPercent` | `10` | share of schedulable CPU and memory kept free (0-99) |
| `resourceCheck` | `true` | `false` skips the free-resource check |
| `prepull.enabled` | `true` | prepull images per class |
| `prepull.timeout` | `5m` | admission goes on after this long |
| `deviceDefaults.cpu` / `.memory` | `250m` / `256Mi` | resources of a device that declares none |

Labs that existed before the upgrade are never queued: a Lab with any phase other than
empty or `Queued` counts as admitted. The operator needs `get/list/watch` on nodes and
`create/delete/get/list/watch` on DaemonSets; the chart's ClusterRole has them.

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
| `vpnBaseNetwork`    | `10.128.0.0/10` | Full VPN supernet; advertised as AllowedIPs to WireGuard clients   |
| Client subnet       | `10.128.0.0/24` | First /24 of vpnBaseNetwork; VPN gateway at .1, clients at .2–.254 |
| Per-lab VPN subnet  | `10.128.N.0/24` | N>=1 carved from vpnBaseNetwork                                |
| `inetBaseNetwork`   | `10.192.0.0/10` | Internet-gateway subnets                                           |
| Per-lab inet subnet | `10.192.N.0/24` | Mirrors VPN lab index                                              |

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
