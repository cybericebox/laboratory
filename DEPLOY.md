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
> `$(LOCAL_K0S)/README.md` in the infrastructure repo (default `../infrastructure/local/cluster`); `make cluster-up` uses the Kind config from `$(LOCAL_K0S)/kind/`.

---

## 2. Configure

Create your `values.yaml` override file. Required: `operator.publicVPNEndpoint`, `operator.baseDomain` and, with the agent, `agent.domain`. Tenants (the platform backends that use the agent) are declared under `tenants:` (see [Tenancy](#tenancy)):

```yaml
# my-values.yaml
operator:
  publicVPNEndpoint: "vpn.example.com:51820"   # REQUIRED
  baseDomain: "lab.example.com"                 # REQUIRED
  supportEmail: "support@example.com"           # shown on the VPN probe page; defaults to support@cybericebox.com
agent:
  enabled: true
  domain: "ctl.example.com"                     # REQUIRED with the agent
tenants:
  platform: { }                                 # one Tenant per backend; the name is its client certificate CN
```

Domains are configuration only, with no defaults in code: `operator.baseDomain` (web devices are
`<device>-<code>.<baseDomain>`, the proxy serves `*.<baseDomain>`), `operator.publicVPNEndpoint` (the WireGuard
demultiplexer) and `agent.domain` (the management agent). See `docs/specs/domains.md`.

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

The proxy verifies lab access links with the public access keys of the tenants, which a tenant registers by
enrolling (see [Enrollment & access keys](#enrollment--access-keys)); there is no key to create here.

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

- CRDs (Pools, Tenants, LabGroups, LabGroupClients, LabGroupAccessPolicies, Labs, Devices, Connections, ImagePulls (prepull requests), and the internal
  LabVPNs, LabGateways, LabTrafficReports)
- `laboratory-system` namespace, the `laboratory-tenants` namespace (enrollment tokens and access keys of tenants) and the `laboratory-images` namespace (credentials of a prepull request)
- One `Tenant` per entry of `tenants:`, and `default`
- Operator (Deployment + RBAC)
- Node-agent (DaemonSet + RBAC) on every node
- Management agent (`agent.enabled`), the L7 proxy and WireGuard demultiplexer, and, when enabled, the platform registry
- `laboratory-config` ConfigMap with your settings

**The proxy** (`proxy.*`) is one Deployment with two containers, the L7 HTTPS proxy and the WireGuard demux, **two replicas by default**
(`proxy.replicas`), a PodDisruptionBudget with `minAvailable: 1` (`proxy.podDisruptionBudget`, rendered only with more than one replica) and a
a **required** pod anti-affinity by hostname: never two replicas on one node (with more replicas than nodes the extra ones stay Pending). For larger clusters
set `proxy.mode: daemonset`: one proxy pod on every node (`proxy.nodeSelector` and `proxy.tolerations` narrow the set; `replicas` and the budget do not apply; the default is
`replicas`, a fixed count: on a one-node cluster set `proxy.replicas: 1`, and a live install refuses more replicas than nodes). In both modes the proxy requests are pods like any other, so the scheduler's room for lab pods on each node is what is left after them
(see "Platform reserve"). The proxy's own session is sliding: it expires `proxy.l7.sessionIdleTTL` (24h) after the last request; the cookie is
re-issued only when less than `proxy.l7.sessionRenewBefore` (1h) of it remains, so an active user costs about one renewal per hour; and it never
lives past `proxy.l7.sessionMaxTTL` (168h) from the handoff or past the link's own session end. The L7 proxy is stateless (its session cookie is signed with the shared `proxy-session` secret), so a request
may land on any replica. The demux keeps no state a restart would lose: a WireGuard client that lands on the other replica (after a reconnect, or when
the replica it used is gone) re-handshakes within its persistent keepalive, about 15 s. Both containers are Guaranteed (requests = limits): L7 `500m`/`64Mi`,
demux `250m`/`64Mi`. Measured on the local stand (kubectl top, one replica): L7 serves about 5000 requests per second for 500m of CPU (0.1 ms per request;
p95 63 ms at 100 concurrent clients, 36Mi at 200), the demux is 1m idle and about 110m at ten active VPN peers (it relays the packets in userspace),
and L7 load does not slow the tunnels (VPN latency p95 3-4 ms with 100 L7 clients).

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

## Scheduler

When an event starts, the platform creates many Labs and LabGroups at once. The scheduler in the
operator starts their pods through a conveyor, so the cluster is brought up smoothly instead of
all at the same moment.

**Units and slots.** A unit is a top-level object: a *LabGroup* (its VPN and gateway pods) or a *Lab*
(one pod per container device; switches and hubs have no pod). A slot is a **pod**: an object needs as
many slots as it has pods. `scheduler.maxPods` pods (default 20) may be starting at once. A pod holds
its slot from the moment it is dispatched until it is Ready or declared failed.

**Conveyor.** The pods of an object are dispatched one object after another, and no other object's
pods are interleaved once an object has started. An object may start partially: with 3 free slots and
5 pods, 3 start now and 2 as slots free up.

**Service pods go first.** The VPN and gateway pods of a LabGroup have a lane of their own: they are
dispatched ahead of every queued Lab, so a new team is not held up by a burst of labs that wait for room
(`InsufficientResources`). They still need a slot and room, and they obey `deploy-after`. Labs keep
strict order among themselves (no backfill: a lab that does not fit holds back the labs behind it).

**Groups.** Objects with the same *deploy group* form a group (mixed kinds, one object or many). The
agent writes two operator-internal markers on LabGroups and Labs; the operator reads only these, never
user labels (and they work when someone applies the CRs directly):

| Marker | Meaning |
|---|---|
| label `laboratory.cybericebox.com/deploy-group=<key>` | the group of the object; key of at most 63 characters (base36) |
| annotation `laboratory.cybericebox.com/deploy-after=<key1>,<key2>` | the groups that must be **complete** (every pod Ready or failed) before this group starts |

The rules:

1. The group being dispatched (some pod out, some still pending) gets the slots first.
2. When it has **no undispatched pod left**, the next group starts on the next free slot; it does not wait
   for readiness.
3. The order of groups is arrival order (the creation time of their first object), except that a group
   with `deploy-after` starts only when all listed groups are complete. A group that waits for another
   does not hold up the groups after it. An unknown key waits too, and the status says so
   (`waiting for group X (not known yet)`); a cycle starts nothing.
4. Objects **without** a deploy group are independent: they go one object at a time, in creation order,
   after all grouped work that can start.
5. A late object (a team that registered late) joins its group: if the group is still being dispatched it
   goes with it, and a group that already finished is dispatched again first, ahead of the groups that
   have not started. Pods that already run are never affected.

**Before the first pod of a group** its images (those of all its Labs) are pulled onto the eligible nodes. The
scheduler makes a cluster-scoped `ImagePull` request `prepull-<hash>` listing the images and the nodes of
`labWorkloads.nodeSelector`/`tolerations`; the node-agent of each listed node pulls them through the container
runtime's image service (CRI `PullImage`) and writes its own entry in `status.nodes`. **No tenant code runs and no
pod is made**: the runtime only fetches and unpacks the image, so a tenant image with a hostile `/bin/sh` gets
nothing from the prepull (it used to run, as root with the platform pull secrets, in `laboratory-system`).
Credentials are the tenant's own (see "Tenant images" below), never the platform's: the operator copies the
tenant's registry Secret into the `laboratory-images` namespace for the life of the request, the node-agent
(whose Role there is `get` on Secrets only) reads it, and the copy goes with the request. A request is one per
tenant and launch class. The group's pods wait until every listed node has resolved all images, then the request
is deleted. An image that **cannot be pulled** (a wrong name or tag, no access: the runtime refuses it) is
given up on at once, within seconds: the group goes on without it and its pods that use that image fail
through the normal per-pod `ImagePull` path (reason and error in `Device.status.scheduling.failure`). A node
whose node-agent does not answer holds the group until `scheduler.prepull.timeout`. Pulls on a node run two
at a time (`IMAGE_PULL_CONCURRENCY`, 5 minutes per image: `IMAGE_PULL_TIMEOUT`, node-agent environment). `scheduler.prepull.timeout` is only a safety net for a pull that is slow
but not failing. A prepull gates **only its own group**: while a group's images are still being pulled
(reason `PreparingImages`), the pods of other groups and of independent objects are dispatched as the order and
dependency rules allow, and their own prepulls run meanwhile. Filling the registry cache beforehand is the
agent's job (`PrewarmImages`), not the scheduler's.

**Resource check.** A pod is dispatched only when the schedulable nodes (Ready, not cordoned, matching the
lab node selector, taints tolerated) have free CPU and memory for its requests: allocatable minus the
requests of all scheduled pods, minus what already dispatched pods will still request, and with the platform
reserve kept free (see "Platform reserve" below). Otherwise the queue waits (it does not
fail) and the reason is in the status. A pod that requests more than the whole schedulable capacity can never
fit: it is declared failed (`DoesNotFit`) and the queue goes on.

**Platform reserve.** CPU and memory that user labs can never consume, so the platform itself always has room.
It is `scheduler.platformReservePercent` (10) of the schedulable CPU and memory, plus an optional absolute reserve on
every schedulable node (`scheduler.platformReserveCpu`, `scheduler.platformReserveMemory`, `"0"` = none) that comes off the
node first. It is applied after the kubelet's own reserve (a node's allocatable is already net of kube-reserved,
system-reserved and eviction thresholds) and after the requests of every pod scheduled on the node: DaemonSets, the
proxy (Deployment or DaemonSet mode alike) and system pods are counted because the check subtracts the requests of all
scheduled pods, not only lab pods. The value `scheduler.headroomPercent` of earlier versions was renamed, and the chart
refuses it.

**Failed pods.** A dispatched pod that is not Ready after `scheduler.startupTimeout` (5m), or that restarted
`scheduler.restartThreshold` times (5), is declared failed: its slot is freed, the group still completes,
nothing else is rolled back, and the device gets a warning: `Device.status.scheduling.failure` and, on the
Lab, `status.devices[].failure`, with the reason (`ImagePull`, `CrashLoop`, `Unschedulable`,
`StartupTimeout`, `DoesNotFit`), the last error the node reported and the restart count. If the pod becomes
Ready later it is Started and the warning clears. A snapshot-backed device whose pod ended after it had started
is recreated at once, with no slot, as without the scheduler.

**Guaranteed resources.** Every device container gets requests equal to limits, so its pod is Guaranteed.
Per resource the limit wins, then the request, then `limits.device.defaultCpu` / `defaultMemory` (100m CPU and 256Mi memory);
a declared request and limit that differ collapse to the limit. The resources are applied when a device is created; running devices are not
changed. Each lab group namespace has one PodDisruptionBudget `lab-group` (`maxUnavailable: 0`, all pods of
the namespace), so node drains and the autoscaler do not evict running labs.

**Status.** `Lab.status.scheduling` and `LabGroup.status.scheduling` show the place in the queue:

```bash
kubectl get labs -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase,GROUP:.status.scheduling.group,POS:.status.scheduling.position,OF:.status.scheduling.length,WHY:.status.scheduling.reason,PENDING:.status.scheduling.pending
```

`group` is the deploy group, `position` the place among the objects that still have pods to dispatch
(0 when all are dispatched), `length` their number, `reason` why the next pod waits (`InFlightLimit`,
`WaitingForGroup` with `message` "waiting for group X", `WaitingForTurn`, `PreparingImages`,
`InsufficientResources`, `NoSchedulableNodes`), `pods`/`pending` the pod counts. A Lab is in phase `Queued`
while none of its pods has been dispatched. The position is refreshed at a limited rate (at most 50 writes per
2 s tick, one per object per 10 s), so in a long queue it lags by a few seconds. Each pod's own state is
`Device.status.scheduling` (`Queued`, `Starting`, `Started`, `Failed`) and, for a group, `LabGroup.status.pods`.

**A worked example.** Window `maxPods: 2`. Three groups and an independent lab: lab `a` (group `g1`, devices
`web`, `db`), lab `b` (group `g2`, `deploy-after: g1`, device `web`), lab `c` (group `g3`, device `web`), lab
`i` (no group, device `web`).

1. All four labs and five devices are created and queued (`Queued`). Group `g1` is first: `a-web` and `a-db`
   take the two slots. `b`, `c`, `i` wait; `b` says `waiting for group g1`.
2. `a-db` is Ready (`Started`): a slot is free. `g1` has no pod left to dispatch, but `g2` waits for `g1`, so
   the next group, `g3`, starts: `c-web` is dispatched. `i` still waits: independent labs go after all grouped
   work that can start.
3. `a-web` and `c-web` are Ready: `g1` is complete, so `b-web` (`g2`) is dispatched, then `i-web`.
4. `b-web` never gets Ready: after 5 minutes it is declared `Failed` (reason `StartupTimeout`, or `ImagePull`
   with the pull error). Its slot is free, `g2` is complete, and the lab shows the warning in
   `status.devices[].failure`.
5. If `b-web` becomes Ready later after all, it is `Started` and the warning clears; nothing else changes.

**Values** (chart `scheduler.*`):

| Value | Default | Meaning |
|---|---|---|
| `enabled` | `true` | `false` starts every pod as soon as its object is created |
| `maxPods` | `20` | pods starting at once; `0` = no limit |
| `startupTimeout` | `5m` | a pod not Ready after this is declared failed |
| `restartThreshold` | `5` | restarts after which a pod that is not Ready is declared failed |
| `platformReservePercent` | `10` | platform reserve: share of the schedulable CPU and memory (0-99) user labs never consume |
| `platformReserveCpu` / `platformReserveMemory` | `"0"` / `"0"` | absolute platform reserve of every schedulable node (quantities) |
| `resourceCheck` | `true` | `false` skips the free-resource check |
| `prepull.enabled` | `true` | prepull the images of a group |
| `prepull.timeout` | `5m` | dispatch goes on after this long |
| (moved) | | the planning profile of a device that declares none is `limits.device.defaultCpu` / `defaultMemory` (`100m` / `256Mi`), see "Limits" |

Objects that existed before the upgrade are never queued: the Device and LabGroup reconcilers record a pod
that already runs as `Started` and leave its workload alone. The operator needs `get/list/watch` on nodes and
`create/delete/get/list/watch` on DaemonSets; the chart's ClusterRole has them. The old `launch.*` values are
gone (no fallback): the launch class, `Lab.spec.launchClass` and `Lab.status.launch` no longer exist.

---

## Workload names and user labels

**Names.** The name of a device's workload never contains the lab. Every container device gets a short
random code (3 characters, 4 after repeated collisions) when it is created, kept in `Device.spec.code`; the
Deployment is named `<device>-<code>` (a pod of it adds the ReplicaSet hash and 5 characters, at most 63 with a
device name of at most 35 characters), a snapshot-backed device's bare pod `<device>-<code>-<incarnation>`, and its
web Service, which is also its host label, `<device>-<code>`. The code is unique in the group namespace (checked
against the other devices' codes and the Services), so two labs may have a device of the same name in one
namespace. The relation between a pod, its device and its lab goes through owner references and the labels
`laboratory.cybericebox.com/lab` and `/device`, never through the name. Devices created before codes existed
keep their names (no migration). The Device resource itself is still named `<lab>-<device>`, as the per-device
env Secret `<lab>-<device>-env`.

**User labels.** The labels of a Lab (and of a LabGroup) are copied onto its Devices and onto the pods of
those Devices (the VPN and gateway pods for a LabGroup), except the operator's own: every label with the prefix
`laboratory.cybericebox.com/` (`deploy-group` among them) and the keys `app`, `pod-template-hash` and
`controller-revision-hash`. They are kept in sync: a changed or removed label follows on the Devices and on live
pods, which are patched in place (metadata only, so nothing restarts); the copied keys are listed in the
annotation `laboratory.cybericebox.com/user-labels` so only they are removed.

---

## 6. Uninstall

```bash
helm uninstall laboratory --namespace laboratory-system
kubectl delete namespace laboratory-system
# CRDs are NOT removed by helm uninstall — remove manually if needed:
kubectl delete crd \
  pools.allocation.cybericebox.com \
  tenants.laboratory.cybericebox.com \
  labgroups.laboratory.cybericebox.com \
  labgroupclients.laboratory.cybericebox.com \
  labgroupaccesspolicies.laboratory.cybericebox.com \
  labs.laboratory.cybericebox.com \
  devices.laboratory.cybericebox.com \
  connections.laboratory.cybericebox.com \
  labvpns.laboratory.cybericebox.com \
  labgateways.laboratory.cybericebox.com \
  labtrafficreports.laboratory.cybericebox.com \
  imagepulls.laboratory.cybericebox.com
kubectl delete namespace laboratory-tenants laboratory-images
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
| Other | `Ping`, `Monitoring` (stream), `GetCapacity`, `GetFeatures`, `PrewarmImages` |

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
naming the item. Internal keys are never exposed: the prefix `laboratory.cybericebox.com/`, Kubernetes system keys (`kubernetes.io`, `k8s.io`
domains), `app` and `pod-template-hash` are rejected when set, stripped from every object in `List*` and in `Monitoring`
(the `labels` maps hold only the caller's labels), and a selector that names one (in `List*`, `Update*`, `Delete*`, the
device calls and `Monitoring`) fails with `INVALID_ARGUMENT`. Labels go onto the custom resource. The labels of a Lab are copied to its Devices and their pods when those are
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
updated): `devices[].persistence {enabled, debounce}` in `spec_json`, both optional, with the
platform defaults from the chart. `enabled: true` is refused when `devices.statePersistence.enabled` is off in the
chart (the agent reads it as `AGENT_STATE_PERSISTENCE_ENABLED`); `debounce` must be positive. The excluded paths and the
quota are cluster settings of the chart (`devices.statePersistence.maxFileSize`, default `256Mi`: bigger files are skipped; `devices.statePersistence.excludePaths`, default `/tmp`, `/var/tmp`, `/run`; and `writeQuota`, default `512Mi`: the most of a
participant's writes kept per device). They are configurable only in the chart and cannot be requested per device.

### Tenancy

The agent serves several clients (tenants) from one cluster and keeps them apart. A tenant is a cluster-scoped
`Tenant` resource; the chart always creates `default` and the tenants listed in its `tenants:` values.

```yaml
tenants:
  platform:                  # the Tenant name = the client certificate CN (a DNS-1123 label)
    persistence:
      allowed: true          # may ask for device persistence (needs devices.statePersistence.enabled)
      writeQuota: 256Mi      # optional, capped by devices.statePersistence.writeQuota (the default)
      maxFileSize: 128Mi     # optional, capped by devices.statePersistence.maxFileSize (the default)
    quota:                   # optional; absent = no limit
      cpu: "50%"             # the sum of the CPU requests of the tenant's pods: "32", "500m" or a percentage
      memory: "40Gi"         #   of what the lab nodes (labWorkloads selector and tolerations) allocate
```

Quote a quota value ("32", "50%"): it is a string. `default` may be listed to change its policy; unless it is, it may use
persistence and has no quota.

#### Tenant images

A tenant's images are pulled with the **tenant's** registry credentials, never the platform's, and the tenant can be limited to
the registries it is meant to use:

```yaml
tenants:
  acme:
    images:
      pullSecret: acme-registry   # kubernetes.io/dockerconfigjson Secret in laboratory-tenants (create it there yourself)
      allow:                      # optional; "registry/repository-prefix", equal or below
        - ghcr.io/acme/
        - docker.io/library
images:
  tenantDeny:                     # platform-wide, every tenant: the platform's private registries and organizations
    - ghcr.io/cybericebox-platform/
```

- **Device pods.** The platform's `imagePullSecrets` go only on the platform's own pods (operator, node-agent, agent, proxy, VPN and
  gateway). Device pods and the prepull never get them. A tenant with `images.pullSecret` has that Secret copied by the operator
  into each of its group namespaces as `tenant-registry` and set on its device pods; a tenant without one pulls anonymously.
  Adding or removing the secret later changes the pod template of the tenant's devices on their next reconcile (a running device
  is restarted by its Deployment). A missing or non-registry Secret keeps the group from reconciling, with the reason in the
  operator log.
- **Image cache.** zot's on-demand sync is one credential set per upstream registry (`registry.cache.credentialsSecret` or the chart's
  `imagePullSecrets`), so it cannot be split by tenant. It is for the **platform's** images and for public images. A tenant with
  `images.pullSecret` therefore does **not** use the shared cache: its labs are created with `Lab.status.imageCache: false`, nodes
  pull straight from its registries with its credentials, and `PrewarmImages` answers `SKIPPED` for it. A tenant without its own
  credentials uses the cache, and zot fetches its images from upstream with whatever credentials the platform gave zot; keep the
  platform's private registries out of reach with `images.tenantDeny` (and the tenant's `allow`), or give zot no credentials for
  them.
- **Allow and deny lists.** The management agent enforces them on `CreateLabs` (`PERMISSION_DENIED`, naming the device) and
  `PrewarmImages` (the image is `FAILED` and never fetched; the list of warmed images an empty request returns shows only what the
  tenant may use). Entries are `registry/repository-prefix`: a reference is covered when it equals an entry or lies below it, on
  path boundaries (`ghcr.io/acme` covers `ghcr.io/acme/web`, not `ghcr.io/acme-private/web`); a Docker Hub image is
  `docker.io/library/nginx`. A tenant with no `allow` may use any image `tenantDeny` does not exclude. `tenantDeny` always wins.
  A reference that names the platform's registry or image cache (`localhost:<registry.forwardPort>/...`) is refused outright: a
  tenant names the original image and the operator routes it. Labs created directly with kubectl are not checked.

- **Identity and certificate.** The tenant of a call is the CN of the verified client certificate, and the CN is the Tenant's name.
  The tenant obtains its certificate by enrolling with a one-time token (see [Enrollment & access keys](#enrollment--access-keys)): its
  private key stays with it. A certificate whose CN is not a Tenant is `PERMISSION_DENIED` on every call (the Tenant resources are the
  only allowlist). A call without a client certificate (TLS or mTLS off, local development) is the tenant `default`.
- **Stamp.** Every object the agent creates (LabGroups, Labs, VPN clients, access policies) gets the reserved label
  `laboratory.cybericebox.com/tenant`. The operator copies it to the Devices and pods of a Lab and the pods of a
  LabGroup. Like every reserved label it is hidden: never in answers, never accepted from a client, never allowed in a selector.
  Objects without the label (created before tenancy) belong to the `default` tenant.
- **Scope.** Every RPC is implicitly scoped to the caller's tenant: `List*`, `Update*`, `Delete*`, the device calls and `Monitoring`
  see only its objects, combined with the caller's own selector (which can narrow the scope but never widen it). Objects inside a
  LabGroup belong to the tenant of the group. Another tenant's object is `NOT_FOUND` for every operation, as if it did not exist.
  LabGroup ids are global: creating an id that belongs to another tenant fails for that item with "the id is not available", with
  no hint that it exists or is being deleted.
- **Persistence policy.** A topology may ask for `devices[].persistence.enabled` only if the platform allows persistence and
  the tenant's `persistence.allowed` is true; otherwise the agent refuses it (`INVALID_ARGUMENT`). The write quota and the
  maximum file size of a device are the tenant's, capped by the chart values (a tenant without its own gets the chart's), and are
  stamped on the Device when it is created, like the persistence choice itself: changing a Tenant later affects new devices only.
- **Group overhead.** Every LabGroup runs a VPN pod and an internet gateway pod of its own. Their resources are chart values
  (`vpn.resources`, `inetGateway.resources`: `cpu` and `memory`, requests = limits, so the pods are Guaranteed). The defaults are
  `100m`/`320Mi` for the VPN and `10m`/`32Mi` for the gateway. Measured on the local stand (kubectl top every 10 s): an idle VPN pod uses
  1m CPU (p95 4m) and 16Mi; with ten active peers pulling pages in a loop it peaks at 55m CPU and 195Mi, so 320Mi leaves about half as headroom; an idle gateway uses 1m and 8Mi. The data path is the kernel's WireGuard and the pod only manages peers and firewall rules. The operator
  applies them when it CREATES a group; existing groups keep what they have (changing a value never restarts a live VPN). `GetCapacity`
  reports their sum as `group_overhead_cpu_millicores` and `group_overhead_memory_bytes`: what one group adds to its labs, for sizing a reservation.
- **Resource quota.** The scheduler caps the sum of the CPU and memory requests of the tenant's dispatched pods (started, starting
  or failed). A pod that would pass the cap waits, the lab's `scheduling.reason` is `TenantQuota`, and other tenants' pods go on.
  A percentage is a percentage of the CPU and memory the lab nodes allocate.
- **Capacity.** `GetCapacity` and the `capacity` of the `Monitoring` stream are the caller's view only: its quota (if any), what its
  pods reserve (the sum of their requests) and use (metrics-server, when installed), and what is free (quota minus reserved). No
  cluster-wide numbers are exposed. The same reserved and used totals are in `Tenant.status` (refreshed by the agent):
  `kubectl get tenant platform -o yaml`.
- **Features.** Everything the laboratory owns that the backend needs reaches it only through the agent: `GetFeatures` and the
  `features` of the `Monitoring` stream (sent with the first message and again when they change; a change shows at the next
  heartbeat at the latest, so the backend needs no polling). The answer is the caller's tenant view:
  `state_persistence` (`available` = the cluster enables it AND the Tenant is allowed; the default debounce; the write quota and
  maximum file size, the tenant's limit capped by the cluster's; the excluded paths), `image_cache` (enabled, registries),
  `scheduler` (enabled, `max_pods`), `endpoints` (the labs domain, the VPN endpoint), `proxy` (`access_token_max_ttl_seconds`: the longest exp - iat of a handoff link the proxy accepts, 60s; `session_idle_ttl_seconds`: the session expires after this much inactivity, 24h; `session_max_ttl_seconds`: the absolute cap from the handoff, 168h; chart `proxy.l7.accessTokenMaxTTL`, `sessionIdleTTL`, `sessionMaxTTL`) and `certificate` (`not_after_unix` of the client
  certificate the call came with, `issued_ttl_seconds` of new ones: the backend schedules `RenewCertificate` from the expiry, and the
  expiry changes only when it reconnects with the renewed certificate). Quotas and the group overhead stay in `GetCapacity`. The agent
  gets the cluster values from the same chart keys as the operator (`devices.statePersistence.*`, `scheduler.enabled/maxPods`,
  `registry.cache`, `operator.baseDomain`, `operator.publicVPNEndpoint`, `agent.enrollment.certificateTTL`).
- **Limits.** `limits` in the features (`device`, `lab`, `group`, `tenant`; 0 = no limit) is a sanity ceiling, not a sizing profile, and `CreateLabs`
  enforces it: `limits.device.maxCpu` (2000m) and `maxMemory` (4Gi) per device; `limits.lab.maxDevices` (20 container devices; switches and
  hubs do not count); per LabGroup `limits.group.maxLabs` (50) and the optional sums over all its labs `limits.group.maxCpu` / `maxMemory`
  (`"0"` = unlimited); `limits.tenant.maxLabs` (0 = unlimited; the tenant's resource quota still applies). There is no per-lab resource
  cap. Devices are Guaranteed (request = limit, no CPU overcommit), so a group's sum is what it reserves. A device's resources are its
  limit, else its request, else the planning profile `limits.device.defaultCpu` (100m) / `defaultMemory` (256Mi), which is also what the
  scheduler gives a device without resources. A variant over a device or lab cap is `InvalidArgument` naming the variant, the device and both
  numbers. Over a group or tenant cap the new items are `FAILED` (`ResourceExhausted` reason, not retryable); an item whose lab exists is
  not new. The counts are checked when the call arrives. Labs created directly with kubectl are not checked.
- **Monitoring** is cut to the tenant before the user selector.

### Required values and defaults

The operator takes every setting from the environment the chart gives it and has no code default for what is deployment-specific:
`operator.publicVPNEndpoint`, `operator.baseDomain` and `operator.supportEmail` are required (the render fails when one is empty); the images
of the VPN, gateway and netconfig pods (`vpn.image`, `inetGateway.image`, `nodeAgent.image`: repository and tag, the tag is the chart
appVersion unless set) are always passed, and the operator refuses to start without `VPN_IMAGE`, `GATEWAY_IMAGE`, `NETCONFIG_IMAGE`
and `SUPPORT_EMAIL`. Where a code default remains it mirrors the value in `values.yaml` (for example `registry.cache.pinTTL` 30m,
`proxy.l7.listen` `:8443`, `nodeAgent.criSocket` `/run/k0s/containerd.sock`). The ACME directory of the issuer is `certManager.acme.server`
(empty: the Let's Encrypt preset of `certManager.acme.presets` chosen by `certManager.staging`).

Also explicit values (each default mirrors the code default): `operator.groupNetworkPolicy.enabled` (true, the default-deny baseline of every
group namespace; `operator.networkPolicy.enabled` is the policy of the operator pod itself), `vpn.statsInterval` (30s, `STATS_INTERVAL` of the
VPN pod of a new group), `devices.statePersistence.retentionInterval` (10m), `agent.id` (`laboratory-agent`),
`agent.tenantStatusInterval` (30s) and `nodeAgent.ovsBridge` (`br-ovs`).

### Enrollment & access keys

A tenant's private keys never leave it. The platform (the tenant's backend) generates its own client key and access key
pair, and sends the cluster only public material: a certificate request and the access public key.

1. **The token.** When a `Tenant` exists, the operator generates a random one-time enrollment token. Only its SHA-256 and
   expiry (`agent.enrollment.tokenTTL`, 24h) go to `Tenant.status.enrollment`; the token itself is shown once, in the Secret
   `tenant-<name>-enrollment` of the namespace `laboratory-tenants`. The admin reads it and hands it to the tenant:

   ```bash
   kubectl -n laboratory-tenants get secret tenant-platform-enrollment -o jsonpath='{.data.token}' | base64 -d
   ```

   When the token is used, the Secret is removed. An expired token disappears the same way. For a new token annotate the Tenant
   (the operator makes it, replaces the Secret and removes the annotation):

   ```bash
   kubectl annotate tenant platform laboratory.cybericebox.com/regenerate-enrollment-token=true
   ```

2. **Enroll** (agent RPC over plain TLS, server authentication only: the client has no certificate yet). The request is
   `{token, csr_pem, access_public_key_pem, access_key_id}`. The agent verifies the token (hash, expiry, unused) and the request
   (a valid signature; EC P-256 or stronger, or RSA 2048 or more; the requested subject is ignored), signs a client
   certificate with CN = the tenant name from the client CA (`agent.enrollment.certificateTTL`, 30 days, never past the CA), stores the
   access public key and burns the token. The answer is `{certificate_pem, chain_pem, not_after_unix}`: **tenant identity = the client certificate CN** (the tenant name, no prefix, a DNS label); there is no separate tenant field, a client reads the CN from the certificate. A used, expired
   or unknown token is `PERMISSION_DENIED` with one and the same message. Access keys are Ed25519 (PKIX PEM); an id is 1 to 64 characters
   of `A-Z a-z 0-9 . _ -`.
3. **Renewal and rotation** (mTLS, as the tenant). `RenewCertificate{csr_pem}` issues a new certificate with the same CN for a
   new key. `RotateAccessKey{public_key_pem, key_id}` adds a key (a tenant keeps at most 10); `RemoveAccessKey{key_id}` removes one,
   never the last. A rotation overlaps: add the new key, switch the backend to it, remove the old one.
4. **Where the keys are.** The agent keeps them in the Secret `tenant-<name>-access-keys` of `laboratory-tenants` (one entry per key id,
   the PEM public key); the proxy reads them from there (it watches that namespace and nothing else).
5. **Handoff links.** The proxy accepts a lab access link only if it is a JWT signed with EdDSA by an access key of the tenant it
   names: `iss` = the tenant, the header `kid` = the key id, `aud` = `laboratory-proxy`, `sub` = the LabGroupClient, `iat`, `nbf` and `exp`
   (at most 5 minutes apart), plus `group_id`, `host` (the `<device>-<code>` label) and `sess` (the end of the session, unix time). An unknown
   issuer or key id, another audience, an expired or not yet valid link is refused. The group must belong to the issuer (the tenant
   label of the LabGroup): a tenant cannot open another's labs, even with a perfect signature, and a session stops when its group
   changes owner. There is no shared lab access key any more.
6. **Without enrollment.** Setting `agent.tenantCertificates.enabled` makes cert-manager issue a client certificate (and key) per tenant into
   the Secret `laboratory-agent-client-<name>-tls`. This is a manual fallback, off by default; the access key must then be
   registered some other way (the agent only writes it through `Enroll` and `RotateAccessKey`).

The agent accepts a connection without a client certificate and requires one for every call except `Enroll`. The CA key
(`laboratory-agent-ca`) is mounted into the agent for signing.

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

## Security hardening

Findings of the isolation audit (`docs/security/2026-10-02-laboratory-isolation.md`) and how the laboratory answers them.

### Layer 2 between teams: fail-secure Open vSwitch

The bridge `br-ovs` is `fail_mode=secure`, and table 0 ends in `priority=0 actions=drop` (the standalone default, a `NORMAL`
learning switch over every port of the bridge, would bridge the ports of different teams). A device port forwards only after its own
table-0 flow (`in_port` to the VNI) is installed, so a port that has no flow, for whatever reason, reaches nobody:

- **CNI ADD to reconcile.** The port exists before the connection reconciler binds it; until then its frames are dropped.
- **Node-agent restart.** At start the node-agent clears tables 0 and 6, installs the default drop, and the reconcilers bring the flows of
  the live ports back within seconds. L2 of the lab devices on that node is interrupted for those seconds (by design: fail-secure).
- **OVS restart.** The flows are not persistent; the bridge is secure, so nothing is forwarded until they are reinstalled.
- **Rebinding a port.** The table-0 flow is replaced in place (an `OFPFC_ADD` with the same match and priority replaces), never deleted
  and added again, so there is no gap.
- **A device interface without a Connection** never gets a flow and stays isolated.
- **Stale flows.** When a refresh of the port map shows that an OpenFlow port number is gone (or now belongs to another port), the
  table-0 flow of that number and every flow that outputs to it (table 6 flood) are deleted before the number can be reused.

Check on a node: `ovs-vsctl get bridge br-ovs fail_mode` is `secure`, and `ovs-ofctl -O OpenFlow13 dump-flows br-ovs table=0` has no
`NORMAL` action and ends with `priority=0 actions=drop`.

### Revoking access cuts live sessions

FORWARD in the VPN pod accepts established connections before the access chain, so replacing the chain stops only new connections.
After every rewrite of the chain (a policy change, a client added, removed or given a new address, a lab becoming ready or not) the VPN pod
therefore deletes, through ctnetlink, every tracked connection that a client opened to a lab network and that no allow rule covers.
The next packet of such a session is a new connection, which the chain refuses; an open SSH session, reverse shell or download ends within
the reconcile (a second or two). A lab that stops being ready also loses its open sessions. A failed delete is retried by the next reconcile.

The L7 proxy checks access once per request, so a WebSocket (a web terminal) or a long download would also outlive a lock. The proxy
therefore keeps every request in flight and, every `proxy.l7.liveCheckInterval` (10s), runs the same checks again (the group still belongs
to the tenant that issued the session; the client still exists and the group policy allows it the lab) and closes the ones that fail: a plain
request is cancelled, an upgraded connection is closed. A lock takes effect within about the check interval plus the proxy's cache lag.
No connection lives past the session's absolute end or `proxy.l7.liveMaxLifetime` (12h).

### Device interface names and MACs

A device's `interfaces[].name` is a lowercase word of at most 15 characters (`^[a-z][a-z0-9-]{0,14}$`; `lo` and `accessport`
are reserved: `accessport` is the delegated Kubernetes network of a web-exposed device). `eth0` is allowed: a device with lab
interfaces has no real `eth0`, the CNI leaves a stub the lab interface replaces. `interfaces[].mac` is `random` or a **unicast**
hardware address (`aa:bb:cc:dd:ee:ff`; the second hex digit of the first octet is even; not all zeros). A multicast or broadcast MAC
is refused. The same rules are in the CRDs (`Lab`, `Device`: a client that bypasses the agent is refused by the API server) and in the
agent (`CreateLabs` answers `INVALID_ARGUMENT` before anything is created); the operator and the node-agent drop an entry that
still gets through.

The `network.cybericebox.com/networks` pod annotation that carries them to the node-agent is a JSON array
(`[{"iface":"eth1","mac":"02:..."}]`; lab VPN and gateway ports add `"name":"<ovs port>"`), so no character of a name can start another
entry. The node-agent still reads the old `iface@name|MAC,...` form, so pods created before an upgrade keep working. **Upgrade the
node-agents before the operator** (the DaemonSet first): an old node-agent cannot read the JSON form.

### What a lab can reach through its internet gateway

A lab with `internet.enabled` leaves through the gateway pod of its group, so the lab inherits whatever the gateway may reach.
The gateway forwards to the **public internet only**:

- The denied ranges are **a constant of the code** (`internal/egress`), not a setting, and there is no allow-list exception: a lab needs no
  internal service and must not learn where it runs. They are: `10/8`, `172.16/12`, `192.168/16` (so the node, VPC, pod and service networks of a
  typical cluster, and an internal load balancer of the control plane), CGNAT `100.64/10`, link-local `169.254/16` (the cloud metadata service),
  loopback `127/8`, `0/8`, `192.0.0/24`, `198.18/15`, and multicast and reserved `224/3`; and the IPv6 equivalents (`::/128`, `::1/128`,
  IPv4-mapped and NAT64, `fc00::/7`, `fe80::/10`, `ff00::/8`, discard and documentation). Public addresses of nodes are not listed: protect them with
  the cloud firewall (security groups).
- Its iptables chain `LABEGRESS` (first in FORWARD for traffic leaving the pod) drops what a lab sends to those ranges (IPv6 through ip6tables
  where the pod has it).
- Its `CiliumNetworkPolicy` allows egress to the world **except** the same ranges (`toCIDRSet` with `except`, for `0.0.0.0/0` and `::/0`), and
  to the API server entity on ports 6443 and 443 only (the gateway's own reconciler; the entity covers every port of a node the API server
  runs on, so the ports are what limits it). The VPN pod's policy has the same port limit. The labs themselves can reach the API server
  neither directly (default-deny) nor through the gateway (the filter above).
- A gateway that already runs is restarted by the operator with the new image; the old `GATEWAY_EGRESS_*` environment variables are removed from
  it. Verify from a lab device with `internet.enabled`: the metadata address, a node address and the API server are unreachable, a public address is
  reachable.

### Hardening of the lab pods

| Pod | Token | Seccomp | Capabilities (everything else dropped) | Other |
|---|---|---|---|---|
| Device | never | RuntimeDefault | the base set plus the profile's (see below) and, for an in-image DHCP client, NET_ADMIN and NET_RAW | no service links; `ping_group_range` sysctl; `ephemeralStorage` limit; `hostUsers: false` by default |
| Device `netconfig` init | never | RuntimeDefault | NET_ADMIN, NET_RAW | no privilege escalation |
| VPN | kept (talks to the API) | RuntimeDefault | NET_ADMIN, NET_RAW | no privilege escalation, **no privileged init container** |
| Gateway | kept | RuntimeDefault | NET_ADMIN, NET_RAW, NET_BIND_SERVICE (its DHCP server binds port 67) | no privilege escalation |

- **Device security profiles** are a fixed catalog in the code (`internal/profiles`), each with a stable ID. The settings only say which catalog profiles
  are enabled (`devices.security.profiles`, default both); the agent reports the enabled IDs (`device_profiles` in the features) and refuses a lab whose
  device asks for another one (`InvalidArgument`, naming the device). The spec field is still `securityPreset`; an empty value is `standard`, and the old names
  are aliases: `basic` and `service` mean `standard`, `net` and `debug` mean `extended`. Never in any profile: SYS_ADMIN, SYS_MODULE, SYS_RAWIO,
  SYS_TIME, SYS_BOOT, DAC_READ_SEARCH, BPF, PERFMON, SYSLOG, AUDIT_CONTROL, MAC_ADMIN, MAC_OVERRIDE, MKNOD, privileged, host network, PID and IPC, hostPath.

  | ID | Adds to the base set | Covers |
  |---|---|---|
  | `standard` (default) | SYS_PTRACE, IPC_LOCK, LINUX_IMMUTABLE; ping through `net.ipv4.ping_group_range` | web, API, databases, mail, DNS, LDAP, SSH, FTP, Samba, privilege escalation (sudo, SUID, cron, capabilities), cracking, crypto, forensics, gdb and strace, `chattr`, mlock, ping, nmap connect scan, noVNC desktop |
  | `extended` | standard plus NET_RAW, NET_ADMIN and `/dev/net/tun` | nmap SYN and OS scan, tcpdump, scapy, ARP spoofing, MITM, Responder, routers and firewalls, DHCP server, WireGuard, OpenVPN, tun pivoting, VLAN, GRE, VXLAN, IPsec, Linux bridge, FRR |

  The base set of every device is AUDIT_WRITE, CHOWN, DAC_OVERRIDE, FOWNER, FSETID, KILL, NET_BIND_SERVICE, SETGID, SETPCAP, SETUID, SYS_CHROOT; all other
  capabilities are dropped. Privilege escalation stays allowed in a device on purpose: lab images run `sudo` and setuid binaries.
- **`/dev/net/tun`.** The node-agent is a kubelet device plugin and advertises the extended resource `cybericebox.com/tun` (`nodeAgent.devicePlugin`: the
  kubelet's device-plugins directory, always `/var/lib/kubelet/device-plugins` (the kubelet hardcodes it, on k0s too, whatever its root-dir is), and `tunSlots`, 1000 per node). A device pod with the `extended` profile
  requests one (request = limit) and the kubelet passes `/dev/net/tun` and nothing else from the host; no extra daemon, no hostPath. The host needs the `tun`
  module loaded at boot (infrastructure); without the device the slots are advertised unhealthy and such pods stay Pending. The plugin checks that the device exists on the **host** through the kernel's sysfs entry `/sys/class/misc/tun/dev` (`TUN_CHECK_PATH`; present exactly when the tun module is loaded; the container's own `/dev` has no tun even when the host does, and sysfs needs no extra mount). The plugin is optional by construction: the node-agent is also the CNI, so the device-plugins host path is `DirectoryOrCreate` and a failure in the plugin (no directory, no kubelet socket, no registration) is only logged and retried every 30 s, never blocking the pod. Its log lines (`device plugin: ...`) say when it waits for the kubelet, registers, advertises, and when the health of the host device changes.
- **VPN conntrack accounting.** The switches `nf_conntrack_acct` and `nf_conntrack_timestamp` need a writable `/proc/sys`, which an unprivileged
  container does not have. The VPN pod carries the annotation `network.cybericebox.com/conntrack-accounting: "true"` and the node-agent sets them in the
  pod's network namespace when it wires the pod (CNI ADD). If that fails the flow collector still counts attempts and replies, only bytes stay zero.
- **Existing groups.** The operator brings the VPN and gateway Deployments that already run to this shape (their pods restart once, WireGuard clients
  reconnect within the keepalive). Device pods of labs that already run keep what they were created with; new labs get the hardening.
- **User namespaces** (`devices.security.userNamespaces`, a hidden setting, default `true`): device pods run with `hostUsers: false`, so root in a device is
  not root on the node. Needs Kubernetes 1.33+, containerd 2 and kernel 6.3+. Check on the cluster that device networking (the veth is moved into the
  pod namespace by the node-agent), state snapshots, `sudo` and setuid binaries, and images with UIDs above 65535 still work.
- **Ephemeral storage** (`devices.security.ephemeralStorage`, 2Gi): the writable layer, logs and emptyDirs of one device. Without a limit a device can fill
  the node's disk and DiskPressure evicts other pods. The PID limit is a node setting: set the kubelet's `podPidsLimit` (`--pod-max-pids`) in the
  cluster configuration (not a pod field).
- Not done here: a sandbox RuntimeClass (gVisor, Kata) for hostile images, and Pod Security Admission on the group namespaces (the lab pods need NET_ADMIN,
  which `baseline` allows, but the `net` preset adds more; the admission policy below is the guard that matters for the operator).

### Namespaces of the lab groups

The namespace of a **new** LabGroup is `lg-<name, at most 40 characters>-<8 hex of the SHA-256 of the whole name>` (at most 52 characters), never the bare group
name. The group name is chosen by a tenant, so without the prefix it could equal an existing namespace; with it, it never can (the CRD also refuses the
names of the system namespaces outright). On top of that the operator **never adopts or deletes a namespace without the group's label**
`laboratory.cybericebox.com/group=<group>`: a namespace that exists under the name a group would get, without that label, is refused (the group stays
unprovisioned) and, when the group is deleted, left exactly as it is. The operator puts the label on every namespace it creates.

Migration: the namespace is recorded in `LabGroup.status.namespace`, and everything (the operator, the demux, the L7 proxy, the agent) uses that. A group created
before the prefix keeps the namespace it has (its bare name, with the label the operator always set) and works as before; only new groups get the prefixed one.
Nothing is renamed. Scripts that derive the namespace from the group name must read `status.namespace` instead.

### The L7 proxy under hostile clients

- **HTTP server limits** (`proxy.l7.readHeaderTimeout` 10s, `readTimeout` 5m, `idleTimeout` 2m, `maxHeaderBytes` 65536) apply before any routing or
  authentication: a client that dribbles its headers or body, or opens connections and says nothing, is cut instead of holding a connection forever (an
  unauthenticated slowloris could otherwise exhaust the proxy). There is no write timeout, because responses stream; an upgraded (WebSocket) connection has
  its deadlines cleared and lives at most `LIVE_MAX_LIFETIME` (12h), with the access policy re-checked every 10 s.
- **The handoff link** is valid for 60 s at most (`proxy.l7.accessTokenMaxTTL`; the backend's token lifetime must not be longer). It is a bearer link that the
  proxy cannot make single-use, because its replicas share no memory; the short life is the control.
- **The session cookie belongs to the proxy.** Any `Set-Cookie` of the session cookie's name in a device's response is removed, so a device cannot set,
  replace or clear it; the device's own cookies pass.

### The WireGuard demux under hostile senders

The demux (`wg-demux`) reads one public UDP port for every team. It holds no keys (the VPN pods do; WireGuard's own handshake is the security boundary), so
everything a stranger can make it hold or spend is capped (`proxy.wg.limits`):

- **No per-packet logs.** Nothing is logged for an incoming datagram (a flood could otherwise fill the log pipeline).
- **Table caps:** `maxEntries` conntrack entries in all (two per session), `maxEntriesPerSource` for one client address (a team behind one NAT needs two per VPN
  client). A handshake that would pass either is dropped and the client retries.
- **Rates, per source address:** `handshakeRate`/`handshakeBurst` for handshake initiations (each costs a scan of the groups' mac1 keys), and
  `missRate`/`missBurst` for packets that match no session or come from an address the session does not expect, so guessing the 32-bit session index is
  rate-limited. `maxSources` bounds the memory of these limiters.
- **Roaming works as before**, but one session changes address at most once per `roamInterval`; a packet from a third address inside the interval is dropped and
  moves nothing. An index a live session owns is never taken by another handshake (the handshake is dropped).
- **Type 2** (the server's answer) is accepted only from the VPN pod the init was forwarded to; **type 3** (cookie reply, the peer's own load protection) is
  forwarded, only from the exact address the session expects, and never moves the session.

One overloaded VPN pod only hurts its own team: the demux caps are shared, the pods are not.

### Revoking client certificates: the enrollment epoch

A client certificate cannot be revoked one by one, so the agent keeps an **enrollment epoch** per tenant: `Tenant.status.certificateEpoch`, a number that
moves by one at every enrollment. A certificate carries the epoch it was issued in and the UID of the Tenant (a private extension), and works only while both
equal the Tenant's now: an exact comparison, no clock. Consequences:

- **Enrolling again revokes every certificate issued before** (a leaked one included), even one issued a millisecond earlier: give the tenant a new token (see
  "Enrollment & access keys") and enroll. A revoked certificate also cannot be used to renew itself. Renewal is limited to once per
  `agent.server.renewMinInterval` (10 s) per tenant, and a renewed certificate is in the epoch it was read in, so one renewed just before an enrollment dies with it.
- **Enrolling replaces the tenant's access keys** with the one in the request, so a key that the holder of a revoked certificate added does not outlive the
  revocation, and a tenant whose ten slots were filled can always enroll again.
- **The default tenant is revocable like any other**: with mTLS on every certificate needs its Tenant object, `default` included (the chart creates it), and an
  empty common name is refused.
- **A tenant deleted and created again under the same name** has another UID: the certificates of the old tenant stop working.
- **Certificates issued before the epoch was a number** (they carry no epoch) keep working while the tenant's epoch is 0, judged by time as before (issued after the
  Tenant was created and after `status.certificatesNotBefore`). The first enrollment by this version moves the epoch above 0 and revokes them all; renewing gives a
  numbered one. Nothing has to be re-enrolled on upgrade.
- **The CRD has to be new** (`kubectl apply --server-side -f charts/laboratory/crds/`): an API server that does not know `status.certificateEpoch` drops it, and
  `Enroll` then fails loudly (after burning the token) with a message that says so.
- **Checked before the token is spent**: a request that is bound to fail (no CA to sign with, a key id of `.` or `..`) is refused without burning the token. The
  token is burnt before the certificate is signed (a conditional write: of any number of requests with the same unused token exactly one wins); if signing then
  fails, the token is spent and the admin issues a new one.
- **Streams** (Monitoring, snapshot export) are authorized again every `agent.server.streamRecheck` (30 s): a stream whose certificate was revoked or has
  expired is cut and the caller gets the reason. Connections are replaced after `agent.server.maxConnectionAge` (1 h).

### The agent under hostile callers

The agent is reachable by anyone who can reach its host, so what a caller without a certificate can cost is bounded before anything is decoded:

- **Two servers behind one port.** The agent completes the TLS handshake itself and sends a connection that presented a client certificate to the main
  server (full API, messages up to 64 MiB), and one that did not to the **Enroll server**, which knows only the Enroll call, reads messages of at most
  `agent.server.enroll.maxMessageBytes` (64 KiB), and is rate limited (`rate` 5 per second, `burst` 10, all callers together). Nothing a caller without a
  certificate sends reaches the main server or is buffered at 64 MiB. A caller that already has a certificate enrolls through a connection without one (the
  client library's connection without a keypair).
- **Connections**: the TLS handshake has a timeout and at most `maxHandshakes` run at once; at most `maxAnonymousConns` connections without a certificate are open
  and `anonymousConnsPerIP` from one address; one address opens at most `newConnRate` connections per second. Behind a TLS passthrough route the address seen is
  the gateway's, so the per-address limits then act as global ones: raise them if enrollments queue up.
- **Streams and keepalive**: `maxConcurrentStreams` (64) per connection, a connection ends after `maxConnectionAge` with `maxConnectionAgeGrace`, and a client
  that pings more often than `keepaliveMinTime` (10 s) is disconnected.
- **The token is found by a label.** The operator labels a Tenant with the start of its unused token's hash (`laboratory.cybericebox.com/enrollment-token`), so an
  Enroll costs one narrow, cached list instead of a read of every Tenant. A token issued before the label existed gets it when the operator next reconciles the Tenant
  (at its start): until then that token is not found, and the admin can ask for a new one.
- **Two replicas** (`agent.replicas`, default 2) spread over nodes with a PodDisruptionBudget (`minAvailable: 1`): an agent that is restarted does not take every
  tenant down. The Monitoring journal and the prewarm progress are per replica.
- **mTLS off is development only**: `agent.mtls.enabled=false` makes every caller the default tenant, and both the chart (`agent.allowInsecure=true`) and the agent
  (`AGENT_ALLOW_INSECURE=true`) refuse it unless it is asked for by name.
- **Upgrade order.** Apply the CRDs first. Then upgrade the chart: the operator labels the tokens and the agents roll one at a time. Clients keep their
  certificates; a client that enrolls during the roll talks to either version, and an old agent ignores the label it does not know. If an agent rolls before the operator
  has labelled an unused token, that token is not found until the operator has (a minute): retry.

### Device state under abuse

A participant is root in the device and controls what its writable layer holds, so the snapshot engine bounds what a layer can cost:

- **Entries.** The byte quotas count only the bytes of regular files, so `devices.statePersistence.maxEntries` (100000) caps the entries (files,
  directories, links) of one snapshot layer and of a squashed chain (the merge keeps a map of every path in memory and stops at the cap). Over it the last good
  snapshot is kept and the status warns, like the byte quota. An entry whose names and attributes pass 8 KiB is left out of the snapshot.
- **No device nodes.** Character and block devices and named pipes are never kept in a snapshot.
- **Watching.** The node-agent watches at most `devices.statePersistence.maxWatchedDirs` (2000) directories of one layer with inotify (the kernel's watch limit
  is shared by everything on the node); past it the layer is only polled (about every 30 s), so a change is noticed later but nothing else on the node starves.
- **The registry is shared.** `devices.statePersistence.tenantQuota` (10Gi, `"0"` = none) caps the snapshots of ALL one tenant's devices together (the sizes
  recorded in their status), so one tenant cannot fill the volume for everyone; `Tenant.spec.persistence.registryQuota` gives a tenant less. A snapshot that
  would pass it is refused like one over the write quota. The agent reports the tenant's `registry_quota_bytes` and `max_entries` in the features.

### Upgrade note: zot's data directory

zot runs as the unprivileged user 65532. `fsGroup` makes a volume writable for it only when the storage provisioner honours it; a hostPath or
local-path volume, or one that an earlier version filled as root, keeps its owner and zot fails with `open /var/lib/registry/cache.db: permission denied`.
The registry pod therefore has one init container, `own-data`, that runs `chown -R 65532:65532 /var/lib/registry` as root with only the CHOWN, DAC_OVERRIDE
and FOWNER capabilities and a read-only root file system, and does nothing else. It runs on every start (a no-op once the volume is owned), so an upgrade needs
no manual step; on a large volume the first start takes as long as the chown.

### Who may read the registry

zot is shared by every team and the image cache, so what it serves is split by repository (no catch-all pattern: a repository that matches none is
denied to everyone but the writer):

- **The public image cache** (`docker.io/**`, `ghcr.io/**`, `quay.io/**`, `registry.k8s.io/**`, and the extra registries you list): anonymous read.
- **The snapshots of the labs (`lab/**`) and the shared `base` repository: not anonymous.** The `reader` account may read them, the `writer` account
  everything. Both live in the Secret `laboratory-registry` (generated once and kept across upgrades; a registry made by an earlier version gets a reader added,
  the writer stays). The node-agent forwarder (`127.0.0.1:<forwardPort>`) adds the reader to the node runtime's **GET and HEAD** requests of those repositories
  that carry no credentials of their own, so pulling a snapshot needs no host configuration; writes are never given the reader. The agent exports a snapshot
  with the reader too (the Secret `laboratory-registry-reader` in the agent namespace).
- The network policy still lets the node-agents and the platform pods reach zot; the accounts are what keeps another team's snapshots (which can hold
  solutions) from any other process that can reach it. A process on a node can still reach the forwarder on that node's loopback: the node-agent is the
  trusted path.

### The laboratory's error journal

`MonitoringUpdate.errors` (an `ErrorJournal`) carries what broke in the laboratory since the last message of the stream (the first message covers the last ten
minutes), for the platform's error journal (see `docs/specs/error-journal.md`):

- **`components`**: per component instance (`operator`, `node-agent` per node, `l7-proxy`, `wg-demux`, `vpn`, `gateway`, `agent`) the error groups since the
  last message: a stable `fingerprint` (component, error path, normalized text), the `kind` (the first name of the logger: `reconcile`, `ovs`, `registry`, ...),
  the `normalized` message (numbers, ids, names, hosts and addresses replaced), the `count`, first and last time, and up to three recent `samples`. Only for a
  tenant with `Tenant.spec.receivesLabErrors` (chart `tenants.<name>.receivesLabErrors: true`: set it for the platform's own backend, not for a customer).
- **`deploy_failures`**: the failed deploys of the **subscriber's own** labs, with a reason code (`ImagePull`, `CrashLoop`, `Unschedulable`, `StartupTimeout`,
  `DoesNotFit` for devices and the group's VPN and gateway pods; `LabFailed` and `LabError` for a lab in that phase without a device to blame), the group and lab ids, and a
  redacted message. Each failure is sent once, and again if its reason changes.
- **`client_cert_not_after_unix`**: the expiry of the connection's client certificate, with the first message, when it changes and about hourly.

**How it is collected, without log access.** Every component counts its error-level log lines (the agent its error lines) in memory by fingerprint and publishes the
groups that changed, about every 30 s, as a Kubernetes Event (`reason: ErrorJournal`, label `cybericebox.com/error-journal`, one event per fingerprint, updated in place;
the API server keeps them for an hour). The operator, the node-agents and the proxy publish into the release namespace, the VPN and gateway pods into their group
namespace. The agent reads the release namespace (a read-only Role `laboratory-agent-events`) and every group namespace (the events permission it already has there),
turns the cumulative totals into deltas, and keeps a bounded ring. The permissions added for this: the agent's read-only Role on events in the release namespace, and
`events` create/patch for the proxy in a namespaced Role `laboratory-proxy-events` of the release namespace, not in its ClusterRole (the operator, node-agent, VPN and gateway already could). It is best effort: a cluster that refuses the events loses the
journal, nothing else.

**What never leaves.** Every message is redacted where it is written and again where it is read: private keys, JWTs, bearer tokens, credentials in URLs, values of
`password`/`token`/`secret`-like keys, email addresses, IPv4, IPv6 and MAC addresses, uuids, hashes, host names, object names (`namespace/name`, group namespaces), and
any quoted string (they hold tenant and group names); a message is cut to 300 characters. The fingerprint is made from the further normalized text, so
occurrences that differ only in numbers or names group together. Nothing names a tenant or a participant.

### Service pods and images

The service pods of the laboratory are hardened without a setting:

| Pod | User | Root file system | Capabilities | Seccomp |
|---|---|---|---|---|
| operator | non-root | | all dropped, no privilege escalation | RuntimeDefault |
| agent | non-root (65532) | read-only (`/tmp` is an emptyDir) | all dropped, no privilege escalation | RuntimeDefault |
| L7 proxy and wg-demux | non-root (65532) | read-only | all dropped, no privilege escalation | RuntimeDefault |
| zot (registry) | non-root (65532, `fsGroup` for the volume) | writable | all dropped, no privilege escalation | RuntimeDefault |
| node-agent and OVS | root, **privileged** (it drives OVS and pod networking on the host) | | | |

Every service pod, the node-agent included (the node-agent container, the OVS sidecar and the init containers each have their own `nodeAgent.resources`,
`ovsResources`, `initResources`), and zot (`registry.resources`) has requests and limits, so one runaway process cannot starve a node. The values are starting
points: measure on your nodes. The kubelet's `podPidsLimit` is a node setting (infrastructure). The directory of the node-agent's socket
(`/run/cybericebox`) is `0700`: only root can reach the socket that drives pod networking.

Images (one `Dockerfile`, five targets), all bases pinned by digest:

- controller, agent and proxy: `gcr.io/distroless/static:nonroot`, user 65532, no shell, no package manager; the final stage holds only the binary.
- lab (the VPN and gateway pods): built **from scratch** with only the alpine `iptables` and `ip6tables` and their libraries and the `/lab` binary: no shell, no
  busybox, no package manager. It stays root: Kubernetes gives capabilities (NET_ADMIN) only to root.
- node: alpine with Open vSwitch, iproute2 and a shell, as root: the OVS start script and the host-prep and CNI-install init containers are shell, and the
  node-agent drives the host. It is the one image that keeps a shell, because the role needs it.

### Operator permissions

The operator is not a cluster-admin in disguise. Its ClusterRole (`laboratory-manager-role`) holds:

- full access to the platform's own resources (`laboratory.cybericebox.com`, `allocation.cybericebox.com`) and to Namespaces (the namespace of
  every LabGroup is made and removed by it);
- **read-only** access (get, list, watch) to nodes, pods, services, serviceaccounts, endpoints, deployments, networkpolicies,
  poddisruptionbudgets and rolebindings: the informers of its controllers and the scheduler's view of node capacity;
- create/update/delete of RoleBindings, and `bind` on exactly three ClusterRoles (`laboratory-operator-namespaced`, `laboratory-vpn-role`,
  `laboratory-agent-role`).

It has **no cluster-wide access to Secrets**, and no cluster-wide write access to anything that lives in a namespace. What it writes inside
a namespace (Secrets, Services, ServiceAccounts, Deployments, bare pods, NetworkPolicies, CiliumNetworkPolicies, PodDisruptionBudgets) comes
from RoleBindings to the ClusterRole `laboratory-operator-namespaced`, which exist only in: each LabGroup namespace (the operator creates
the binding itself right after the namespace, with the RoleBinding rights and the `bind` verb above), the release namespace (made by this
chart: the platform pull secrets it copies and the snapshot registry credentials) and, as narrower Roles, `laboratory-tenants` (Secrets only) and
`laboratory-images` (Secrets only: the credentials of a prepull request). The manager reads Secrets
straight from the API server instead of caching them, because an informer would need cluster-wide list and watch.

What remains, and why: the operator can still create a RoleBinding in any namespace to the three roles it may bind, so on its own RBAC cannot
stop a compromised operator from giving itself its working role in, say, `kube-system`. That is the job of the admission policy:

**Admission policy** (`operator.admissionPolicy.enabled`, default on, Kubernetes 1.30+). Three `ValidatingAdmissionPolicy` objects, each with a
`Deny` binding, apply to the operator's ServiceAccount only (anyone else is unaffected):

- `laboratory-operator-scope`: it may create, change and delete Secrets, ServiceAccounts, Services, Endpoints, pods, Deployments, DaemonSets,
  NetworkPolicies, CiliumNetworkPolicies, PodDisruptionBudgets and RoleBindings **only in namespaces labelled `laboratory.cybericebox.com/group`** (the
  namespaces of its LabGroups, which it labels when it creates them) and, except RoleBindings, in the release namespace and `laboratory-tenants`.
- `laboratory-operator-namespaces`: it may create only labelled namespaces, and change or delete only namespaces that already carry the label
  (so it cannot label `kube-system` and take it over).
- `laboratory-operator-pods`: a pod it creates, directly or through a Deployment or DaemonSet, may not be privileged or use the host's network, PID
  or IPC namespace, or mount a host path.

Together with the RBAC above, a compromised operator can read no Secret outside its namespaces and cannot start a pod that reaches into a node.
(CEL does not expose `ownerReferences`, so "a namespace of a LabGroup" is the label.) Denials say which policy refused. The policies are tested on a real
API server (`internal/controller/laboratory/admission_policy_test.go`).
The platform's own resources and Namespaces are cluster-wide because LabGroups, Labs and the VNI pool are cluster-scoped or cross-namespace.
The `config/rbac` kustomize role generated from the kubebuilder markers is for the development install only; the chart's roles are the
deployed ones.

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
    writeQuota: 512Mi         # write quota per device (the most of a participant's writes we keep)
    maxFileSize: 256Mi        # a file larger than this is never snapshotted
    maxLayers: 10             # snapshot layers before they are squashed into one
    retention: 168h           # how long a deleted lab's snapshots are kept
    containerdRoot: /var/lib/k0s/containerd   # host path of the containerd root
```

`enabled: false` (the default) renders nothing of the feature and keeps today's behaviour. With `true`:

- the chart deploys the platform registry ([zot](https://zotregistry.dev)) in `laboratory-system`: a
  PersistentVolumeClaim (`registry.storageClass`, `registry.size`; it is kept on `helm uninstall`), a Service, and a
  Secret with generated `htpasswd` credentials for the single writer;
- the switch only **allows** persistence. Each device decides for itself with `persistence.enabled` in its topology
  (see "State persistence" in the agent API): such a device runs as a bare Pod (`restartPolicy: Never`) that the operator owns
  and recreates, every other device as a Deployment, and one lab may mix both. The choice is stamped on the Device
  (`spec.state`) when it is created and is immutable, so flipping the switch later never changes an existing device;
- the node-agent runs the snapshot engine whenever persistence is allowed, and snapshots only the devices that run as
  snapshot-backed Pods.

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
4. **Large files.** A regular file larger than `maxFileSize` (default `256Mi`) is left out of the layer, like an excluded path
   but for that file only; everything else is snapshotted normally. `status.state.warning` of the Device names the skipped
   files with their sizes (the first 10 and a count of the rest) and stays while the files are there. Whiteouts are not affected.
5. **Write quota.** If a snapshot would make the kept layers larger than `writeQuota` (uncompressed), the last good
   snapshot is kept and `status.state.warning` of the Device is set; the warning clears with the next good snapshot. The quota is the
   total backstop and applies after the large files were skipped.
6. **Recreate.** When the pod ends, the operator waits until the node-agent marks the exit snapshot done
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

`devices.statePersistence.enabled: false` only stops new devices from using persistence (the agent refuses a topology that asks
for it). Devices that already run as snapshot-backed Pods keep that mode, but they depend on the registry and on the
node-agent's snapshot engine, which the switch also removes from the node-agent. Leave the switch on until the last such device is
deleted (or accept that they stop being snapshotted).

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

Size the volume for the base images plus (devices x `writeQuota`) in the worst case. If a device's warning says
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
- **Scheduler.** With the cache on, the prepull request lists the rewritten images, so the first node warms
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
management agent only (the operator and the scheduler take no part), through one RPC:

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

## Snapshot export

`ExportDeviceSnapshot(DeviceSnapshotRequest{ref}) returns (stream SnapshotChunk)` hands the platform the **latest state** of a
device with state persistence as one file, for archiving or analysis (for example after an exercise). `ref` is the
device: `lab_group`, `lab`, `name` (the device name). There is no layer history: the snapshot layers of the device's
chain are squashed into one.

**Stream.** The first message is `meta`, then `data` chunks, then `trailer`:

- `meta`: the device ref; `base_image` (as in the device spec) and `base_image_digest` (when the lab pinned it);
  `base_layer_digests` (compressed digests of the base image layers in the snapshot image, in order);
  `snapshot_digest` (manifest digest in the registry) and `snapshot_unix_ms`; `squashed_layers` and `layers_size_bytes`
  (the sum of the uncompressed sizes of the squashed layers, an upper bound of the content); `format`; `chunk_size`.
- `data`: the bytes of the archive, in order; every chunk is `chunk_size` (1 MiB) long except the last.
- `trailer`: `compressed_bytes` and `sha256` (hex) of the whole data stream, for the receiver to verify.

**Archive format.** The concatenated data is a **gzip** stream of one **tar** archive, laid out as an OCI image
layer: it holds the files that differ from the base image and applies on top of it. A file deleted since the device
started is a **whiteout** entry `<dir>/.wh.<name>` (it also hides the file of the base image), a directory emptied and
refilled is marked by `<dir>/.wh..wh..opq`. Whiteouts come first in the archive. Ownership, modes, symlinks and
extended attributes are those of the snapshot. Paths excluded from snapshots (`/tmp`, `/var/tmp`, `/run`, the runtime's
mount points) are not in it. To look at the files: `tar xzf archive.tgz` (the `.wh.` entries show as regular empty
files); to apply it to a base image rootfs, use any OCI layer applier.

**How.** The agent reads the snapshot image from the platform registry (the chart passes its address as
`AGENT_REGISTRY_ADDR` whenever persistence or the cache is on), squashes the layers while it streams (they are read
twice from the registry; nothing is stored on the agent) and compresses with gzip. A caller that goes away stops the
export.

**Errors.** `FailedPrecondition`: the device has no state persistence; it has no snapshot yet (nothing differs from the
base image, or the first snapshot is not taken); the agent has no registry address. `NotFound`: no such device or
group. `Unavailable` with a message: the registry could not be read.

The export is of the snapshot the registry holds at that moment; changes the node-agent has not snapshotted yet (the
last debounce period) are not in it. For a device that must be exported with everything, stop it first: its exit
snapshot is taken at once.

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

## Re-audit fixes and upgrade order

The fixes after the second audit (docs/security/2026-10-02-laboratory-reaudit.md). The rule for all of them: objects that already
exist keep working after the upgrade; a stricter check applies to new input, and where an old object could slip under it the
component that runs it clamps or ignores the bad value instead of failing.

**Upgrade order.**

1. Apply the CRDs first: `kubectl apply --server-side -f charts/laboratory/crds/`. `helm upgrade` does not update the `crds/`
   directory, so without this step the new validation patterns below are missing on an upgraded cluster (the code does not rely on
   them, they are the second line).
2. `helm upgrade` (the operator, agent, node-agent and proxy roll out together; the operator and the agent can run in either order).
3. The notes of each item below say what, if anything, must happen in a different order.

### Device resources (R-2)

- A device resource value must be a positive quantity (no exponent, at most 24 characters) and at most 1024 cores or 1 TiB. The agent
  refuses `"0"`, negative, overflowing and out-of-bound values on CreateLabs with an error that names the device; the CRD has the same
  pattern. The sums use saturating arithmetic.
- The operator ignores such a value on an object that predates the check (the next candidate or the default is used) and clamps
  anything above the chart maximum to it (`limits.device.maxCpu` / `maxMemory`, passed to the operator as `DEVICE_MAX_CPU` /
  `DEVICE_MAX_MEMORY`), so a pod never runs without limits. No existing lab is deleted or restarted by this; a pod that was created
  without a limit gets its limit when its Device is next rebuilt.

### Image policy and the registry forwarder (R-3)

- The agent refuses an image reference that names an IP address in any spelling (`127.0.0.1`, `127.1`, `0x7f.1`, `[::1]`), `localhost`
  (and `*.localhost`), or a host without a dot, and one with empty, `.` or `..` path components or characters outside printable ASCII.
  A tenant names the original image on a public registry; the operator routes it through the cache.
- Registry hosts are compared canonically: lowercase, no trailing dot, default ports `:443` / `:80` dropped, and `index.docker.io`,
  `registry-1.docker.io`, `registry.hub.docker.com` and `docker.io` are one registry. The deny list (`images.tenantDeny`) matches the host
  whatever the port, so `ghcr.io:8443/platform/x` is denied with `ghcr.io/platform`. The platform registry and cache hosts are refused by
  host, not by prefix. Allow-list entries are canonicalised the same way.
- The node-agent's forwarder adds the zot reader account only to a request whose `Host` is exactly `localhost:<state forward port>` (the
  address the node's runtime pulls from), and not to a path with `..` or `//`.
- Upgrade: no order constraint. Labs that exist keep their pods; only a new or changed spec is checked again. A lab whose stored spec
  names a refused spelling is not touched until its devices are recreated.
- `images.tenantDeny` still defaults to empty: the chart cannot know which repositories of an installation are private. Set it to the
  platform's private organizations.

### Names and system pods (R-19)

- **System pods are selected by `laboratory.cybericebox.com/component`** (`vpn`, `gateway`), a label under the platform prefix that no
  caller can set, never by `app`: device pods carry `app=<device name>`, so a device named `vpn` used to join the VPN Service and match
  the network policies written for the VPN. The VPN Service, the `vpn-egress` and `gateway-egress` Cilium policies, the label sync,
  the scheduler and the node-agent all use it. The web Service and web NetworkPolicy of a device select by the lab and device labels.
- **Reserved device names**: `vpn`, `gateway`, `internet` are refused on CreateLabs and by the Lab CRD (a CEL rule on the device name,
  ratcheting: a Lab that already has such a device can still be updated).
- **Device object names** are `<lab>-<device>-<hash>` (10 hex over the pair with a separator no name contains), so two pairs never
  share a name; the env Secret is `<device object name>-env`. The old name `<lab>-<device>` was ambiguous (lab `a-b` with device `c`, lab
  `a` with device `b-c`).
- **Gateway**: each lab interface of the gateway pod may send only from its own subnet (`LABSRC`), the forward rule accepts only
  `lab+` interfaces towards the outside, and the filter is built in the order that is never open (policy DROP, then the egress and
  source filters, then the accepting rules; the egress chain is rebuilt beside the old one and the jump switched, so a restart has no
  gap). A pod without ip6tables logs that it forwards no IPv6.
- **Upgrade (existing objects keep working).** The operator adds the component label to the VPN and gateway Deployment templates (the
  selector is immutable and untouched), so each pod is replaced once, like for any hardening change. The VPN Service and the Cilium
  policies keep the old `app` selector until every VPN or gateway pod of the group carries the new label, then switch, so there is no moment
  without a selector. Devices that exist under the old name keep it and keep their pods; only a pair that has no device yet gets the new
  name, and an old-named object that belongs to another pair is never adopted. Nothing has to be done by hand. Apply the CRDs first
  (see the upgrade order above) so the reserved-name rule and the device ceiling (`maxItems: 64`) are present.
