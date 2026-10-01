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

**Before the first pod of a group** its images (those of all its Labs) are pulled onto the eligible nodes by
a short-lived DaemonSet `prepull-<hash>` in `laboratory-system`, one container per image with the command
replaced by a sleep (the lab service never starts), on the nodes of `labWorkloads.nodeSelector`/`tolerations`,
with `imagePullSecrets`. Dispatch waits until every pod holds all images or `scheduler.prepull.timeout`, then
the DaemonSet is deleted (a missing image must not stop the queue). Filling the registry cache beforehand is
the agent's job (`PrewarmImages`), not the scheduler's.

**Resource check.** A pod is dispatched only when the schedulable nodes (Ready, not cordoned, matching the
lab node selector, taints tolerated) have free CPU and memory for its requests: allocatable minus the
requests of all scheduled pods, minus what already dispatched pods will still request, and with
`scheduler.headroomPercent` of the allocatable resources kept free. Otherwise the queue waits (it does not
fail) and the reason is in the status. A pod that requests more than the whole schedulable capacity can never
fit: it is declared failed (`DoesNotFit`) and the queue goes on.

**Failed pods.** A dispatched pod that is not Ready after `scheduler.startupTimeout` (5m), or that restarted
`scheduler.restartThreshold` times (5), is declared failed: its slot is freed, the group still completes,
nothing else is rolled back, and the device gets a warning: `Device.status.scheduling.failure` and, on the
Lab, `status.devices[].failure`, with the reason (`ImagePull`, `CrashLoop`, `Unschedulable`,
`StartupTimeout`, `DoesNotFit`), the last error the node reported and the restart count. If the pod becomes
Ready later it is Started and the warning clears. A snapshot-backed device whose pod ended after it had started
is recreated at once, with no slot, as without the scheduler.

**Guaranteed resources.** Every device container gets requests equal to limits, so its pod is Guaranteed.
Per resource the limit wins, then the request, then `scheduler.deviceDefaults` (250m CPU and 256Mi memory);
a declared request and limit that differ collapse to the limit. Set a default to `""` to leave a device without
that resource (best effort). The resources are applied when a device is created; running devices are not
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
| `headroomPercent` | `10` | share of schedulable CPU and memory kept free (0-99) |
| `resourceCheck` | `true` | `false` skips the free-resource check |
| `prepull.enabled` | `true` | prepull the images of a group |
| `prepull.timeout` | `5m` | dispatch goes on after this long |
| `deviceDefaults.cpu` / `.memory` | `250m` / `256Mi` | resources of a device that declares none |

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
quota are cluster settings of the chart (`devices.statePersistence.excludePaths`, `maxSnapshotSize`) and cannot be requested per device.

**Tenancy.**

The agent serves several clients (tenants) from one cluster and keeps them apart.

- **Identity.** The tenant of a call is the CN of the verified client certificate (mTLS). A CN that is a valid label
  value of at most 63 characters is the tenant key as is, any other CN becomes `h` + base36(SHA-256). A call without a client
  certificate (TLS or mTLS off, local development) is the tenant `default`, and so is a client whose CN is `default`.
- **Stamp.** Every object the agent creates (LabGroups, Labs, VPN clients, access policies) gets the reserved label
  `laboratory.cybericebox.com/tenant`. The operator copies it to the Devices and pods of a Lab and the pods of a
  LabGroup. Like every reserved label it is hidden: never in answers, never accepted from a client, never allowed in a selector.
  Objects without the label (created before tenancy) belong to the `default` tenant.
- **Scope.** Every RPC is implicitly scoped to the caller's tenant: `List*`, `Update*`, `Delete*`, the device calls and `Monitoring`
  see only its objects, combined with the caller's own selector (which can narrow the scope but never widen it). Objects inside a
  LabGroup belong to the tenant of the group. Another tenant's object is `NOT_FOUND` for every operation, as if it did not exist.
- **Names.** LabGroup ids are global. Creating an id that belongs to another tenant fails for that item with "the id is not
  available", with no hint that it exists or is being deleted.
- **Monitoring** is cut to the tenant before the user selector. `GetCapacity` is cluster-wide for now.

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
- **Scheduler.** With the cache on, the prepull DaemonSet pulls the rewritten images, so the first node warms
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
