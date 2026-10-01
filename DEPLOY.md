# Deployment Guide

CyberICEBox Laboratory installs via Helm. All configuration lives in `values.yaml` — no manual editing of YAML
manifests.

## Prerequisites

- `kubectl` ≥ 1.28
- `helm` ≥ 3.12
- Worker nodes need only k0s and a Linux kernel with the `openvswitch`, `geneve`, `wireguard`, `br_netfilter`, `nf_conntrack` and `nf_conntrack_netlink` modules (cgroup v2). Open vSwitch runs in the node-agent DaemonSet, which also loads the modules and sets the sysctls (`nodeAgent.hostPrep`). The image ships Open vSwitch 3.7.1 (alpine 3.24); 4.0.0 comes with the next alpine stable release.

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

## Management agent API

The agent (`LabManager`, [`pkg/agent/protobuf/agent.proto`](pkg/agent/protobuf/agent.proto)) is a thin layer: it
writes custom resources and the per-device Secrets, and scheduling is the operator's job. The CRUD API is **plural
only**: every call takes a list, one object is a list of one. There are no singular calls.

| Family | Calls |
| --- | --- |
| LabGroups | `CreateLabGroups`, `ListLabGroups`, `UpdateLabGroups`, `DeleteLabGroups` |
| VPN clients | `CreateLabGroupClients`, `ListLabGroupClients`, `UpdateLabGroupClients`, `DeleteLabGroupClients` |
| Access | `SetLabGroupAccess` (the policies of many groups, full replacement) |
| Labs | `CreateLabs`, `ListLabs`, `UpdateLabs`, `DeleteLabs` |
| Devices | `ResetDevices`, `RescueDevices` |
| Other | `Ping`, `Monitoring` (stream), `GetCapacity`, `PrewarmImages` |

**References and ids.** Objects are named by id (`ItemRef{lab_group, lab, name}`), never by namespace; the agent
resolves the namespace from the group (`status.namespace`). A LabGroup, and the policy of one, is identified by `name`
alone; a client or lab by `lab_group` + `name`; a device by `lab_group` + `lab` + `name` (the device name).

Ids of LabGroups, Labs and LabGroupClients are **arbitrary strings of 1 to 64 characters** (no control characters), so
any client can use the API. The custom resource is named after the id when that already is a valid DNS-1123 label
(lowercase, at most 63 characters; every UUID is), so `kubectl` shows the real ids. Any other id gets the name `h` +
base36(SHA-256 of the id) (51 characters). The original id is kept in the annotation `laboratory.cybericebox.com/id`;
every answer (results, lists, `Monitoring`, access-policy rules) returns the original, and every lookup by id goes
through the same encoding (`names.EncodeName`). Two ids that would get one name (practically impossible) fail for that item.
Namespaces keep their composed names; device and pod names follow from the CR names.

**Answers.** A mutating call returns one `ItemResult{ref, state, error, retryable}` per item, in request order (selector
calls: sorted by reference). `state` is `CREATED`, `EXISTS`, `UPDATED`, `DELETED`, `NOT_FOUND` or `FAILED`. A failing item
never fails the call; `retryable` marks failures a repetition cures (the object is still being deleted, the group
namespace is not ready yet). The call itself fails with `INVALID_ARGUMENT` only for a malformed request: more than 5000
items, a reserved or invalid label, an invalid selector, a duplicate item or variant id, an unknown variant, device
variables for a device the spec does not have.

**Idempotency.** Re-sending an item whose object exists with the same spec is `EXISTS` (device Secrets are rewritten
with the same values; labels the object lacks are added and reported as `UPDATED`). A different spec for an existing
name is `FAILED` for that item. A lab's spec is compared by a hash the agent stores in an annotation. A write onto an
object that is still being deleted fails with `retryable` (`TERMINATING`); repeat until the object is gone.

