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

## Device state persistence (optional)

By default a device is a container in a Deployment: when it restarts, it starts again from its image and the work
done inside it (files, configuration, installed packages) is lost. With device state persistence the **writable
layer** of a device container survives an *unplanned* restart: a crash, an out-of-memory kill, an eviction or the loss
of a node. Planned reboots of a whole OS are out of scope (such tasks belong in VMs).

### What is and is not kept

- Kept: files of the container filesystem (everything except the excluded paths below), including deletions and
  file ownership and attributes.
- Not kept, exactly as in a VM reboot: memory, running processes, open connections, live netfilter rules and routes.
  The device gets the same MAC address on every start, so its DHCP lease is the same.
- When a node dies, the work of the last few seconds (the debounce period plus the snapshot time) is lost.
- Flags and other secrets reach a device as environment variables from a per-device Secret and are never files of the
  writable layer, so they are not part of a snapshot.

### Enable

```yaml
devices:
  statePersistence:
    enabled: true
    registry:
      storageClass: ""        # empty = cluster default
      size: 20Gi
    debounce: 5s              # quiet time of the writable layer before a snapshot
    excludePaths: [/tmp, /var/tmp, /run]
    maxSnapshotSize: 512Mi    # quota per device
    maxLayers: 10             # snapshot layers before they are squashed into one
    retention: 168h           # how long a deleted lab's snapshots are kept
    containerdRoot: /var/lib/k0s/containerd   # host path of the containerd root
```

`enabled: false` (the default) renders nothing of the feature and keeps today's behaviour. With `true`:

- the chart deploys a snapshot registry ([zot](https://zotregistry.dev)) in `laboratory-system`: a
  PersistentVolumeClaim (`registry.storageClass`, `registry.size`; it is kept on `helm uninstall`), a Service, and a
  Secret with generated `htpasswd` credentials for the single writer;
- the operator runs the devices of **new** labs as bare Pods (`restartPolicy: Never`) that it owns and recreates. The
  mode is fixed on each Lab when it is first reconciled (`Lab.status.statePersistence`); flipping the switch never
  changes an existing lab, in either direction;
- the node-agent watches the devices of its node and snapshots them.

Requirements on the nodes (nothing has to be installed or configured on the host):

- containerd 2.x with the default `overlayfs` snapshotter, cgroup v2, and the image layers kept in the content store
  (containerd's CRI option `discard_unpacked_layers` must be `false`, which is the default);
- TCP port `devices.statePersistence.forwardPort` (default 5035) free on `127.0.0.1` of every node;
- the node-agent DaemonSet mounts `containerdRoot` read-only and the host cgroup tree, and gets the
  `DAC_READ_SEARCH` capability. Set `containerdRoot` to the containerd root of your distribution (k0s:
  `/var/lib/k0s/containerd`; stock containerd: `/var/lib/containerd`).

### How it works

1. **Registry access without host setup.** The node-agent (host network) relays `127.0.0.1:<forwardPort>` to the registry
   Service. Snapshot images are referenced as `localhost:<forwardPort>/lab/<namespace>/<lab>/<device>@sha256:...`;
   containerd treats `localhost` registries as plain HTTP, so the kubelet pulls them with no `registries.yaml`,
   certificates or DNS on the node. Reads are anonymous and reachable only from the node itself and the operator; writes
   need the generated credentials (node-agent and operator).
2. **When a snapshot is taken.** Changes of the container's overlay upper directory are detected with inotify plus a
   periodic scan (the scan catches what events miss, for example when the kernel watch limit is reached). After the layer
   has been quiet for `debounce` (at the latest after 30 seconds, or six debounce periods if longer, of continuous writes), the node-agent freezes
   the container cgroup (`cgroup.freeze`), runs `syncfs`, asks containerd's diff service for the difference between the
   container's active snapshot and its parent, and thaws the container. When the container exits (containerd
   `TaskExit`) the layer is final, so the snapshot is taken without freezing, before the pod is removed.
   Without a cgroup v2 freezer the snapshot is taken unfrozen (crash-consistent).
3. **What a snapshot is.** An OCI image in the registry: the base image layers (uploaded once, shared by all devices
   through cross-repository mounts from the `base` repository) plus one snapshot layer per container run, with the
   base image configuration (entrypoint, environment, working directory) unchanged. `excludePaths` and the runtime's
   own mount points (`/dev`, `/proc`, `/sys`, `/etc/hosts`, `/etc/hostname`, `/etc/resolv.conf`, the service account
   directory) are left out of every layer. When the chain exceeds `maxLayers` the snapshot layers are squashed into
   one (whiteouts are preserved, so deletions of files of the base image stay deleted).
4. **Quota.** If a snapshot would make the kept layers larger than `maxSnapshotSize` (uncompressed), the last good
   snapshot is kept and `status.state.warning` of the Device is set; the warning clears with the next good snapshot.
5. **Recreate.** When the pod ends, the operator waits until the node-agent marks the exit snapshot done
   (`status.state.exitSnapshotPod`) or 30 seconds have passed, deletes the pod and creates the next one from the latest
   snapshot (`status.state.image`). Pods are named `<device>-<incarnation>`. A device that keeps ending within 30
   seconds of its start is recreated with a growing back-off (2s, 4s, ... up to 2 minutes).

### Status and controls

`Lab.status.devices[].state` and the agent's `LabDeviceStatus.snapshot` report, per device: the time of the last
snapshot, the time it was last restored from a snapshot, its size in bytes, a quota or failure warning, and whether
rescue mode is on. The management agent has two calls, meant for organizers and admins only (the platform backend
enforces who may call them):

- `ResetDevice(namespace, lab, device)` deletes the device's snapshots and starts it from the base image again;
- `RescueDevice(namespace, lab, device, enable)` starts the device from its latest snapshot with a shell
  (`/bin/sh`, kept alive with `sleep`) instead of the image entrypoint, to repair a configuration that makes the
  service crash. It needs a shell in the image. `enable=false` returns to the normal start. Snapshots keep being taken
  while the device is in rescue mode.

Both set fields on `Device.spec.state` (`resetToken`, `rescue`) that the operator acts on.

### Retention of deleted labs

The snapshots of a lab are not deleted with the lab. The operator runs a sweep every 10 minutes: for each snapshot
repository whose Lab no longer exists it records the time it first noticed this in the ConfigMap
`laboratory-snapshot-retention` (namespace `laboratory-system`), and once `retention` (default 168h) has passed it deletes
the lab's repositories. zot's garbage collection (`registry.gc.interval` and `registry.gc.delay`, both 1h by default)
then frees the blobs, so the space comes back within about `retention` plus two hours. A lab recreated under the same
name before the deadline cancels the deletion.

### Operating it

```bash
# Snapshot state of one device
kubectl -n <lab-namespace> get device <lab>-<device> -o jsonpath='{.status.state}{"\n"}'

# Registry logs and volume usage
kubectl -n laboratory-system logs deployment/laboratory-snapshots
kubectl -n laboratory-system get pvc laboratory-snapshots-registry

# Node-agent: snapshot activity of a node
kubectl -n laboratory-system logs -l app=node-agent -c node-agent | grep device-state
```

Size the volume for the base images plus (devices x `maxSnapshotSize`) in the worst case. If a device's warning says
`state persistence unavailable`, the node's containerd does not use the overlayfs snapshotter or the paths above are
not mounted. A device whose snapshot image cannot be pulled stays in `ImagePullBackOff`; use `ResetDevice` to start it
from the base image.

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