**Labels.** Every mutating call has request-level `labels` applied to every item, and per-item `labels` (the item wins on
a conflict). Labels pass through as given, with no re-encoding: keys and values must be valid Kubernetes labels (key
prefix and name, value at most 63 characters, allowed characters only), otherwise the call fails with `INVALID_ARGUMENT`
naming the item. The prefix `laboratory.cybericebox.com/` is reserved and rejected. Labels go onto the custom resource. The labels of a Lab are copied to its Devices and their pods when those are
created (a later `UpdateLabs` changes the Lab's labels only). Update calls take `LabelChanges{set, remove}`.

**Scheduling parameters** are dedicated fields, not labels: `deploy_group` (at most 64 characters) and `deploy_after`
(at most 32 deploy groups this one waits for, each at most 64 characters) on LabGroup and Lab items; no commas. The
agent writes the reserved label `laboratory.cybericebox.com/deploy-group` (the group itself when it is a valid label
value of at most 63 characters, otherwise `h` + base36(SHA-256), `names.DeployKey`) and two annotations that keep the
originals: `laboratory.cybericebox.com/deploy-group` and `laboratory.cybericebox.com/deploy-after` (the original keys,
comma-separated). The operator maps the `deploy-after` keys to group labels with `names.DeployKey`. `LabGroup` and `Lab`
return both fields as given.

**Selectors.** `ListLabGroups`, `ListLabGroupClients` and `ListLabs` take names (`items`) or a Kubernetes label selector
(`a=b,c in (d,e),!f`), never both; nothing means everything; `lab_group` narrows a listing of namespaced kinds to one
group. `Update*`, `Delete*` and the device calls take explicit items or `by_selector`. A selector must be non-empty and
may carry `expected_count`: when MORE objects match, nothing is done and the call fails with `FAILED_PRECONDITION`
(reason `COUNT_MISMATCH`, `client.CountMismatch(err)` returns the actual count). A selector matches at most 5000 objects.
For device calls the selector selects labs, and `device` names the device in each.

**Device variables are secrets only.** `DeviceEnv{device, vars}` becomes the write-only Secret `<lab>-<device>-env`
(loaded through `envFrom`, owned by the Lab). The agent never reads it back, and no variable ever lands in the Lab or
Device spec. A `spec_json` that carries `env`, `flags` or similar fields on a device is rejected, as is any unknown field.
There is no "flag" concept in the agent.

**Variants are not stored.** `CreateLabs` expands every variant into each Lab: the spec is copied into the Lab CR and
the variant's variables are merged with the lab's own (the lab wins per device and variable name) into the Secrets.

**State persistence** is a property of a device in the topology, set at creation and immutable (a lab's spec is never
updated): `devices[].persistence {enabled, debounce, excludePaths, maxSnapshotSize}` in `spec_json`, all optional, with the
platform defaults from the chart. `enabled: true` is refused when `devices.statePersistence.enabled` is off in the
chart (the agent reads it as `AGENT_STATE_PERSISTENCE_ENABLED`); `debounce` must be positive, `excludePaths` absolute,
`maxSnapshotSize` a positive Kubernetes quantity.

**Limits.** Ids and deploy keys at most 64 characters, `deploy_after` at most 32 keys, at most 5000 items per call, message size 64 MiB (`MaxRecvMsgSize`/`MaxSendMsgSize`; the Go client sets the
same call options), bounded internal concurrency (16 calls to the Kubernetes API per request).

### Examples (Go, `pkg/agent/client`)

```go
conn, _ := client.NewConnection(client.Config{Endpoint: "agent.example.com:443", TLS: client.TLS{Enabled: true, /* ... */}})
defer conn.Close()

// LabGroups: request labels for all, item labels per group, scheduling fields.
res, _ := conn.CreateLabGroups(ctx, &pb.CreateLabGroupsRequest{
    Labels: map[string]string{"event": "e1"},
    Items: []*pb.LabGroupItem{
        {Name: "e-1-t-1", Labels: map[string]string{"team": "alpha"}, DeployGroup: "0198c0a4-7a41-7000-8000-000000000001"},
        {Name: "e-1-t-2", DeployAfter: []string{"0198c0a4-7a41-7000-8000-000000000001"}},
    },
})
for _, r := range res.Results { fmt.Println(r.Ref.Name, r.State, r.Error) }

// Suspend every group of the event, but only if exactly the 2 expected ones match.
conn.UpdateLabGroups(ctx, &pb.UpdateLabGroupsRequest{
    BySelector: &pb.Selector{Selector: "event=e1", ExpectedCount: proto.Int64(2)},
    Changes:    &pb.LabGroupChanges{Suspended: proto.Bool(true)},
})

// VPN clients: the config with the private key is in the CREATED result, once.
cl, _ := conn.CreateLabGroupClients(ctx, &pb.CreateLabGroupClientsRequest{
    Items: []*pb.LabGroupClientItem{{LabGroup: "e-1-t-1", Name: "p-alice"}, {LabGroup: "e-1-t-1", Name: "p-bob"}},
})
fmt.Println(cl.Results[0].Client.Status.Config)

// Access for many groups at once.
conn.SetLabGroupAccess(ctx, &pb.SetLabGroupAccessRequest{Policies: []*pb.LabGroupAccessPolicy{{
    LabGroupName: "e-1-t-1",
    Rules: []*pb.LabGroupAccessRule{{Action: pb.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW, ClientNames: []string{"p-alice"}, LabNames: []string{"web"}}},
}}})

// Labs: each variant (spec + common variables) is sent once, items refer to it.
conn.CreateLabs(ctx, &pb.CreateLabsRequest{
    Labels: map[string]string{"event": "e1"},
    Variants: []*pb.LabVariant{{
        VariantId: "web-v3", SpecJson: specJSON,
        Env: []*pb.DeviceEnv{{Device: "web", Vars: map[string]string{"MODE": "ctf"}}},
    }},
    Items: []*pb.LabItem{
        {LabGroup: "e-1-t-1", Name: "c-1", VariantId: "web-v3", Env: []*pb.DeviceEnv{{Device: "web", Vars: map[string]string{"FLAG": "..."}}}},
        {LabGroup: "e-1-t-2", Name: "c-1", VariantId: "web-v3", Env: []*pb.DeviceEnv{{Device: "web", Vars: map[string]string{"FLAG": "..."}}}},
    },
})

// Rewrite one device's Secret, delete all labs of a group's round, reset a device.
conn.UpdateLabs(ctx, &pb.UpdateLabsRequest{Items: []*pb.UpdateLabItem{{LabGroup: "e-1-t-1", Name: "c-1",
    Changes: &pb.LabChanges{Env: []*pb.DeviceEnv{{Device: "web", Vars: map[string]string{"FLAG": "new"}}}}}}})
conn.DeleteLabs(ctx, &pb.DeleteRequest{BySelector: &pb.Selector{Selector: "round=1", LabGroup: "e-1-t-1"}})
conn.ResetDevices(ctx, &pb.DevicesRequest{Items: []*pb.ItemRef{{LabGroup: "e-1-t-1", Lab: "c-1", Name: "web"}}})
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
registry:                     # the platform registry, shared with the image cache (below)
  storageClass: ""            # empty = cluster default
  size: 20Gi
devices:
  statePersistence:
    enabled: true
    debounce: 5s              # quiet time of the writable layer before a snapshot
    excludePaths: [/tmp, /var/tmp, /run]
    maxSnapshotSize: 512Mi    # quota per device
    maxLayers: 10             # snapshot layers before they are squashed into one
    retention: 168h           # how long a deleted lab's snapshots are kept
    containerdRoot: /var/lib/k0s/containerd   # host path of the containerd root
```

`enabled: false` (the default) renders nothing of the feature and keeps today's behaviour. With `true`:

- the chart deploys the platform registry ([zot](https://zotregistry.dev)) in `laboratory-system`: a
  PersistentVolumeClaim (`registry.storageClass`, `registry.size`; it is kept on `helm uninstall`), a Service, and a
  Secret with generated `htpasswd` credentials for the single writer;
- the operator runs the devices of **new** labs as bare Pods (`restartPolicy: Never`) that it owns and recreates. The
  mode is fixed on each Lab when it is first reconciled (`Lab.status.statePersistence`); flipping the switch never
  changes an existing lab, in either direction;
- the node-agent watches the devices of its node and snapshots them.

Requirements on the nodes (nothing has to be installed or configured on the host):

- containerd 2.x with the default `overlayfs` snapshotter, cgroup v2, and the image layers kept in the content store
  (containerd's CRI option `discard_unpacked_layers` must be `false`, which is the default);
- TCP port `registry.forwardPort` (default 5035) free on `127.0.0.1` of every node;
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
enforces who may call them); both take explicit `(lab_group, lab, device)` items or a selector plus a device name
(see [Management agent API](#management-agent-api)):

- `ResetDevices` deletes the devices' snapshots and starts them from the base image again;
- `RescueDevices` (`enable`) starts the devices from their latest snapshot with a shell
  (`/bin/sh`, kept alive with `sleep`) instead of the image entrypoint, to repair a configuration that makes the
  service crash. It needs a shell in the image. `enable=false` returns to the normal start. Snapshots keep being taken
  while the device is in rescue mode.

Rescue mode runs `/bin/sh` in a loop that leaves on any stop signal (an image may name its own, nginx uses `SIGQUIT`).

Both set fields on `Device.spec.state` (`resetToken`, `rescue`) that the operator acts on.

### Retention of deleted labs

The snapshots of a lab are not deleted with the lab. The operator runs a sweep every 10 minutes: for each snapshot
repository whose Lab no longer exists it records the time it first noticed this in the ConfigMap
`laboratory-snapshot-retention` (namespace `laboratory-system`), and once `retention` (default 168h) has passed it deletes
the lab's repositories. zot's garbage collection (`registry.gc.interval` and `registry.gc.delay`, both 1h by default)
then frees the blobs, so the space comes back within about `retention` plus two hours. A lab recreated under the same
name before the deadline cancels the deletion.

### Turning it off

`devices.statePersistence.enabled: false` only stops new labs from using persistence. Labs that already run with it
keep depending on the registry and on the node-agent's snapshot engine, which the switch also removes from the
node-agent. Leave the switch on until the last such lab is deleted (or accept that they stop being snapshotted).

### Operating it

```bash
# Snapshot state of one device
kubectl -n <lab-namespace> get device <lab>-<device> -o jsonpath='{.status.state}{"\n"}'

# Registry logs and volume usage
kubectl -n laboratory-system logs deployment/laboratory-registry
kubectl -n laboratory-system get pvc laboratory-registry

# Node-agent: snapshot activity of a node
kubectl -n laboratory-system logs -l app=node-agent -c node-agent | grep device-state
```

Size the volume for the base images plus (devices x `maxSnapshotSize`) in the worst case. If a device's warning says
`state persistence unavailable`, the node's containerd does not use the overlayfs snapshotter or the paths above are
not mounted. A device whose snapshot image cannot be pulled stays in `ImagePullBackOff` and its status warning says
`snapshot image unavailable`; the operator never falls back to the base image on its own (that would lose state
silently). Fix the registry, or use `ResetDevices` to start it from the base image.

---

## Image cache (optional)

Every lab pulls its images (device images, the VPN and gateway images, the netconfig init container) from their
registries on every node. With the image cache on, the platform registry (zot) sits in between as a
pull-through cache: the first node that needs an image makes zot fetch it from the upstream registry (on-demand
sync), and every other node pulls it from zot, inside the cluster. It saves upstream bandwidth and rate limits
(Docker Hub) and speeds up a burst of labs.

```yaml
registry:
  cache:
    enabled: true
    unusedTTL: 48h          # a cached image nobody pulled for this long is deleted
    registries: [docker.io, ghcr.io, quay.io, registry.k8s.io]   # built in
    extraRegistries: []     # - name: registry.example.com
                            #   url: https://registry.example.com
```

It is independent of state persistence: either switch deploys the registry (the full zot image, about 70 MB
compressed on amd64 and arm64; the cache needs its sync extension), both can be on. Needs `nodeAgent.enabled`.

- **How the nodes reach it.** As for snapshots: the node-agent relays `127.0.0.1:<registry.forwardPort>` on every node
  to the registry Service, and containerd reads `localhost:<port>` as a plain-HTTP registry, so nothing is configured
  on the host.
- **What is rewritten.** The operator rewrites `REG/repo:tag` (and `@sha256:` references) into
  `localhost:<port>/REG/repo:tag`, for registries in `registry.cache.registries` and `extraRegistries` only. Names
  follow Docker rules: `nginx` is `docker.io/library/nginx:latest`. Snapshot images and images of other registries
  are pulled directly. zot maps the `REG/` prefix back to the upstream registry (one sync entry per upstream, with
  the destination `/REG`).
- **Fixed per lab.** The mode is recorded in `Lab.status.imageCache` when the lab is first reconciled, and the
  device image is written to `Device.spec.imageMirror`; switching the cache on or off never changes existing labs.
  The VPN and gateway images of a lab group are rewritten when the group's pods are created.
- **Launch pacing.** With the cache on, the prepull DaemonSet pulls the rewritten images, so the first node warms
  zot and the others pull from it.
- **Snapshots.** The base layers of a device snapshot are mounted from the cached repository of the base image
  instead of being uploaded.
- **Upstream credentials.** zot reads them from a Secret in its sync credentials format. The chart builds it from
  `imagePullSecrets` (dockerconfigjson Secrets in the release namespace) with `lookup`, which sees nothing under
  `helm template` or GitOps renderers; there, create your own Secret with the key `credentials.json`
  (`{"ghcr.io": {"username": "...", "password": "..."}}`; Docker Hub is `registry-1.docker.io`) and set
  `registry.cache.credentialsSecret`. Restart the registry pod after the credentials change.
- **Digest pinning.** A tag can move upstream during an event. When a lab is created with the cache on, the operator
  asks the upstream registry (directly, with the pull secrets; not through zot, which would answer from its store)
  for the digest of each device image and of the netconfig image, once, and the lab's pods pull
  `localhost:<port>/REG/repo@sha256:...`. The digests are recorded in `Lab.status.imageDigests` and copied to
  `Device.spec.imageDigests`; the prepull uses them. A resolved digest is remembered for `registry.cache.pinTTL`
  (default 30m), so labs created in the same wave get the same image. The VPN and gateway images of a group are
  pinned the same way when the group's pods are created; a failure there is recorded in `LabGroup.status.imageWarning`.
  Both warnings reach the management agent (`LabStatus.image_warning`, `LabGroupStatus.image_warning`). An image whose digest cannot be resolved is pulled by its
  tag and named in `Lab.status.imageWarning` (and a Warning event on the Lab). The operator needs HTTPS egress to the
  upstream registries for this (the chart's operator network policy allows it when the cache is on).
- **Platforms.** zot syncs a whole multi-platform image index, Windows variants included (the `pause` image alone is
  about 550 MB across all platforms). So when every lab node has one architecture the operator pins the digest of that
  platform's manifest instead of the index, and the cache fetches that one image. With several architectures it pins
  the index. An image pulled by tag (unpinned fallback) always fetches the whole index. zot runs with
  `http.compat: [docker2s2]` and `preserveDigest` so that the digests of Docker-format images stay what the upstream
  registry says; without them a pull by digest of such an image fails.
- **Retention.** The cache is not an archive. zot's retention policy deletes a cached image that nobody pulled for
  `registry.cache.unusedTTL` (default 48h, counted from the last pull, so an image in use is never deleted), and its
  garbage collection frees the blobs; the image is simply fetched again on the next pull. Snapshot repositories
  (`lab/...`) are never touched by it.
- **Size.** The registry volume (`registry.size`) holds the cached images as well: plan for the images of the labs you
  run, next to the snapshots.


### Cache prewarm

An event starts many labs at once. The first pull of an image through the cache makes zot fetch it from the upstream
registry, which is slow for the first lab. **Prewarm** fills the cache before the event. It is done by the
management agent only (the operator and the launch queue take no part), through one RPC:

`PrewarmImages(PrewarmImagesRequest{images}) returns (PrewarmImagesResult{images[]})`, each element
`{image, state, error, digest, updated_unix_ms}` with `state` one of `QUEUED`, `WARMING`, `DONE`, `FAILED`, `SKIPPED`.

- **Asynchronous and idempotent.** The call returns at once with the current state of every requested image; repeat it
  to poll. A new image starts; a `QUEUED` or `WARMING` one is only reported; a `FAILED` one is tried again once
  30 seconds have passed since it failed (so a poller sees the failure); a `DONE` one older than 30 minutes is checked
  again. An empty list reports every image the agent knows. The status is kept in the agent's memory: after an agent
  restart, ask again (the images are checked in seconds).
- **What it does.** For each image the agent resolves the tag to the digest the operator will pin labs to (the manifest of the
  node platform when every lab node has one architecture, the index otherwise), asks zot for that manifest, and zot
  fetches the image from upstream before it answers. zot stores the blobs while it serves that manifest request (checked
  on the stand: the layers are on the volume although nobody pulled a blob); the agent then checks that the config and
  **every layer blob** are stored (an index: every platform manifest too), and only then reports `DONE` with the digest.
  A lab created later pins the same digest and its nodes pull from zot with no upstream traffic.
- **Concurrency and time.** `registry.cache.prewarm.concurrency` images at once (4), at most `registry.cache.prewarm.timeout`
  (10m) for one; every image has its own error.
- **`SKIPPED`:** the image's registry is not in the cache list (`registry.cache.registries`, `extraRegistries`): nodes pull it
  directly. The whole call fails with `FailedPrecondition` when the cache is not enabled.
- **Retention.** The manifest request counts as a pull for the cache's `unusedTTL` (48h), as does every later check.
  Prewarm within 48 hours before the event, or repeat the call: an image nobody pulled for 48 hours is deleted.
- **Requirements.** `agent.enabled` and `registry.cache.enabled`. The chart gives the agent the cache address, the
  registry list, and read access to exactly the `imagePullSecrets` Secrets of the release namespace (used to ask
  upstream registries for digests), and opens the network policies between the agent, zot and the upstream registries.

---

## Monitoring stream

`LabManager.Monitoring` (agent.proto) is the server-push stream the platform backend follows. One agent can serve
several backends at once (for example two platform instances against one cluster): each opens its own stream with its
own selector, minimum interval and position, and all of them are fed by **one** poller and **one** in-memory journal.

### Request

| Field | Meaning |
|---|---|
| `selector` | Kubernetes label selector (`instance=a`, `tier in (gold,silver),!legacy`). Empty = everything. Invalid: `InvalidArgument`. |
| `min_interval_ms` | Least time between two messages of this stream; changes in between are merged into one message. Bounded below by 10 ms; the agent observes the platform every `agent.monitoring.pollInterval` (1s), so that is the real resolution. Default 5 s when unset. |
| `resume_after_sequence`, `agent_epoch` | Where to resume (below). 0 / empty = no resume. |

### What is sent

- The **first message** is either a full **snapshot** (`snapshot = true`: replace everything you hold) or, on a successful
  resume, the merged updates you missed (`snapshot = false`). Then deltas follow. A delta holds the changed or new
  records (`groups`, `labs`, `clients`, `policies`, `traffic`), `deleted_keys`, and `capacity` when it changed.
- Every message carries `agent_epoch` and `sequence`, the position of the stream up to which everything has been sent
  (or filtered out as not matching). A quiet stream sends an empty heartbeat every 30 s with the position, so that the
  stored position stays inside the journal window.
- **Selector.** It is matched against the Kubernetes labels of the object itself: LabGroup, Lab, LabGroupClient and the
  access policy. A deletion is delivered iff the deleted object matched (the journal keeps the labels of deleted objects).
  A traffic report is cut down to the touches of labs that match, and dropped when none do (it follows the labels of
  its lab). **Capacity always goes to everyone.** A change of labels that moves an object out of the selector is not
  sent as a deletion: use labels that do not change (an instance or tenant id).
- **Labels** are set through the same API: `labels` on `LabGroup`, `Lab`, `LabGroupClient` (create and update) and on
  `LabGroupAccessPolicy`. Updates merge labels in; a label is never removed this way. The access policy takes the labels of
  its LabGroup plus its own, so a selector that matches a group also matches its policy. The labels are part of the
  messages (`labels`, field 10).

### Resume contract for the backend

1. Keep, per stream, the last `sequence` and `agent_epoch` of the last message you **processed**.
2. On every (re)connect send them as `resume_after_sequence` and `agent_epoch`.
3. Look at the first message: `snapshot = true` means replace your state with it (the agent restarted, the epoch changed,
   or the journal no longer reaches back to your position); `snapshot = false` means apply it on top of what you hold.
4. Sequences grow within an epoch and may skip numbers (updates that did not match your selector). Never assume
   `sequence + 1`. A new `agent_epoch` always starts a new snapshot.
5. If the stream ends with `ResourceExhausted`, your consumer was too slow: its buffer of `subscriberBuffer` updates
   overflowed and the agent dropped it so that the others are not held back. Reconnect with your last position (a
   snapshot follows if the journal lost it). Any other error: reconnect the same way.

The journal lives in the agent's memory: an agent restart gives a new epoch (a snapshot), and a position older than
`journalSize` updates or `journalAge` is answered with a snapshot too. Sizing: an update is a few KiB, so the default 10000 updates are tens of MiB at
most; raise `agent.resources` together with `journalSize`.

```yaml
agent:
  monitoring:
    journalSize: 10000      # updates kept for resuming subscribers
    journalAge: 15m         # how long an update stays
    pollInterval: 1s        # observation period while anybody is subscribed
    subscriberBuffer: 256   # updates a subscriber may lag before it is dropped
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
