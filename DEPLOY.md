# Deployment Guide

CyberICEBox Laboratory installs via Helm. All configuration lives in `values.yaml` — no manual editing of YAML
manifests.

## Prerequisites

| Requirement | Version | Why |
|---|---|---|
| Kubernetes | **1.33 or newer** (the chart refuses an older cluster: `kubeVersion` in `Chart.yaml` and `templates/validate.yaml`) | device pods run in user namespaces (`spec.hostUsers: false`; below 1.33 the API server can drop the field silently and a device would run in the host user namespace), native sidecars, ValidatingAdmissionPolicy |
| Container runtime | containerd 2.x | user namespaces for pods |
| Node kernel | 6.3 or newer | idmapped mounts for user-namespace pods |
| `kubectl` | 1.33+ | |
| `helm` | 3.12+ | |

With `devices.security.userNamespaces: true` (the default) the operator's admission policy also refuses any device pod without `hostUsers: false`, so a
cluster that ignored the field would fail loudly instead of running devices as host root.
- Worker nodes need only k0s and a Linux kernel with the `openvswitch`, `geneve`, `wireguard`, `br_netfilter`, `nf_conntrack` and `nf_conntrack_netlink` modules (cgroup v2). Open vSwitch runs in the node-agent DaemonSet, which also loads the modules and sets the sysctls (`nodeAgent.hostPrep`; `net.core.fb_tunnels_only_for_init_net=2` keeps the kernel fallback tunnel devices gre0, gretap0, erspan0 out of every pod). The image ships Open vSwitch 3.7.1 (alpine 3.24); 4.0.0 comes with the next alpine stable release.

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
> `$(LOCAL_K0S)/README.md` in the infrastructure repo (default `../infrastructure/local/cluster`); `make cluster-up` uses the Kind config `hack/kind/kind-config.yaml` of this repo (`KIND_CONFIG`).

---

## 2. Configure

Create your `values.yaml` override file. Required: `operator.publicVPNEndpoint`, `operator.baseDomain` and, with the agent, `agent.domain`. Tenants (the platform backends that use the agent) are declared under `tenants:` (see [Tenancy](#tenancy)):

```yaml
# my-values.yaml
operator:
  publicVPNEndpoint: "vpn.example.com"         # REQUIRED: a host, or host:port (the port is proxy.wg.publicPort, 51820 by default)
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
  publicVPNEndpoint: "${NODE_IP}"
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
- `laboratory-system` namespace, the `laboratory-tenants` namespace (enrollment tokens of tenants) and the `laboratory-access-keys` namespace (their access keys) and the `laboratory-images` namespace (credentials of a prepull request)
- One `Tenant` per entry of `tenants:`, and `default`
- Operator (Deployment + RBAC)
- Node-agent (DaemonSet + RBAC) on every node
- Management agent (`agent.enabled`), the L7 proxy and WireGuard demultiplexer, and, when enabled, the platform registry
- `laboratory-config` ConfigMap with your settings

**The proxy** (`proxy.*`) is the L7 HTTPS proxy, an unprivileged Deployment in `laboratory-proxy`, **two replicas by default**
(`proxy.replicas`), a PodDisruptionBudget with `minAvailable: 1` (`proxy.podDisruptionBudget`, rendered only with more than one replica) and a
a **required** pod anti-affinity by hostname: never two replicas on one node (with more replicas than nodes the extra ones stay Pending). For larger clusters
set `proxy.mode: daemonset`: one proxy pod on every node (`proxy.nodeSelector` and `proxy.tolerations` narrow the set; `replicas` and the budget do not apply; the default is
`replicas`, a fixed count: on a one-node cluster set `proxy.replicas: 1`, and a live install refuses more replicas than nodes). In both modes the proxy requests are pods like any other, so the scheduler's room for lab pods on each node is what is left after them
(see "Platform reserve"). The WireGuard demux is not in that pod: it is its own DaemonSet `laboratory-proxy-wg` in the namespace `laboratory-wg`, on the nodes
selected by `proxy.wg.nodeSelector` and `proxy.wg.tolerations` (empty: they follow `labWorkloads`, every node when that is empty too), with its own ServiceAccount, RBAC and the `laboratory-proxy-wg` LoadBalancer Service
(`proxy.wg.externalTrafficPolicy`; empty by default, which follows the mode: `Cluster` for `userspace`, `Local` for `tc` and `xdp`). `proxy.wg.mode` (`userspace`, `tc` or `xdp`; `userspace` is the default; `tc` and `xdp` are opt-in, see "WireGuard demux modes") decides its privileges: `tc` and `xdp` run on the host network
as root with the capabilities `BPF`, `NET_ADMIN` and `PERFMON` only and the host's `/sys/fs/bpf`, in a namespace that admits the privileged Pod Security level;
`userspace` is unprivileged on the pod network in a restricted namespace. The L7 namespace stays restricted in every mode. The proxy's own session is sliding: it expires `proxy.l7.sessionIdleTTL` (24h) after the last request; the cookie is
re-issued only when less than `proxy.l7.sessionRenewBefore` (1h) of it remains, so an active user costs about one renewal per hour; and it never
lives past `proxy.l7.sessionMaxTTL` (168h) from the handoff or past the link's own session end. The L7 proxy is stateless (its session cookie is signed with the shared `proxy-session` secret), so a request
may land on any replica. In `userspace` mode the demux keeps no state a restart would lose: a WireGuard client that lands on the other replica (after a reconnect, or when
the replica it used is gone) re-handshakes within its persistent keepalive, about 15 s. In `tc` and `xdp` the kernel keeps the sessions across a restart of the pod. Both pods are Guaranteed (requests = limits): L7 `500m`/`64Mi`,
demux `250m`/`128Mi`. Measured on the local stand (kubectl top, one replica): L7 serves about 5000 requests per second for 500m of CPU (0.1 ms per request;
p95 63 ms at 100 concurrent clients, 36Mi at 200), the demux in `userspace` mode is 1m idle and about 110m at ten active VPN peers (it relays the packets in userspace; `tc` and `xdp` are not measured under load yet),
and L7 load does not slow the tunnels (VPN latency p95 3-4 ms with 100 L7 clients).

Verify:

```bash
kubectl -n laboratory-system get pods
kubectl -n laboratory-system get configmap laboratory-config -o yaml
```

---

## 4. Upgrade (change config)

Edit `my-values.yaml`, then (the first upgrade to the WireGuard demux modes needs one step before it, see "Upgrade order" step 2):

```bash
helm upgrade laboratory ./charts/laboratory \
  --namespace laboratory-system \
  -f my-values.yaml
```

The ConfigMap is re-rendered from values. Operator pod restarts automatically (Deployment update triggers rollout).

**Image tags.** Production uses exact version tags (`operator.image.tag`, `vpn.image.tag`, `inetGateway.image.tag`, `proxy.image.tag`, `nodeAgent.image.tag`,
`agent.image.tag`; an empty tag is the chart's `appVersion`). The chart always refuses `latest` and an empty tag (there is no switch for it): a moving tag leaves
nodes on different images and a rollout cannot tell the versions apart. A release version is the tag of a release, the short commit (`sha-<short commit>`) of a development build. The image
pull policy is `IfNotPresent` when `pullPolicy` is empty.

**The VPN and gateway pods of existing groups.** They run `vpn.image` and `inetGateway.image` (the binary with the gateway egress filter, the anti-spoof check and the
WireGuard handling), so after a chart upgrade the operator brings every existing group's VPN and gateway Deployment to the configured image and pull policy, and to nothing else: a group's pods keep the size (CPU and memory) and the command they were created with, a changed
`vpn.resources` or `inetGateway.resources` reaches only new groups. It does that **one group at a time**: a group's two pods restart together (its tunnels drop and the clients
re-handshake), and the next group starts when the previous one's pods are rolled out and available, or after 10 minutes if they never become so (a bad image does not
hold up the cluster, but it does reach the next group). Watch it with `kubectl get deploy -A -l app -o wide` (the `vpn` and `gateway` Deployments) and the operator log.
With the image cache on, the image is the pinned reference the operator resolves (`registry.cache.pinTTL`).

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
(`InsufficientResources`). They still need a slot and room, and they obey `deploy-after`.

**Larger labs first, with backfill.** Nothing is repacked during a running event, so the labs are placed to leave
no holes: inside a deploy group (and among the objects without one) the order is an object that is already being
dispatched, then the **explicit priority** (annotation `laboratory.cybericebox.com/deploy-priority`, an integer,
`0` when absent; higher goes first and wins over size), then the **larger lab** (the sum of its pods' CPU, then
memory), then arrival. The deploy groups themselves, their `deploy-after` dependencies and the group order
(arrival, i.e. the category order the platform creates them in) are unchanged: size orders objects only inside a group. A
group (or an independent lab) that does not fit now as a whole stays queued (`InsufficientResources`) and the ones behind it
go past it (**backfill**), so small labs fill the gaps while the big one waits; on the next pass the big one is first again.
The order of the pods inside one object is kept.

**Groups.** Objects with the same *deploy group* form a group (mixed kinds, one object or many). The
agent writes two operator-internal markers on LabGroups and Labs; the operator reads only these, never
user labels (and they work when someone applies the CRs directly):

| Marker | Meaning |
|---|---|
| label `laboratory.cybericebox.com/deploy-group=<key>` | the group of the object; key of at most 63 characters (base36) |
| annotation `laboratory.cybericebox.com/deploy-after=<key1>,<key2>` | the groups that must be **complete** (every pod Ready or failed) before this group starts |

The rules:

1. The group being dispatched (some pod out, some still pending) gets the slots first.
2. When it has **no undispatched pod left**, the next group starts once there are free slots for all its pods
   (a group larger than `maxPods` goes in slices of the free slots, its room checked as a whole) and room for all of
   them on the nodes; it does not wait for readiness. A group with a pod the operator has not set up yet (a lab created a
   moment ago) waits whole (`WaitingForTurn`, the message names the lab) until every pod of it is set up, so it never starts
   without some of them. Two limits bound the wait, and the earlier one decides. Every lab is waited for at most 15 minutes
   from its creation (enough for the operator's queues at the start of an event, about 150 labs). A lab with a Device the
   operator never made is also waited for at most 2 minutes from the operator's first work on the lab (`status.imageCache`
   set, as the scheduler saw it; a restart starts this again); a lab not worked on yet, or whose Devices exist but are not
   set up yet, is still in a queue and only the 15 minutes apply. A lab still not set up then gets one Warning event `PodsNotSetUp` and the
   group goes on without it, the whole lab out (none of its pods starts, also when some of them are set up); it joins later as
   the rest of its group once all its pods are set up. A lab in phase `Failed` is no member of its group at all.
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
nothing from the prepull.
Credentials are the tenant's own (see "Tenant images" below), never the platform's: the operator copies the
tenant's registry Secret into the `laboratory-images` namespace for the life of the request, the node-agent
(whose Role there is `get` on Secrets only) reads it, and the copy goes with the request. A request is one per
tenant and launch class. The group's pods wait until every listed node has resolved all images, then the request
is deleted. An image that **cannot be pulled** (a wrong name or tag, no access: the runtime refuses it) is
given up on at once, within seconds: the group goes on without it and its pods that use that image fail
through the normal per-pod `ImagePull` path (reason and error in `Device.status.scheduling.failure`). A node
whose node-agent does not answer holds the group until `scheduler.prepull.timeout`. Pulls on a node run two
at a time, 5 minutes per image (`IMAGE_PULL_CONCURRENCY`, `IMAGE_PULL_TIMEOUT` of the node-agent). `scheduler.prepull.timeout` is only a safety net for a pull that is slow
but not failing. A prepull gates **only its own group**: while a group's images are still being pulled
(reason `PreparingImages`), the pods of other groups and of independent objects are dispatched as the order and
dependency rules allow, and their own prepulls run meanwhile. Filling the registry cache beforehand is the
agent's job (`PrewarmImages`), not the scheduler's.

**Resource check.** A pod is dispatched only when one schedulable node (Ready, not cordoned, matching the
lab node selector, taints tolerated) has room for its CPU and memory together. The room of a node is its allocatable less
the platform reserve, the requests of the pods that are not lab pods and the packing reserve, less the lab pods on it and the
pods already dispatched to land there (see "Platform reserve" below). No sum over the cluster is checked: only this per-node
placement decides. Otherwise the queue waits (it does not fail) and the reason is in the status. The labs of one deploy group (an independent lab and each LabGroup alike) start
whole or not at all: every pod must find room on one node at once, else none starts (`InsufficientResources`), and a
group that waits does not hold up the others behind it. A group that does not fit even on the empty lab nodes waits with
`DoesNotFitNow` and starts once capacity returns (a node comes back, a rollout ends); it never starts partly. Each of its labs
gets one Warning event `GroupTooLarge` and the status message names the group, what it requests in all and the room of the
largest empty lab node. The platform puts one deploy group on a task for all teams, so **the largest task times the number of
teams must fit the empty room of the lab nodes** (with the reserves below), or that task does not start for anyone: the load
test checks this before an event. Only a pod that no single node can hold, its
CPU and memory together on the same node, is failed for good (`DoesNotFit`), with its whole group, and the queue goes on.
Each node counts with nothing on it: its allocatable less the platform reserve and the packing reserve. Every node of the
pool counts, cordoned, NotReady and rebooting ones too (the pool is the lab node selector without the `node-agent-ready`
label, which only says the node-agent runs now), so a drained or rebooting big node does not fail a pod; placement itself
still needs the label. With no
schedulable lab node at all the queue waits (`NoSchedulableNodes`). Every pass places the pods on the nodes in one simulation,
each pod whole on one node (its CPU and memory from the same node): first the lab pods already on a node and those dispatched
but not on a node yet, then the rest of every group that already has pods out, in arrival order, then the new groups. Such a
rest that places keeps that room and starts in it; one that does not place keeps nothing in that pass and waits, so it never
holds up the groups behind it (`DoesNotFitNow` when it does not fit even on the empty nodes beside its own running pods). A group
whose sum never fits its tenant's quota is failed at once (`TenantQuotaNeverFits`); one that fits only later waits
(`TenantQuota`).

**Fair order and aging.** Groups that already have pods out keep their order ahead of all others. Of the new groups, those
that run under a reservation (label `laboratory.cybericebox.com/reservation`) go first, the reservation with the lowest
satisfied share (used / reserved, memory, then CPU) first, then arrival; the unreserved groups follow in the order above. A
group that has waited in the queue longer than `scheduler.agingSeconds` (default `0`: aging is off; counted from when its
pods were last queued, so a resumed lab starts again from zero) is aged: the oldest aged group that waits only for room keeps
its placement on the empty nodes (beside its own running pods and the room stopped labs keep), and the other groups use only
the room not kept for it, so a stream of small groups cannot keep a big one out forever. One group at a time; the room is
free again once it starts, or once it waits for anything but room (quota, images, `DoesNotFitNow`). Only lab groups age and
are held back: the VPN and gateway pods of LabGroups are placed first and may use any room, so a new team always gets its
network. A group still waiting for its `deploy-after` groups never keeps room; once they are complete it ages by its arrival.
Aging is off by default: the room kept for the aged group is held against every other group of every tenant, without a time
limit, so on a node full of labs that run for a whole event one group that does not fit would stop all new work. Turn it on
once the reservation scopes the aged room (all work runs under reservations).

**Reservations.** A `Reservation` (cluster-scoped, `kubectl get res`) claims CPU and memory for a tenant from `spec.from` to
`spec.to`. A Lab or LabGroup with the label `laboratory.cybericebox.com/reservation=<name>` starts only if that reservation
exists for its own tenant (else it waits with `ReservationMissing`), its window is open (`ReservationNotActive`), and the
requests of the reservation's pods already out plus the whole group stay within `spec.resources` (`ReservationFull`). All
labs of a deploy group must carry the same label (or none): a group whose labs disagree waits whole with `ReservationMissing`
("objects of the group belong to different reservations"); its pods already out still count under their own label. A
reservation with a resource other than `cpu` and `memory`, or with `cpu` or `memory` missing, zero or negative, reserves
nothing. Pods already out always count and are never stopped, also when the reservation is made smaller than them. What an
open reservation has not used yet is kept on the nodes on every pass, the emptiest node first and with its CPU to memory
ratio: unreserved groups, the groups of other reservations and the aged group never use that room (a LabGroup's VPN and gateway
without the reservation neither); the reservation's own groups use it first. When the nodes cannot hold all of it, the
reservations whose groups come first in the fair order are kept first. Unreserved work (no label, e.g. the `default` tenant)
runs as before, in the room no open reservation keeps. The scheduler writes `status.used` (the requests of the reservation's
pods out, of its own tenant) and `status.groups` (the groups with pods out) at the same limited rate as the queue status, and
`status.usedAt`, when it counted them (refreshed every 30 s while the reservation is open, also when nothing changed; these
heartbeats have a small budget of their own per pass, the oldest first). A label that is there but empty names no
reservation: such a Lab or LabGroup waits with `ReservationMissing` ("the reservation label is empty"), never runs as
unreserved work and is never taken by the cleanup below.
Without the Reservation CRD (an upgrade that did not apply `crds/`) the operator still starts and keeps running: the scheduler
logs one warning and runs as if there were no reservations (unreserved labs start, labelled ones wait with
`ReservationMissing`), and the operator starts without the Reservation controller (one line in its log at start). Labelled
work keeps waiting with `ReservationMissing` also after `kubectl apply` of the CRDs, since no window would end without the
controller: restart the operator to get it.

**A Reservation deleted by hand.** A Reservation that is deleted (by hand or by the backend) instead of ending never reaches
its end, so the scheduler takes care of what ran under it: a Lab or LabGroup whose reservation label names a Reservation that
does not exist is never admitted (`ReservationMissing`), and one that has pods out already (a Lab also when stopped) gets the
notice of an ended reservation (`reservations.endingNoticeSeconds`, at least a minute, counted from when the scheduler first
saw it so, with a Warning event `ReservationGone`) and is then deleted. The scheduler must have seen it so on every pass of
that notice: a restart or a gap in the passes starts the notice again, and a Reservation that comes back calls it off. Before
the deletion the Reservation (it must still be gone) and the object (the same UID, the same label) are read past the cache,
and the object goes with its UID and resourceVersion as precondition; what is not deleted is tried again 30 s later. A
LabGroup whose namespace holds anything but labs of its own tenant under the same gone reservation is kept. Labs of the gone
name that never ran only wait. Without the Reservation CRD nothing is taken (the list then says nothing about what exists),
and with `scheduler.enabled=false` nothing either. Deleting the Reservation CRD deletes every Reservation with it. While the
CRD is absent nothing is deleted and no notice runs: the scheduler asks the API server's discovery whether the kind is served
before a notice starts and before any deletion, and reads the UID of the CRD (`get` on that one CRD). Reinstalling the CRD
does not bring the reservations back: from then on the labs under the missing names get a full notice and are deleted unless
the backend sends their reservations again within it (`reservations.endingNoticeSeconds`). A CRD deleted and applied again
between two passes is caught by its new UID: the notice given under the old one deletes nothing and starts again. The label alone decides: a running lab that gets the label of a
reservation that does not exist (a wrong label set by hand) is deleted after the notice, whatever its tenant.

**End of a reservation.** The operator moves `status.phase` with the clock: `Pending` before `spec.from`, `Active`, `Ending`
from `spec.to` minus the notice (chart `reservations.endingNoticeSeconds`, default `300`; condition `Ending=True`,
`status.noticeAt` = when the notice was given), `Ended` at `spec.to`. At the end every LabGroup and Lab of the reservation's
tenant with its label is deleted in one pass; labs of other reservations, unlabelled ones and other tenants' objects stay, and
a LabGroup of the reservation that still holds such labs is kept until they are gone (deleting it would drain them). Moving
`spec.to` past the notice calls the end off (`Ending=False`, reason `Extended`); moving `spec.to` into the past ends the
reservation at once. The end is written to the status (with the version read as precondition) before anything is deleted,
and later passes check it on the API server, so an extension the operator's cache does not hold yet never loses groups. An end
that passed while the operator was down is processed when it starts; for 10 minutes after `spec.to` the operator looks again
every minute (objects it did not see at the end go too). When the tenant of a reservation is gone (checked on the API server),
the reservation ends one notice after that is seen, at least one minute (or at `spec.to` if sooner, reason `TenantGone`), and
stays ended (an admin who resets the status by hand while a tenant of that name exists reopens it). The condition
`WithinAmount` says whether `status.used` fits `spec.resources`; its message keeps the generation and amount of the last fit
(`last fit: generation=G cpu=Cm memory=M`). When the backend made the reservation smaller than what runs (a newer generation,
and the amount lower than at the last fit in a resource that is over), it gets `Exceeded=True` with the deletion time; still
so one notice later (at least one minute, checked on the API server, and with `status.usedAt` younger than a minute: else
the deletion waits and the message says "usage not fresh"; when the server drops `status.usedAt`, an outdated Reservation
CRD, it says to apply the chart CRDs), the groups created more than 10 s before that notice are deleted
once, the reservation stays open, `Exceeded` turns False (`GroupsDeleted`) and the current amount becomes the
new baseline, so only a further shrink can delete again. A raise that brings the usage within the amount, or the usage
dropping within it, cancels it (a raise that is still short keeps the notice). Usage over the amount for any other reason
(device defaults changed, a Lab edited, a label moved: the scheduler recounts what runs; also usage that was already over the
last fitting amount before a shrink), or with an `Invalid` spec, only sets `OverAmount=True` and never deletes. With `scheduler.enabled=false` the usage conditions are off.
An ended reservation is deleted a day after its end. Resources that reserve nothing get the condition `Invalid=True`; the phase
still follows the clock.

**Capacity shortfall and unreserved work.** On every pass (with `scheduler.resourceCheck` on) the scheduler places the active
and future reservations on the empty lab nodes the way it keeps their room (emptiest node first, CPU to memory ratio kept), for
every stretch of time in which the same ones overlap, and adds what the active ones find no room for on the nodes now. When
either is short, every tenant with an active or future reservation gets `CapacityShortfall=True` (reason
`ReservationsDoNotFit`; the message has the peak, the room on the nodes and what is short); it turns False
(`ReservationsFit`) when they fit, and the metric `laboratory_reservation_shortfall{resource="cpu"|"memory"}` (cores, bytes) has
the same number. A warning only: nothing of a reserved tenant is stopped for it. The condition is written with the version read
as precondition, so the agent's `status.reserved` and `status.used` are never overwritten (and the agent's write conflicts
instead of erasing it). When the active reservations find no room now because unreserved work holds it (Labs and LabGroups
without the reservation label, e.g. the `default` tenant), every unreserved Lab and LabGroup of each tenant whose work holds that
room (without it the shortfall would be smaller) gets the annotation `laboratory.cybericebox.com/evict-at` (now plus
`reservations.endingNoticeSeconds`, at least a minute) and a Warning event. The cluster cannot know which of them matter, so all
of the tenant's unreserved objects are marked, stopped labs too. The annotation is removed as soon as the shortfall clears (or
the tenant's work no longer holds the room; a Normal event `UnreservedEvictionCancelled` says so). A mark alone never deletes:
the scheduler must also have seen the object as a marked target on every pass for one full notice. After a restart, a stopped
scheduler or passes where the capacity was unknown, a mark found with less than a full notice left is given again (now plus
the notice, with a new event), so the shortfall of the moment always gets its full notice. At its time the scheduler reads the
cluster again past its cache, and deletes in one pass, each with its UID and resourceVersion as precondition, the marked
objects whose tenant still holds the room and that are still unreserved; what is not deleted is tried again 30 s later. A
tenant counts as holding the room only if removing exactly what the cleanup would take of it (not a LabGroup it keeps, see
below) makes the shortfall smaller.
Objects with a reservation label (an empty one too) are never touched, and a LabGroup whose namespace holds a reserved lab or
another tenant's lab is kept (deleting it would drain that lab); its unreserved labs are deleted. A lab node that is NotReady
or cordoned has no room for the reservations: a node down longer than the notice (a reboot, an upgrade) can therefore have the
unreserved work on the other nodes deleted, as spec item 19 says (unreserved work has no guarantee). The agent writes only its
own tenant status fields (`reserved`, `used`, `observedAt`, a merge patch), so it never conflicts with the condition.

**Platform reserve.** CPU and memory that user labs can never consume, so the platform itself always has room.
It is kept on every schedulable node: an optional absolute reserve (`scheduler.platformReserveCpu`,
`scheduler.platformReserveMemory`, `"0"` = none) comes off the node first, and `scheduler.platformReservePercent` (10) of the
rest. It is applied after the kubelet's own reserve (a node's allocatable is already net of kube-reserved,
system-reserved and eviction thresholds), and the requests of every other pod scheduled on the node come off as well:
DaemonSets, the proxy (Deployment or DaemonSet mode alike) and system pods, not only lab pods.

**Failed pods.** A dispatched pod that is not Ready after `scheduler.startupTimeout` (5m), or that restarted
`scheduler.restartThreshold` times (5), is declared failed: its slot is freed, the group still completes,
nothing else is rolled back, and the device gets a warning: `Device.status.scheduling.failure` and, on the
Lab, `status.devices[].failure`, with the reason (`ImagePull`, `CrashLoop`, `Unschedulable`,
`StartupTimeout`, `PodNotCreated`, `DoesNotFit`, `TenantQuotaNeverFits`), the last error the node reported and the restart count. If the pod becomes
Ready later it is Started and the warning clears. A snapshot-backed device whose pod ended after it had started
is recreated at once, with no slot, as without the scheduler.

**Device resources and QoS.** Memory: request = limit (the limit wins, then the request, then `limits.device.defaultMemory`, 256Mi).
CPU may burst: the container requests `cpuRequest` (else `cpuLimit`, else `limits.device.defaultCpu`, 100m) and is limited to `cpuLimit` (else
the request), never below the request. Both are clamped to `limits.device.maxCpu`. A device with only a limit behaves as before (request = limit), one
with only a request has limit = request. The pod is therefore Burstable, not Guaranteed (the VPN and gateway pods stay Guaranteed). Everything that
plans counts the REQUEST: the scheduler, `SpecTotals`/`CheckSpec`, the group caps, the tenant quota and the reservation fit and budget; the CPU limit above
the request is a burst that nothing reserves (a 15m/60m device consumes 15m). The resources are applied when a device is created; running devices are not
changed. Each lab group namespace has one PodDisruptionBudget `lab-group` (`maxUnavailable: 0`, all pods of
the namespace), so node drains and the autoscaler do not evict running labs (see "Maintenance of a node" below).

**Packing reserve (hidden).** `scheduler.packingReservePercent` (15, 0-49) is the part of every lab node that is kept
unused, because nodes do not share memory and devices leave gaps between them, so the last part of a node can never be packed
full. It is taken **per node**, from what is free there after the platform reserve and the requests of the non-lab pods, by
the operator and the agent alike: the operator admits gangs only into the room of each node net of it (the placement
simulation above), and the agent reports `GetCapacity` and the features' tenant quota net of it: the smaller of the tenant's
quota and the real room of the lab nodes (a tenant without a quota is told the real room), each less its reserve.
`scheduler.packingReserveCpu` / `packingReserveMemory` (absolute quantities, `"0"` = none) replace the percentage for that
resource on every node; the agent takes that fixed amount once off a tenant's quota, which is a cluster-wide number, so with
several lab nodes the quota it reports is a little high. A tenant never sees the reserve or the nodes; the quota the operator
enforces is unchanged. The platform adds no buffer of its own. 15 % is the default and stays so for now; the value is
revisited from the load-test measurements (what the nodes really pack), in both roles at once.

**Bin-packing scheduler.** Lab pods (devices, VPN, gateway) are placed by a **second kube-scheduler** with one profile,
`laboratory-binpack` (chart `labScheduler.*`): `NodeResourcesFit` with `scoringStrategy: MostAllocated` (weight
`labScheduler.fitWeight`, 3) and the spreading `NodeResourcesBalancedAllocation` score off, so pods fill one node after
another instead of spreading and no hole is left everywhere. The operator puts `schedulerName` into the pods it creates
(`LAB_SCHEDULER_NAME`; the VPN and gateway Deployments of groups that already exist keep what they have). System pods keep the
cluster's scheduler. A second instance is the portable way (the control plane's own scheduler configuration, k0s included, is
not reachable from a chart); it runs in the release namespace with a lease of its own, the cluster's `system:kube-scheduler` and
`system:volume-scheduler` roles, and the image `registry.k8s.io/kube-scheduler` at the cluster's own version (override
`labScheduler.image` for an air-gapped registry). `labScheduler.enabled: false` leaves lab pods to the default scheduler. If the
lab scheduler is down, new lab pods stay Pending until it is back (running labs are not affected).

**Placement of a group.** Pod placement is the lab scheduler's (or the default one's), with two soft preferences on device pods (never required, so a full
node never blocks a lab): the node of another device of the same lab (weight 100: a lab's traffic stays node-local), then a node that already
runs any pod of the same group, i.e. its VPN, gateway and other labs (weight 50). A group's labs therefore end up together on as few nodes as
capacity allows, which makes a node empty sooner when you take it out for maintenance. Nothing guarantees one node per group.

**Maintenance of a node.** The `lab-group` budget blocks every voluntary eviction in a group namespace, the VPN and gateway pods included, so
`kubectl drain`, an autopilot upgrade or the autoscaler's scale-down wait on a node that still has lab pods. That is intended: a running lab is
never cut for maintenance (a cluster autoscaler too: lab pods also carry `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`,
see "Maximum capacity and node autoscaling"). The flow is:

1. `kubectl cordon <node>`: new lab pods go elsewhere (the operator's scheduler skips unschedulable nodes). Running labs are not touched.
2. Wait until the labs on the node end: `kubectl get pods -A -o wide --field-selector spec.nodeName=<node>` (lab pods carry the label
   `laboratory.cybericebox.com/lab`; a VPN or gateway pod stays as long as its group has a lab on the node, so wait for the group).
3. `kubectl drain <node> --ignore-daemonsets --delete-emptydir-data` once only platform pods and DaemonSet pods are left; do the work; `kubectl uncordon <node>`.

To empty a node before the labs end, an operator can accept the cut explicitly: suspend the group (`LabGroup.spec.suspended: true`, devices stop, snapshot-backed
ones take an exit snapshot), or delete the pod with `kubectl delete pod`, which bypasses the budget (eviction goes through it, deletion does not). A node
that is lost (not drained) is handled by "Node loss" in "Device state persistence".

**Status.** `Lab.status.scheduling` and `LabGroup.status.scheduling` show the place in the queue:

```bash
kubectl get labs -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase,GROUP:.status.scheduling.group,POS:.status.scheduling.position,OF:.status.scheduling.length,WHY:.status.scheduling.reason,PENDING:.status.scheduling.pending
```

`group` is the deploy group, `position` the place among the objects that still have pods to dispatch
(0 when all are dispatched), `length` their number, `reason` why the next pod waits (`InFlightLimit`,
`WaitingForGroup` with `message` "waiting for group X", `WaitingForTurn`, `PreparingImages`, `TenantQuota`,
`InsufficientResources`, `DoesNotFitNow`, `NoSchedulableNodes`, `Withdrawn`; and `NodeStarting` when every pod is sent and
some wait for a node, see "Maximum capacity and node autoscaling"), `pods`/`pending` the pod counts. A Lab is in phase `Queued`
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
| `packingReservePercent` | `15` | hidden packing reserve of every lab node (0-49): the operator admits and the agent reports net of it |
| `packingReserveCpu` / `packingReserveMemory` | `"0"` / `"0"` | absolute packing reserve of every lab node; replaces the percentage for that resource when not `"0"` |
| `labScheduler.enabled` | `true` | a second kube-scheduler with the bin-packing profile for lab pods (see "Bin-packing scheduler") |
| `resourceCheck` | `true` | `false` skips the free-resource check |
| `agingSeconds` | `0` | a group waiting longer than this has the room it needs kept for it (the oldest one only); `0` = no aging (the default: enable once reservations scope the aged room) |
| `prepull.enabled` | `true` | prepull the images of a group |
| `prepull.timeout` | `5m` | dispatch goes on after this long |

`reservations.endingNoticeSeconds` (default `300`, `0` = no notice): the Ending condition comes this long before the end of a
reservation.

The operator needs `get/list/watch` on nodes; the chart's ClusterRole has them.

## Maximum capacity and node autoscaling

The operator knows nothing about an autoscaler. It knows two capacities of the lab nodes:

- **Current**: the room for labs of the lab nodes that exist and are schedulable (Ready, not cordoned, `node-agent-ready`,
  taints tolerated). A node that boots, a node a cluster autoscaler removes (taint `ToBeDeletedByClusterAutoscaler`) and a
  NotReady node do not count.
- **Maximum**: a **budget** for the autoscaled lab nodes, the nodes with the label `cybericebox.com/node-group` (infra sets it
  when a node of an autoscaled group joins). `capacity.budget.cpu` / `memory` is what those nodes may give together at most,
  as infra renders it: **the number of nodes × (allocatable − the DaemonSets' requests)**, no percentage taken (for example
  3 × m7i.xlarge: `9885m` and `44757612Ki`). The operator takes its platform and packing percentages itself. The fixed nodes
  (the controller, any node without the label) count with their real room only. So:
  - maximum = the real room of the fixed nodes + the larger of the real room of the autoscaled nodes and the budget;
  - virtual room = the budget less what the autoscaled nodes that exist give, counted the same way as the budget: for every
    autoscaled node of the lab pool, cordoned, NotReady, booting and the ones the autoscaler removes included (the autoscaler
    counts them in its totals), its allocatable less its DaemonSet pods; per resource, never below zero. A cluster at its
    budget therefore has no virtual room, whatever platform pods run on the autoscaled nodes. The operator then keeps its
    platform reserve percent and packing reserve percent on the virtual room, as on a real node (the absolute per-node
    reserves are not taken: the number of nodes to come is not known).

  Without a budget (both values empty, the default) the maximum is the current capacity.

**Admission against the maximum.** The placement simulation of the scheduler has the real nodes first, then the virtual room
as one aggregate room: a group may span nodes, and the cluster autoscaler picks which nodes it adds (`least-waste`), so the
placement on the virtual room is only an admission estimate. A group that fits real and virtual room together is sent whole;
its pods beyond the real room stay `Pending` (`PodScheduled=False`), and a cluster autoscaler, if infra installed one, adds
nodes for them. On the next passes these pods hold the virtual room they wait on, so no more than the maximum is ever sent.
With no lab node at all the queue goes on while virtual room is left (scale from zero). `DoesNotFitNow` and the
`GroupTooLarge` event come only for a group beyond the maximum. While virtual room is left no pod is failed with
`DoesNotFit` (a single device is capped by `limits.device.*`, and every node type infra uses is larger); without it the
per-node rule above holds as before. `CapacityShortfall`, the cleanup of unreserved work (spec decisions 6 and 19) and the
agent's `GetCapacity` use the maximum too; the condition message says how much of the capacity is on nodes not yet present. A
percentage tenant quota (`50%`) is a share of the maximum: the allocatable of the nodes that exist plus what the budget still
allows. `GetCapacity` also reports `current_*`, the same capacity without the budget's room; while there is virtual room its
largest device is the device limit (`limits.device.maxCpu` / `maxMemory`) or the largest real node, whichever is larger.

**Waiting for a node.** A sent pod **waits for a node** only when a budget is set, it has no node, and either the operator's
own placement sent its group partly onto the virtual room (recorded at dispatch, `status.scheduling.onBudget`: whatever the
scheduler says about the nodes that exist, so also with no lab node at all or while the only new node boots), or the scheduler
says the nodes lack resources a new node brings (`PodScheduled=False` with `Insufficient cpu`, `Insufficient memory` or `Too
many pods`). The placement is decided per group: when a group is placed partly on the real room and partly on the virtual room,
every pod of it is on the budget, so a pod of it that got the real room but still has no node (for any reason, a taint
included) also waits, and the group is withdrawn at the node timeout. A pod that no node of any autoscaled group can take (a
node selector or a taint no node template has) is withdrawn and queued again with its group at every node timeout, with the
scheduler's message, instead of failing: the scheduler cannot tell it from a node that is still coming. An
object whose pods are all sent and some wait so shows `status.scheduling.reason: NodeStarting` ("n pods wait for a node since
T") and gets one Normal event `NodeStarting`; the gauge `laboratory_pods_waiting_for_node` counts such pods. Such a pod holds
**no in-flight slot** (it pulls and runs nothing yet), so a group larger than `scheduler.maxPods` goes out whole within a few
passes and the autoscaler sees all of its demand at once; the budget still bounds it. The startup timeout does not fail it;
once it has a node, `scheduler.startupTimeout` counts from the moment it got the node. Any other pod without a node (no
budget, or a taint, a node selector, `cybericebox.com/tun`, a scheduler error) is failed after `scheduler.startupTimeout` as
before, reason `Unschedulable` with what the scheduler said.

**Gang commit.** A group must start as one commit. When a sent pod of a group still waits for a node (above)
`capacity.nodeTimeoutSeconds` (900) after the commit's last dispatch, the whole commit is withdrawn: every pod record goes back
to `Queued`, and the operator deletes every pod the commit made, the running (Ready) ones too. Its room, tenant quota and reservation
usage are free at once. The group keeps its place in the order and waits `Withdrawn` ("not every pod got a node within 15m0s;
withdrawn (attempt n), retry at T; the scheduler said: ..."; every record keeps that in `status.scheduling.schedulerMessage`)
for 1, 2, 4, 8 ... minutes, at most the node timeout, then it is sent again; retries do
not end. Each withdrawal gives a Warning event `Withdrawn` and counts in `laboratory_gang_withdrawals_total`. After three
withdrawals in a row the tenant gets the condition `GroupsWithdrawnRepeatedly` naming the groups, so the admin sees a stuck
group; it goes once the group starts. A pod that got a node and then fails (image pull, crash loop) keeps the per-pod rules
above, so a broken image never loops its group. A Ready pod never makes its group due, and a pod that was Ready once and lost
its node later is never withdrawn; but a Ready pod of a commit that is withdrawn goes with it. A commit is what was sent within
one node timeout of the earliest waiting pod: a lab that joins its deploy group later neither joins it nor moves its deadline,
and labs of the group sent long before are left alone.

Before it deletes a withdrawn pod the operator keeps the native identity the node-agent reported for it in the device's
`status.runtimeInventory`, as a lab stop does, so a later stop of the lab waits for its release. A withdrawn device that left
no such identity (its pod never got a node) holds a lab stop back at most 2 minutes; the stop then goes on and the Lab gets the
condition `WithdrawnRuntimeUnverified` naming the devices. A group stop ignores the `Queued` records of its stopped labs
(withdrawn, or stopped before their turn): they are never dispatched and hold nothing.

Without a budget no pod ever waits for a node, so no group is withdrawn: the behaviour is that of the scheduler before the
budget (a pod without a node fails `Unschedulable` after `scheduler.startupTimeout`).

**Breaker.** After a withdrawal, new groups go on the current nodes only (no virtual room) until a lab node becomes schedulable
that was not schedulable at the withdrawal, or one node timeout passes. A group that needs virtual room waits
`InsufficientResources` meanwhile. The gauge `laboratory_capacity_breaker` is `1` and the `CapacityShortfall` message adds "no
new lab node came within 15m0s: admission uses the current nodes only". The breaker only keeps new groups off the virtual
room: the reservations, the shortfall and the cleanup of unreserved work still count the maximum, so a temporary breaker never
evicts unreserved work, and the agent keeps reporting the maximum. The rest of a group that already has pods out may still be
placed on the virtual room. The breaker lives in the operator's memory: a restart forgets it, which allows one more try.

**What a cluster autoscaler must have** (infra installs it; the laboratory chart has no autoscaler code or values):

- a node template that matches the lab pods, or it never adds a node for them. On AWS, with scale from zero, these tags on
  each lab autoscaling group (the node labels and taint themselves are set at join):
  - `k8s.io/cluster-autoscaler/node-template/label/laboratory.cybericebox.com/node-agent-ready` = `true` (lab pods select it;
    the node-agent sets it on the node once it runs);
  - `k8s.io/cluster-autoscaler/node-template/label/cybericebox.com/lab` = `true` and
    `k8s.io/cluster-autoscaler/node-template/label/cybericebox.com/node-group` = `<group name>`;
  - `k8s.io/cluster-autoscaler/node-template/taint/cybericebox.com/lab` = `true:NoSchedule`;
  - `k8s.io/cluster-autoscaler/node-template/resources/cybericebox.com/tun` = the node-agent's TUN slots
    (`nodeAgent.devicePlugin.tunSlots`, default `1000`): pods of the TUN profile request `cybericebox.com/tun: 1`, the only
    extended resource lab pods request, which the node-agent's device plugin advertises only once the node runs;
  - the node's allocatable `ephemeral-storage`, if the autoscaler does not take it from the launch template
    (`k8s.io/cluster-autoscaler/node-template/resources/ephemeral-storage`): device pods request
    `devices.security.ephemeralStorage` (2Gi) each;
- `--max-node-provision-time` below the node timeout (12m for 15m), so it gives up before the operator withdraws;
- scale-down through the `lab-group` PodDisruptionBudget: lab pods carry `cluster-autoscaler.kubernetes.io/safe-to-evict:
  "false"` (VPN and gateway pods of a group that already runs get it with their next template change), so only empty nodes
  go;
- if it ignores pods of the `laboratory-binpack` scheduler, `--bypassed-scheduler-names=laboratory-binpack`.

**Before enabling the budget, apply the chart's CRDs** (`kubectl apply --server-side -f charts/laboratory/crds/`; `helm
upgrade` never updates `crds/`). The gang commit writes `withdrawals`, `retryAfter`, `onBudget` and `schedulerMessage` into the scheduling
records of Devices and LabGroups; an older CRD prunes them, and a withdrawal then deletes nothing and holds nothing back. The
operator checks this at start: with a budget and an older CRD it runs without the budget (as if `capacity.budget` were empty),
logs an error and gives a Warning event `CapacityBudgetDisabled` on the Device CRD; the agent makes the same check and then
advertises no budget either, so tenants never plan against a budget the operator does not use. A read of the CRDs that fails
for a passing reason is retried for about 30 s; only a missing field, a missing CRD or a refused read turns the budget off.
Apply the CRDs and restart the operator and the agent.

**Infra and the budget must count the same DaemonSets.** The operator compares the budget with the autoscaled nodes'
allocatable less the requests of the DaemonSet pods that run on them (node-agent, Cilium, kube-proxy and any other). Infra
must compute the budget from the same DaemonSets with the same chart values, and render it again when their requests change
(a new node-agent or Cilium size): otherwise the virtual room drifts by the number of nodes times the difference. Static and
mirror pods count in neither.

**Values** (chart `capacity.*`):

| Value | Default | Meaning |
|---|---|---|
| `budget.cpu` / `budget.memory` | `""` | room for labs the autoscaled nodes (label `cybericebox.com/node-group`) may give at most; both or neither; empty: no budget |
| `nodeTimeoutSeconds` | `900` | the gang commit: every sent pod of a group must have a node this long after the group's last dispatch |

**Smoke test (AWS, desired 0 → 1 → 0).**

1. Start: the lab autoscaling groups are at 0; the agent's `GetCapacity` is about the controller's room plus the budget (less
   the packing reserve).
2. Start a reservation and a deploy group that does not fit the controller: the group is sent, its pods are `Pending`, the
   Lab shows `NodeStarting`, the autoscaler logs a scale-up and the group's desired capacity is 1.
3. The node joins and gets `node-agent-ready`: the pods bind and become Ready, `NodeStarting` clears, the pods carry
   `safe-to-evict: "false"`.
4. Delete the group: after about 10 + 10 minutes the autoscaler removes the node, the desired capacity is 0, the instance is
   terminated and the Node object is gone.
5. A group larger than the controller's room plus the budget gets `GroupTooLarge` and no scale-up.
6. Set the group's max to 0 and start a gang of two labs, one that fits the controller and one that needs a new node: at 15m
   the whole gang is withdrawn (both workloads deleted, `Withdrawn`, attempt 1), `laboratory_capacity_breaker` is 1 and a
   new group that needs a node waits. Restore the max: on retry the node comes, the gang starts whole and the breaker clears.

---

## Workload names and user labels

**Names.** The name of a device's workload never contains the lab. Every container device gets a short
random code (3 characters, 4 after repeated collisions) when it is created, kept in `Device.spec.code`; the
Deployment is named `<device>-<code>` (a pod of it adds the ReplicaSet hash and 5 characters, at most 63 with a
device name of at most 35 characters), a snapshot-backed device's bare pod `<device>-<code>-<incarnation>`, and its
web Service, which is also its host label, `<device>-<code>`. The code is unique in the group namespace (checked
against the other devices' codes and the Services), so two labs may have a device of the same name in one
namespace. The relation between a pod, its device and its lab goes through owner references and the labels
`laboratory.cybericebox.com/lab` and `/device`, never through the name. The Device resource itself is still named `<lab>-<device>`, as the per-device
env Secret `<lab>-<device>-env`.

**User labels.** The labels of a Lab (and of a LabGroup) are copied onto its Devices and onto the pods of
those Devices (the VPN and gateway pods for a LabGroup), except the operator's own: every label with the prefix
`laboratory.cybericebox.com/` (`deploy-group` among them) and the keys `app`, `pod-template-hash` and
`controller-revision-hash`. They are kept in sync: a changed or removed label follows on the Devices and on live
pods, which are patched in place (metadata only, so nothing restarts); the copied keys are listed in the
annotation `laboratory.cybericebox.com/user-labels` so only they are removed.

---

## Maintenance windows

A **MaintenanceWindow** is how the cluster operator announces that maintenance (draining nodes, upgrades, restarts) may happen, so
the platforms that use the cluster can plan around it. The agent refuses and delays nothing during a window; the **backend** reads it
and its calendar gives this agent no capacity inside the window (or the capacity the window leaves), and flags the reservations that
overlap it.

```yaml
apiVersion: laboratory.cybericebox.com/v1alpha1
kind: MaintenanceWindow
metadata:
  name: kernel-upgrade-2026-10
spec:
  from: "2026-10-12T22:00:00Z"
  to: "2026-10-13T02:00:00Z"      # optional: empty = open-ended (until the object is deleted or edited)
  reason: kernel upgrade of the lab nodes   # shown to tenants, at most 500 characters
  tenants: [acme]                 # optional: Tenant names (client certificate CNs); empty = all tenants
  capacity: { cpu: "2", memory: 4Gi }   # optional: what the window leaves to the tenants; empty = nothing (the usual case)
```

The object is cluster scoped and created with `kubectl` by the cluster operator; the schema refuses a `to` that is not after `from`
and a `capacity` that names anything but `cpu` and `memory` (a resource it does not name is zero). `kubectl get maintenancewindows`
shows from, to and reason. The chart installs the CRD (`helm upgrade` does not update `crds/`: apply it first, as in the upgrade
notes) and gives the agent read-only access (`get`, `list`, `watch`); the operator does nothing with it. Without the CRD the call
fails with `FailedPrecondition` (never an empty list).

**Agent API.** `ListMaintenanceWindows(ListMaintenanceWindowsRequest{include_past}) returns (MaintenanceWindowList)`, over the same
mTLS as the rest. It returns the windows that apply to the **caller's tenant** (a window with no `tenants` applies to everyone; a
window that names other tenants is never listed and its tenants are never revealed), soonest `from` first, each as:

| Field | Meaning |
|---|---|
| `name` | the object name |
| `from_unix_ms`, `to_unix_ms` | the window; `to_unix_ms` is 0 for an open-ended window |
| `reason` | the operator's text |
| `state` | `Upcoming`, `Active` or `Past`, as of the call |
| `all_tenants` | true when the window applies to every tenant |
| `has_capacity`, `capacity_cpu_millicores`, `capacity_memory_bytes` | what the window leaves to the tenant; `has_capacity` false means nothing |

Windows that are over (`Past`) are left out unless `include_past` is set. There is no change feed: poll the call (the backend does,
every minute). The windows are **not** part of the Monitoring stream, which is a journal of per-tenant records; adding a record
kind that is filtered by a tenant list would complicate its deltas for little gain.

---

## Bandwidth protection

Off by default (`bandwidth.enabled`). On, no participant, lab or group can saturate a node or the cluster network. Per node and per direction

    U = the node's bandwidth - the system reserve - the reserves of the platform pods that run on that node

and U is divided into classes: the **entry** (the WireGuard demux and the L7 proxy) gets 50 %, the **internet** (the gateways) 25 %, **Geneve** (traffic between nodes) 25 % (`bandwidth.shares`). Inside a class everyone gets a fair share, never more than a cap: a participant 50, a lab's internet 50, a lab's Geneve traffic 100 Mbit/s (`bandwidth.caps`). **Nothing inside a node is limited**; only what crosses nodes and what goes to the internet:

| What | Where | How |
|---|---|---|
| Geneve between nodes | the node-agent, on the OVS bridge | an OVS meter per lab that is split between nodes (table 7) and one meter for the node (table 8), chained: `t6 (local delivery) -> t7 meter(lab) -> t8 meter(node) + tunnel outputs`. Local delivery is never metered |
| the entry (participants of the VPN and of the web) | the demux (userspace: bytes per session, both directions; tc and xdp: the packet rate of the kernel fast path) and the L7 proxy (paced reads and writes, WebSockets too) | a token bucket per participant and one for the class; the share is the class divided by the participants active in the last seconds, up to the cap |
| the internet | the gateway pod, on each lab interface | a token bucket filter on the egress (to the lab) and a police on the ingress (to the internet) |

The label `cybericebox.com/node-bandwidth` gives a node's bandwidth in Mbit/s (`1562`, or `1562M`, `10G`); the cloud code sets it from the instance type, the Proxmox inventory from the NIC. A node without it is `bandwidth.defaultMbit` (1000). A platform pod keeps bandwidth for itself with the annotation `cybericebox.com/bandwidth-reserve` (Mbit/s), taken off the node it runs on and no other. This chart annotates the registry (100) and the agent (20); the backend and the frontends declare theirs in the platform chart. All numbers are estimates until the load test.

Who computes what: the **operator** works the gateways' ceilings out when something changes (a node's label, a pod that appears, moves or goes, a lab's gateway) and writes them as the annotation `cybericebox.com/bandwidth-limits` of each gateway pod, and the entry limits of every node into the ConfigMap `laboratory-bandwidth` in the namespaces of the demux and the L7 proxy (one key per node). The pods read them from mounted files and apply them themselves. The **node-agent** works the Geneve ceilings of its own node out from the same numbers (it already reads its node and the pods on it), on events only; it touches an OVS meter only when a rate changes (`mod-meter`; a delete would delete the flows that use the meter). The operator needs no right to write to nodes, and none to patch the demux or L7 pods.

Switching on: a pod made before has no volume for its limits, so the **gateway pods that exist keep no ceiling until they are recreated** (changing the template of a Deployment would restart a team's gateway). The demux and L7 pods get the ConfigMap volume at the next rollout of their DaemonSet/Deployment. Switching off: set the shares and caps to 0, the pods then limit nothing; the meters of the node are removed when the node-agent sees zero limits.

Checks on a node (the bridge is `br-ovs`):

    ovs-ofctl -O OpenFlow13 dump-meters br-ovs          # meter 1 = the node, 16 + VNI = a lab
    ovs-ofctl -O OpenFlow13 meter-stats br-ovs          # packet_count / byte_count of the dropped band
    ovs-ofctl -O OpenFlow13 dump-flows br-ovs table=7   # the lab meters
    ovs-ofctl -O OpenFlow13 dump-flows br-ovs table=8   # the node meter and the tunnel outputs
    kubectl -n <group namespace> get pod -l laboratory.cybericebox.com/component=gateway -o jsonpath='{.items[*].metadata.annotations}'
    kubectl -n laboratory-wg get configmap laboratory-bandwidth -o yaml

Known limits (see `docs/research/2026-10-28-lab-bandwidth-plan.md`, "Open for owner"): the OVS meter counts the frame before the Geneve headers, so the Geneve class is scaled by `bandwidth.geneveOverheadFactor` (0.85); a flooded frame is copied to every remote node while the meter counts it once, so a lab's rate is divided by the number of remote nodes; the xdp mode of the demux bypasses queueing disciplines, so the kernel fast path limits by packet rate (the packets of a session are assumed 1500 bytes), not by bytes, and has no class bucket; the userspace demux adds a hop that is not counted. Nothing here is a guarantee: it is a ceiling.

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
| Reservations | `CreateReservations`, `ListReservations`, `UpdateReservations`, `DeleteReservations` |
| Devices | `ResetDevices`, `RescueDevices` |
| Other | `Ping`, `Monitoring` (stream), `GetCapacity`, `GetFeatures`, `ListMaintenanceWindows`, `PrewarmImages` |

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

**Answers.** A mutating call returns one `ItemResult{ref, state, error, retryable, reason}` per item, in request order (selector
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

**Pull credentials are secrets only, too.** `LabVariant.registry_credentials` (at most 8, one per registry host, no
scheme or path; a password is required) become the write-only `kubernetes.io/dockerconfigjson` Secret
`<lab>-registry` (owned by the Lab; delete-then-create on every `CreateLabs` of the lab), and the agent sets
`Lab.spec.imagePullSecret` to its name. Without credentials the field stays empty; an `imagePullSecret` in `spec_json`
is always ignored. Credentials are never returned, logged or put in a status.

**Variants are not stored.** `CreateLabs` expands every variant into each Lab: the spec is copied into the Lab CR and
the variant's variables are merged with the lab's own (the lab wins per device and variable name) into the Secrets.

**State persistence** is a property of a device in the topology, set at creation and immutable (a lab's spec is never
updated): `devices[].persistence {enabled, debounce}` in `spec_json`, both optional, with the
platform defaults from the chart. `enabled: true` is refused when `devices.statePersistence.enabled` is off in the
chart (the agent reads it as `STATE_PERSISTENCE_ENABLED`, the operator's name); `debounce` must be positive. The excluded paths and the
quota are cluster settings of the chart (`devices.statePersistence.maxFileSize`, default `256Mi`: bigger files are skipped; `devices.statePersistence.excludePaths`, default `/tmp`, `/var/tmp`, `/run`; and `writeQuota`, default `512Mi`: the most of a
participant's writes kept per device). They are configurable only in the chart and cannot be requested per device.

### Reservations through the agent

Every LabGroup and Lab created through the agent runs under a **reservation** of its tenant (spec decision 17); see
"Reservations" in the Scheduler section for what the operator does with them. There is no switch around it:
`GetFeatures` (and `Monitoring.features`) reports `reservations {required: true, ending_notice_seconds}`.

**The calls.** A reservation is `ReservationItem{id, from_unix_ms, to_unix_ms, cpu_millicores, memory_bytes, labels}`.
The id is any string of 1 to 64 characters, encoded into the name of the cluster-scoped `Reservation` like every other
id (the original in the `id` annotation); the agent stamps the tenant label and `spec.tenant`. Times are kept to the
second. `CreateReservations` with the same window and amount again is `EXISTS`, with another `FAILED`; an id another
tenant uses is "not available". `UpdateReservations` (by id or selector) moves `to` and the amount at any time and
`from` only while the reservation has not started; `to` must stay in the future, and the id never changes, so extending
a window or enlarging it is always an update (an `ENDING` reservation turns `ACTIVE` again when `to` moves past the
notice). `DeleteReservations` removes it: the groups that ran under it get the ending notice and are then deleted (the
operator's path for a deleted reservation). `ListReservations` lists the caller's own. A window must be at most 31 days,
`to` after `from` and in the future, cpu and memory positive and at most `MaxInt64/1024` (else `FAILED` for that item); the
quota sums saturate, so no amount can wrap around.

**The tenant's quota.** A create, or an update that enlarges or extends a reservation, is refused with reason
`OVER_QUOTA` when the tenant's reservations would reserve more at the same time (from now on) than its quota (resolved
against the maximum, as `GetCapacity` reports it). A tenant without a quota is not checked; a smaller amount is always
accepted.

**One agent replica for reservations.** The quota check and the fit check below are serialized inside one agent process
(a lock per tenant, a lock per reservation), not across replicas. Two replicas that take calls of the same tenant at the
same time may both let a reservation through the quota check, and nothing behind the agent checks the quota of
reservations again. A backend that uses reservations therefore runs the agent with **`agent.replicas: 1`** (the chart
default is 2 for availability; with one replica keep `podDisruptionBudget.minAvailable` at 0 or switch the budget off, or
a node drain waits for it).

**Groups and labs.** `LabGroupItem.reservation_id` and `LabItem.reservation_id` name the reservation; the agent writes
the reserved label `laboratory.cybericebox.com/reservation` from the field (never from client labels: the prefix stays
reserved). A lab's reservation is independent of its group's. `LabGroup` and `Lab` (lists and `Monitoring`) return the
id. Per item, in request order (what an accepted item takes counts for the next):

| Reason | When |
| --- | --- |
| `RESERVATION_REQUIRED` | no `reservation_id` |
| `RESERVATION_NOT_FOUND` | no reservation with that id in the caller's tenant |
| `RESERVATION_ENDED` | the reservation takes no more work: ended, being deleted, past `to`, or its tenant is gone (a `PENDING` one is accepted: the operator holds the work until the window opens) |
| `RESERVATION_MISMATCH` | the item's deploy group already runs, or is created in the same call, under another reservation (one deploy group, one reservation) |
| `RESERVATION_FULL` | the item does not fit what is left of the reservation: "not enough room in this reservation (needs ..., left ...)", `retryable` |

What is left is the amount less the requests of the reservation's LabGroups (VPN and gateway, the chart's sizes when
the group sets none) and Labs (their container devices, counted as the scheduler counts them) that are neither being
deleted nor stopped; queued objects count too. A create of an object that exists is idempotent and is not checked again.
The check is made by one agent replica under a lock per reservation; two replicas may let one extra item through, and
the operator then holds it (`ReservationFull`): the operator is the guarantee, the agent only refuses early (run one
replica, see above).

**Attach.** `UpdateLabGroups` / `UpdateLabs` with `changes.reservation_id` puts an object that runs under no
reservation under one of its tenant (not ended), without a fit check; only the label changes (no spec change, no pod
restart). An object that already runs under another reservation is refused with `RESERVATION_MISMATCH`, also through a
create that names another one.

**Monitoring.** `MonitoringUpdate.reservations` holds `Reservation{id, from, to, amount, labels, uid, status}` records
of the subscriber's tenant (the selector applies to their labels): the snapshot has all of them (ended ones until the
operator removes them, a day after the end), a delta the ones whose spec, phase, notice, conditions or usage changed,
the usage (`used_*`, `groups`, `used_at`) at most every 30 s per reservation. A removed reservation is a deleted key
`{kind: "Reservation", name: <id>}`. `status.ends_at_unix_ms` and `ending_reason` say when the groups under it are
deleted while an end is announced: `WindowClosing` at `to`, `TenantGone` one notice after it was given (or `to`), and
`AmountReduced` (`exceeded`, the phase may stay `ACTIVE`) one notice after the reservation was made smaller than what
runs under it, for the groups created before that.

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
  A reference that names the platform's registry or image cache (`localhost:5035/...`) is refused outright: a
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
  uses them for a group created without an explicit size (see "Sizing of a group's own pods"); existing groups keep what they have (changing a value never restarts a live VPN). `GetCapacity`
  reports their sum as `group_overhead_cpu_millicores` and `group_overhead_memory_bytes`: what one group adds to its labs, for sizing a reservation.
- **Resource quota.** The scheduler caps the sum of the CPU and memory requests of the tenant's dispatched pods (started, starting
  or failed). A pod that would pass the cap waits, the lab's `scheduling.reason` is `TenantQuota`, and other tenants' pods go on.
  A percentage is a percentage of the CPU and memory the lab nodes allocate.
- **Capacity.** `GetCapacity` and the `capacity` of the `Monitoring` stream are the caller's view only: its quota (if any), what its
  pods reserve (the sum of their requests) and use (metrics-server, when installed), and what is free (quota minus reserved). The quota
  shown is net of the hidden packing reserve (see "Packing reserve"). No
  cluster totals are exposed. The same reserved and used totals are in `Tenant.status` (refreshed by the agent):
  `kubectl get tenant platform -o yaml`.
  The one number about the cluster layout is the **largest device the agent can place** (`has_max_device`, `max_device_cpu_millicores`,
  `max_device_memory_bytes`): per resource, the largest allocatable of a lab node (the nodes of the lab node selector, cordoned,
  NotReady and rebooting ones included, whether their node-agent runs now or not) net of the scheduler's platform reserve (`scheduler.platformReserveCpu/Memory` off
  the node first, then `platformReservePercent` of the rest, per node) and of the packing reserve. It is an upper bound per resource: the scheduler fails a pod (`DoesNotFit`) only when
  no single node holds its CPU and memory together. It is one amount: no node list, no node names, no per-node free room reach a
  tenant. The backend uses it to refuse planning a device that no node could hold; the per-node math stays in the agent and the
  scheduler. The two values may come from different nodes, so it can overstate a device that needs both at once; the
  scheduler fails such a device with `DoesNotFit`. It is not limited by the tenant quota, and it is absent (`has_max_device` false) for an
  agent that has no schedulable node or an older one.
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
  enforces it: `limits.device.maxCpu` (2000m) and `maxMemory` (4Gi) per device; `limits.lab.maxDevices` (20 devices; switches and
  hubs count, see "Lab ceilings" below); per LabGroup `limits.group.maxLabs` (50) and the optional sums over all its labs `limits.group.maxCpu` / `maxMemory`
  (`"0"` = unlimited); `limits.tenant.maxLabs` (0 = unlimited; the tenant's resource quota still applies). There is no per-lab resource
  cap. Devices are planned at their requests (the CPU limit above the request is a burst and is not counted), so a group's sum is what it reserves. A device's resources are its
  CPU request, else its CPU limit, else the planning profile `limits.device.defaultCpu` (100m) / `defaultMemory` (256Mi) (memory: limit, else request), which is also what the
  scheduler gives a device without resources. A variant over a device or lab cap is `InvalidArgument` naming the variant, the device and both
  numbers. Over a group or tenant cap the new items are `FAILED` (`ResourceExhausted` reason, not retryable); an item whose lab exists is
  not new. The counts are checked when the call arrives. Labs created directly with kubectl are not checked.
- **Monitoring** is cut to the tenant before the user selector.

### Required values and defaults

**The inputs.** The images and the chart take the minimum: `operator.publicVPNEndpoint`, `operator.baseDomain` and `operator.supportEmail` are
required (the render fails when one is empty), with an ACME issuer also `certManager.email`; everything else has a default that suits any
installation (an override stays possible, nothing is required). **One release tag** drives every image: `image.tag` (empty = the chart
appVersion); `<component>.image.tag` pins one image to another build. The images of the VPN, gateway and netconfig pods are passed to the
operator as `VPN_IMAGE` and `NETCONFIG_IMAGE` (the gateway is the VPN image, one image with two commands: `GATEWAY_IMAGE` is passed only when
`inetGateway.image` differs), and the operator refuses to start without them and without `SUPPORT_EMAIL`. `agent.domain` is derived
(`ctl.<operator.baseDomain>`) unless set, and the agent reads the lab domain and the VPN endpoint it reports from the operator's values.
**One name for one setting**: what both the operator and the agent read (the scheduler, the device limits, the state persistence and image cache
settings, the lab node selector and tolerations, the pod sizes, the pull secrets, the lab domain) is rendered once by the chart
(`laboratory.sharedEnv`) under the operator's names (`SCHEDULER_*`, `DEVICE_*`, `STATE_*`, `IMAGE_CACHE_*`, `LAB_NODE_SELECTOR`, ...), and the agent
reads the proxy's limits under the proxy's names (`ACCESS_TOKEN_MAX_TTL`, `SESSION_IDLE_TTL`, `SESSION_MAX_TTL`).
**Constants of the platform** are only ports INSIDE the cluster network, baked into the images and not values: the WireGuard port 51820 (the
VPN pods, the wg-demux and the `targetPort` of its LoadBalancer Service), the HTTPS port 8443, the probe ports of the proxy (8081 for L7, 8082 for the wg-demux), the metrics port of the wg-demux (8083), and
the registry relay port 5035 on every node. Everything else that is a tuning knob stays an **optional value** (and an optional environment
variable of the image with the same default, so it can be overridden without a rebuild): the agent's monitoring sizes (`agent.monitoring.*`),
`vpn.statsInterval`, the node-agent's host paths, socket, bridge, health port and device-plugin directory (`nodeAgent.ovsSocket`,
`ovsRunHostPath`, `ovsDBHostPath`, `grpcSocket`, `ovsBridge`, `healthPort`, `devicePlugin.dir`), `agent.namespace`, the priority class names
(`priorityClasses.<class>.name`), and, as environment variables only, `OPERATOR_ADMISSION_POLICY_TIMEOUT`, `OPERATOR_NAMESPACE`,
`OPERATOR_SERVICE_ACCOUNT`, `AGENT_SERVICE_NAMESPACE`, `AGENT_SERVICE_ACCOUNT`, `AGENT_RELEASE_NAMESPACE`, `AGENT_PULL_SECRET_NAMESPACE`, the
node-agent's `TUN_CHECK_PATH`, `CGROUP_ROOT`, `STATE_WORK_DIR`, `IMAGE_PULL_CONCURRENCY`, `IMAGE_PULL_TIMEOUT`, the registry address
(`STATE_REGISTRY_ADDR`, `AGENT_REGISTRY_ADDR`, `IMAGE_CACHE_PREFIX`, passed by the chart for the release namespace) and the TLS files of the proxy
and the agent (`TLS_CERT_PATH`, `AGENT_TLS_CERT`, ...). A values file that still sets one of the in-network ports (`operator.vpnServicePort`,
`registry.forwardPort`, `proxy.l7.listen`, `proxy.l7.healthPort`, `proxy.wg.listenPort`, `proxy.wg.healthPort`) fails the render ("... was
removed") instead of being ignored. Where a code default remains it mirrors the value in `values.yaml` (for example `registry.cache.pinTTL` 30m,
`nodeAgent.criSocket` `/run/k0s/containerd.sock`).

**The WireGuard port outside the cluster** is `proxy.wg.publicPort` (default 51820): the UDP port of the LoadBalancer Service, where clients connect
(the Service forwards it to 51820 inside). `operator.publicVPNEndpoint` is a host, or host:port. Without a port, `proxy.wg.publicPort` is appended, and
the result is what the operator and the agent advertise (`PUBLIC_VPN_ENDPOINT`; a binary without the chart also appends 51820 to a bare host). With a
port, the address is advertised as it is: that is for a front (a cloud load balancer, a firewall) that maps its public port to `proxy.wg.publicPort`.

**The WireGuard port of a VPN pod is reachable from the outside only.** The WireGuard listener of the VPN pod binds every address, so, left alone, a
device of the lab could send packets to it over a lab interface. At start, BEFORE the WireGuard interface exists, the VPN pod installs two INPUT rules for
its UDP port (the iptables of the pod, the IPv6 ones too when the pod has ip6tables): `-i <uplink> -p udp --dport 51820 -j ACCEPT`, then
`-p udp --dport 51820 -j DROP`. The uplink is `EXTERNAL_INTERFACE` (`eth0`, the pod network: the wg-demux of the proxy delivers the clients' packets
there). A packet to the port that arrives on any other interface, the lab interfaces `lab<N>` and the tunnel interface among them, is dropped, whatever
its source or destination address is. The CiliumNetworkPolicy of the VPN pod already lets only the proxy pod reach the port from the pod network; it
does not see the lab interfaces, which is why the rule is in the pod itself. The internet gateway pod has no listening port at all (no metrics, no
probes, no server), so a lab device finds nothing to reach there; its forwarding filter is unchanged. The ACME directory of the issuer is `certManager.acme.server`
(empty: the Let's Encrypt preset of `certManager.acme.presets` chosen by `certManager.staging`).

Also explicit values (each default mirrors the code default): `operator.groupNetworkPolicy.enabled` (true, the default-deny baseline of every
group namespace; `operator.networkPolicy.enabled` is the policy of the operator pod itself), `devices.statePersistence.retentionInterval` (10m),
`agent.id` (`laboratory-agent`) and `agent.tenantStatusInterval` (30s). The VPN pod samples its traffic statistics every `vpn.statsInterval` (30s).

**Defaults of the binaries that the chart also sets.** The operator's defaults are the safe ones the chart has: `OPERATOR_REQUIRE_ADMISSION_POLICY`,
`AGENT_ENABLED` and `PROXY_ENABLED` are on, and `--leader-elect` is on (`--leader-elect=false` and `OPERATOR_REQUIRE_ADMISSION_POLICY=false` for a manager
on a developer's machine, as `make run` does). The proxy's `MAX_CONNECTIONS` and `LIVE_TOTAL` default to 2500, the values the chart and its memory limit
(512Mi) were measured with.

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
4. **Where the keys are.** The agent keeps them in the Secret `tenant-<name>-access-keys` of `laboratory-access-keys` (one entry per key id,
   the PEM public key); the proxy reads them from there (it watches that namespace and nothing else). The agent has no role in `laboratory-tenants`.
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

**Limits.** Ids and deploy keys at most 64 characters, `deploy_after` at most 32 keys, at most 5000 items per call, a request of at most 4 MiB (`MaxRecvMsgSize`; a bigger one is refused with `ResourceExhausted` before it is decoded, so split a very large batch into several calls) and an answer of at most 64 MiB (`MaxSendMsgSize`; the Go client sets
the matching call options), bounded internal concurrency (16 calls to the Kubernetes API per request).

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

## Nodes

### A new node and the CNI config

The node-agent's init container `install-cni-conf` wraps the node's real CNI config (Cilium's) with `cni-gate` in `00-cybericebox.conflist`. On a fresh node
Cilium can need minutes (image pull, agent start, scale-out on a cloud), and until its config exists the init container **waits and retries every 5 s**; the node-agent
pod stays `Init:1/2` and the log says `install-cni: waiting for base CNI config`. It never writes a substitute config on its own: a pod must not get an address from anything but
the cluster's CNI, because a node with a different pod network for some of its pods breaks routing and can collide with the pod CIDR. If a node stays in `Init` for a
long time, look at the CNI agent on that node (`kubectl -n kube-system get pods -o wide`), not at the node-agent.

`nodeAgent.cniFallbackTimeout` (default empty) is the explicit opt-in for a cluster that has no other CNI: with a Go duration such as `10m`, a built-in `ptp` + `host-local`
config (10.244.0.0/16) is written after that long without a base config. Leave it empty on any cluster with Cilium.

### The lab interfaces of the VPN and gateway pods

A team's VPN pod and gateway pod have one interface per lab (`lab<N>`, the lab's network index). The Deployment's pod template carries **no list** of them: the node-agent
derives the list from the group's `LabVPN` and `LabGateway` objects (the operator makes one per lab with a VPN or an internet leg; it lives until the VPN or gateway
process has cleaned up after the lab). It attaches a lab's interface to the **running** pod's network namespace when the object appears and takes it out
(OVS port and veth) when the object is gone. Adding or removing a lab therefore never changes the Deployment and never restarts the team's VPN or gateway pod: the tunnel and
the sessions stay. The VPN and gateway processes already wait for `lab<N>` to appear and react to their `LabVPN` / `LabGateway` objects (addresses, routes, allowed IPs,
NAT and egress rules), so nothing else is needed. A pod that is (re)created gets all the interfaces its group's objects list. Only the node-agent of the pod's node acts, and
only on its own node: after a pod moves to another node, its legs are made there.

Test plan on the stand (a group with VPN and internet on two labs):

1. `kubectl -n <group-ns> get pods -l laboratory.cybericebox.com/component=vpn -o wide`; note the pod name and IP, and the `RESTARTS`/`AGE` of the `vpn` and `gateway` pods.
2. Connect a participant with the client's WireGuard config and keep a continuous `ping` of a device of lab 1 running.
3. Create lab 2 (VPN on) in the same group. Expect: the `vpn` pod name, IP and AGE do not change; `kubectl -n <group-ns> exec deploy/vpn -- ip -br link` shows `lab2` next to `lab1`;
   `LabVPN labvpn-<lab2>` reaches `Ready`; the ping does not drop, and a device of lab 2 is reachable over the same tunnel.
4. Repeat with a lab that has the internet leg: the `gateway` pod does not restart; `ip -br link` in it shows the new `lab<N>`; the lab's devices reach the internet.
5. Delete lab 2. Expect: `lab2` disappears from `ip -br link` of the `vpn` (and `gateway`) pod, the OVS port is gone on the node
   (`kubectl -n laboratory-system exec ds/laboratory-node-agent -c ovs -- ovs-vsctl list-ports br-ovs` has no port named like the lab's leg), the pod name, IP and AGE are unchanged, the ping of
   lab 1 never dropped.
6. Create and delete five labs in a row: the pods do not restart at all. Check `kubectl -n <group-ns> get deploy vpn gateway -o jsonpath='{.items[*].metadata.generation}'` is the same before and after.
7. Delete the `vpn` pod by hand: the new pod gets `lab1` and any other lab's interface from the objects (a few seconds after it is Running), and `LabVPN` returns to `Ready`.
8. Restart the node-agent pod of the node (`kubectl -n laboratory-system delete pod <node-agent-pod>`): the pods and their interfaces stay; no leg is removed for a lab that still exists.

### Which nodes run labs

A lab pod (device, VPN, gateway) needs the node-agent of its node: the node-agent wires the device's network into Open vSwitch and its `cni-gate` plugin is
what gives the pod its interfaces. A pod that lands on a node without a working node-agent gets a plain Cilium address and no OVS wiring: a lab outside the
layer-2 isolation model. Two rules keep that from happening:

1. **The node-agent runs on exactly the nodes that can host lab pods.** Its DaemonSet has the same node selector and tolerations as the lab pods
   (`labWorkloads.nodeSelector`, `labWorkloads.tolerations`) and, in addition, tolerates Cilium's `node.cilium.io/agent-not-ready` taint so that it starts while the CNI
   comes up. There is no `excludeControlPlane` any more. A schedulable control-plane node (a controller+worker topology) gets a node-agent and takes labs when
   its taint is soft (`PreferNoSchedule`, as the stand and the AWS cluster role set it) or when `labWorkloads.tolerations` tolerate its taint, for example
   `{key: node-role.kubernetes.io/control-plane, operator: Exists}`. A node that labs avoid (a hard taint they do not tolerate, or another node selector) has no node-agent.
2. **A lab pod is placed only on a node whose node-agent is ready.** The node-agent sets the label `laboratory.cybericebox.com/node-agent-ready=true` on its own node once
   it is up (Open vSwitch is programmed, the gRPC socket listens) and removes it when it stops. The chart adds this label to the lab node selector (`LAB_NODE_SELECTOR` of the
   operator, `LAB_NODE_SELECTOR` of the agent too: the same value under the same name), so it is a required node selector of every VPN, gateway and device pod, and the scheduler and the capacity numbers count only
   marked nodes. A node whose node-agent stops (rollout, crash loop, removed) takes no new lab pods; pods already there keep running. After a hard crash the label stays until the pod restarts
   (a few seconds); a node-agent that is gone for good leaves a stale label, which you remove with `kubectl label node <node> laboratory.cybericebox.com/node-agent-ready-`.
   The node-agent may patch only that one label of its own node: the ValidatingAdmissionPolicy `laboratory-node-agent-label` (rendered with `operator.admissionPolicy.enabled`) refuses any other
   change and any other node, using the node name in the bound token of its pod.

Check a node: `kubectl get nodes -L laboratory.cybericebox.com/node-agent-ready` and `kubectl -n laboratory-system get pods -l app=node-agent -o wide`. A new node takes labs only after its
node-agent is `Running`.

### Label keys

Every key we own is under our domain `cybericebox.com`; the retired `cybericebox.io/*` is not ours and nothing here reads or tolerates it.
The chart and the code use `cybericebox.com/lab` (node label and the taint `=true:NoSchedule`), `cybericebox.com/edge`,
`cybericebox.com/node-group` (the budget of the autoscaled nodes) and the startup taint `cybericebox.com/node-starting`. The laboratory's
own keys stay: `laboratory.cybericebox.com/*` (node-agent-ready, lab-uid, device-uid) and the resource `cybericebox.com/tun`. The full table
is in `docs/specs/LABEL-KEYS.md` of the platform repository.

This is a clean switch, with no old-key compatibility, done while no labs run: infra moves the node labels, the taints and the autoscaler's
`--startup-taint` to the new keys in the same pass, right before the laboratory upgrade, and `labWorkloads.nodeSelector` and
`labWorkloads.tolerations` use `cybericebox.com/lab`. A lab node still labelled or tainted with the old keys takes no labs.

### The startup taint of a new lab worker

A new worker turns `Ready` about 10-15 seconds before its node-agent has registered the `cybericebox.com/tun` device and set the
`node-agent-ready` label. The Cluster Autoscaler sees a real node that cannot hold the pending lab pod and scales up once more. The fix is a
startup taint, in this order:

1. **laboratory first** (this release). The node-agent, the WireGuard demux and the proxy DaemonSet tolerate
   `cybericebox.com/node-starting` (`NoSchedule`). The node-agent removes the taint from its own Node when the three hold: the node-agent has
   labelled the node (`node-agent-ready=true`, set last, so the device plugin is up), the Node's allocatable shows `cybericebox.com/tun` above
   0 (the kubelet has the plugin registered; not waited for when `TUN_SLOTS=0`), and then it patches `spec.taints` with a JSON patch
   guarded by a test of the current list, retried when the node changed. A node without the taint is not touched, so this is safe to roll out
   before infra adds the taint. The admission policy `laboratory-node-agent-label` lets the node-agent remove this one taint and nothing else.
2. **infra second.** Lab workers are created with the taint `cybericebox.com/node-starting=true:NoSchedule`, and the Cluster Autoscaler runs with
   `--startup-taint=cybericebox.com/node-starting`, so it treats the node as not ready yet instead of as a node that fits nothing.

Rolled out the other way round, the node-agent of a tainted node would never start (nothing tolerates the taint) and the node would stay empty.

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

The routes of `accessport` do not stay in the pod's main table: cni-gate moves them, in the same CNI ADD, into table 100 and adds `ip rule from <accessport address> lookup 100` (priority 1000), so a default gateway of a lab interface never takes the replies to the L7 proxy. A device with `NET_ADMIN` (the `extended` profile) may flush the table or delete the rule: the node-agent checks every 10 seconds and puts them back; a deleted `accessport` is reported once as a `Warning` event `AccessPortLost` on the pod (reset the device). The NetworkPolicy of a published device admits only the proxy on the published port and no egress.

The `network.cybericebox.com/networks` pod annotation that carries them to the node-agent is a JSON array
(`[{"iface":"eth1","mac":"02:..."}]`; lab VPN and gateway ports add `"name":"<ovs port>"`), so no character of a name can start another
entry.

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
  device asks for another one (`InvalidArgument`, naming the device). The spec field is still `securityPreset`; an empty value is `standard`; the only other value is `extended`. Never in any profile: SYS_ADMIN, SYS_MODULE, SYS_RAWIO,
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
- **User namespaces** (`devices.security.userNamespaces`, a hidden setting, default `true`): device pods run with `hostUsers: false`, so root in a device is
  not root on the node. Needs Kubernetes 1.33+, containerd 2 and kernel 6.3+. Check on the cluster that device networking (the veth is moved into the
  pod namespace by the node-agent), state snapshots, `sudo` and setuid binaries, and images with UIDs above 65535 still work.
- **Ephemeral storage** (`devices.security.ephemeralStorage`, 2Gi): the writable layer, logs and emptyDirs of one device. Without a limit a device can fill
  the node's disk and DiskPressure evicts other pods. The PID limit is a node setting: set the kubelet's `podPidsLimit` (`--pod-max-pids`) in the
  cluster configuration (not a pod field).
- Not done here: a sandbox RuntimeClass (gVisor, Kata) for hostile images, and Pod Security Admission on the group namespaces (the lab pods need NET_ADMIN,
  which `baseline` allows, but the `extended` profile adds more; the admission policy below is the guard that matters for the operator).

### Namespaces of the lab groups

The namespace of a **new** LabGroup is `lg-<name, at most 40 characters>-<8 hex of the SHA-256 of the whole name>` (at most 52 characters), never the bare group
name. The group name is chosen by a tenant, so without the prefix it could equal an existing namespace; with it, it never can (the CRD also refuses the
names of the system namespaces outright). On top of that the operator **never adopts or deletes a namespace without the group's label**
`laboratory.cybericebox.com/group=<group>`: a namespace that exists under the name a group would get, without that label, is refused (the group stays
unprovisioned) and, when the group is deleted, left exactly as it is. The operator puts the label on every namespace it creates.

The namespace is recorded in `LabGroup.status.namespace`, and everything (the operator, the demux, the L7 proxy, the agent) uses that. Scripts that derive the
namespace from the group name must read `status.namespace` instead.

### The L7 proxy under hostile clients

- **HTTP server limits** (`proxy.l7.readHeaderTimeout` 10s, `readTimeout` 5m, `idleTimeout` 2m, `maxHeaderBytes` 65536) apply before any routing or
  authentication: a client that dribbles its headers or body, or opens connections and says nothing, is cut instead of holding a connection forever (an
  unauthenticated slowloris could otherwise exhaust the proxy). There is no write timeout, because responses stream; an upgraded (WebSocket) connection has
  its deadlines cleared and lives at most `LIVE_MAX_LIFETIME` (12h), with the access policy re-checked every 10 s.
- **The handoff link** is valid for 60 s at most (`proxy.l7.accessTokenMaxTTL`; the backend's token lifetime must not be longer). It is a bearer link that the
  proxy cannot make single-use, because its replicas share no memory; the short life is the control.
- **The session cookie belongs to the proxy.** Any `Set-Cookie` of the session cookie's name in a device's response is removed, so a device cannot set,
  replace or clear it; the device's own cookies pass.

### The lab that is starting

A request to a lab whose service cannot be reached yet (the connection is refused because the pod is not Ready, there is no route, the
connect times out) is not answered with an error. A browser gets a small page «Лабораторія запускається…» (in the browser language,
Ukrainian by default) with the platform crest pulsing as the loader, `503` and `Retry-After: 4`; the page asks again by itself every
4 seconds (`<meta http-equiv="refresh">`, no script) and the lab opens as soon as it answers. Only a `GET` or `HEAD` is repeated; any
other client gets `503 lab is starting` as plain text. A lab that does not exist, was turned off or is refused to the client keeps the
«not available» page (`404`), and a lab that answers wrongly (it hangs up, the response is broken) keeps the `502` card.

### The access log of the L7 proxy

The proxy writes one structured line (`request`, logger `proxy-l7.access`) for every request it forwards to a lab: `lab` (the lab id),
`method`, `path` (without the query, cut at 200 characters), `status`, `upstreamMs` (until the lab answered with its headers, or failed),
`bytes` (the body sent back to the client), `clientCancel` (the client left before the end) and `slow`. A request is slow when
`upstreamMs` is above `SLOW_REQUEST` (default `2s`, a Go duration). No address of a client is logged. A websocket upgrade is one line with
status `101` and no bytes; its traffic is metered, not logged. Requests the proxy refuses before the lab (no session, unknown lab, rate
limit) are not in this log.

### Sizing the proxy

The sizes in `proxy.l7` and `proxy.wg` come from measurements, repeated by two tests (`go test ./internal/proxy/...`, not in `-short`): `TestL7MemoryPerConnection` starts the
proxy as a separate process, holds 300, 1000 and 2000 requests in flight (each one a TLS connection, the HTTP server, the reverse proxy and an upstream connection to a
backend that never finishes the response) and reads the process's heap and stacks after a collection; `TestConntrackMemoryAtTheCap` fills the demux's conntrack table to its cap.

| | measured | chart |
|---|---|---|
| L7 proxy, idle | about 4 MiB | |
| L7 proxy, per connection with a request in flight | about 105 KiB | `maxConnections: 2500` is about 260 MiB |
| L7 proxy, informer caches (Services, LabGroups, clients, policies of all groups) | tens of MiB at several hundred groups | headroom of about 60 MiB |
| L7 proxy memory | | `proxy.l7.resources` 512Mi (the Go runtime stays under 80% of it, 410 MiB) |
| wg-demux, per conntrack entry | about 190 bytes | `maxEntries: 100000` is about 18 MiB, about twice that while the map grows |
| wg-demux memory | | `proxy.wg.resources` 128Mi |

`maxConnections` and the memory limit go together: past `maxConnections` a client waits in the kernel's backlog (the header timeout frees the places of silent clients), which
is a slow answer; a pod killed for memory cuts every request in flight on it. If you raise `maxConnections`, raise the memory by about 130 KiB per connection (the measured
cost with 25% headroom) and keep `liveTotal` at or below it. A request in flight is also a connection for the proxy, so `liveTotal` above `maxConnections` has no effect.

**To serve more clients, add proxy pods, not limits.** The L7 proxy is stateless (its session cookie is signed with the shared secret): `proxy.replicas`
(each replica takes its share of the connections the Gateway sends it; never two on one node), or `proxy.mode: daemonset` for one proxy on every node (larger clusters). The demux's table is per pod
and the demux is a DaemonSet, so its capacity is the number of entry nodes (`proxy.wg.nodeSelector`) times the numbers above; a WireGuard client whose packets land on another demux
re-handshakes (about 25-35 s, see the proxy rollout below), so change the entry nodes outside an event.

### Probes and rollouts

Every long-running component says when it really serves, and a rollout waits for that.

| Component | Ready (readiness) means | Liveness | Rollout |
|---|---|---|---|
| operator | the informer caches have synced (`/readyz` on :8081) | `/healthz` | one pod, leader election |
| agent | the mTLS port is bound: it is opened only after the CRD check and the setup (TCP probe; a startup probe waits up to 3 minutes for the CRDs) | the port answers | `maxSurge 1`, `maxUnavailable 0`, `agent.minReadySeconds` (10) |
| L7 proxy (`l7` container) | the HTTPS listener is bound and the caches (groups, clients, policies, access keys) have synced (`/readyz` on :8081) | `/healthz` | see below |
| wg-demux (`wg-demux` container) | the UDP socket is bound, the caches have synced and every LabGroup has been put in the demux table; in `tc` and `xdp` also: the programs are attached in front on every device and the entry addresses are known (`/readyz` on :8082) | `/healthz` | see below |
| node-agent | Open vSwitch answers over both channels (database and OpenFlow) and the caches have synced (`/readyz` on `127.0.0.1:9440`; loopback only, it is on the host network) | `/healthz`: Open vSwitch answers (a node-agent whose channels hang is restarted; the watchdog still exits when they are lost) | one node at a time (`maxUnavailable 1`), `nodeAgent.minReadySeconds` (20) |

- **Proxy rollout.** A new proxy pod must stay Ready for `proxy.minReadySeconds` (20) before the next one is replaced, so the replicas never go together: the demux keeps one replica's table while the other
  fills its own. The Deployment's pod anti-affinity is required (one replica per node), so a surge pod needs a node without a replica: when a live `helm upgrade` can count the nodes and there are more than replicas, the
  rollout starts the new pod first (`maxSurge 1`, `maxUnavailable 0`); otherwise it replaces one pod at a time (`maxSurge 0`, `maxUnavailable 1`), held back by readiness. `proxy.surge: true|false` forces either.
  WireGuard clients whose packets reach a new demux pod re-handshake (they keep their sessions on the other replica as long as it is up), so expect the clients of a replaced pod to reconnect within about 25-35 s
  (their keepalive and rekey timers), and do not upgrade the proxy during an event if that matters.
- **Shutdown.** After SIGTERM the proxy's HTTP server stops taking connections and lets requests in flight finish, up to `proxy.terminationGracePeriodSeconds` (45), after a preStop sleep of
  `proxy.preStopSleepSeconds` (5) that lets the Gateway and the Service stop sending connections to the pod. The agent does the same with its calls: it stops taking new ones and finishes the running ones for up to
  `agent.shutdownGrace` (30s), so a `CreateLabGroupClients` that has made a participant's WireGuard key (returned only in the answer) is not cut; what is left after the grace (long streams such as Monitoring) is
  closed, and its client resumes. `agent.terminationGracePeriodSeconds` (45) is above the preStop sleep plus the grace.
- A pod that is not Ready is taken out of its Service, so a node-agent or proxy that cannot serve gets no traffic; a pod that fails liveness is restarted by the kubelet.
- If a probe fails on a cluster whose CNI blocks the node's own traffic to pods, allow the kubelet (host entity) to reach the probe ports; Cilium allows it by default.

### When a device is Ready

A device used to be Ready as soon as its container ran, even while the application inside was still starting (a participant then met the proxy's "unavailable" page). Now a container device has a **readiness probe** in two cases:

- **A published web port** (`exposure.web`): the device is Ready once the application answers on that port. `http` (the default) and `https` ports get an HTTP `GET /` (any answer below 500 counts, so a page behind a login or a 404 on `/` is fine; a 5xx or a refused connection is not; the certificate is not verified); any other protocol gets a TCP connect. Nothing to configure.
- **`waitReadyFile: true`** on a device template (exercise device editor: "Wait for the /run/ready file"): the device is Ready once the file `/run/ready` exists inside the container. With both, both must hold.

A device with neither is Ready when it runs, as before. A lab is Ready only when all its devices are (this was already so; the probe makes "Ready" mean the application is up), so the L7 proxy's starting page shows until then.

How it works:

- The probe is an **exec** probe, not an `httpGet` from the node: it runs `/.cib/probe/probe ready ...` inside the container, so it needs neither a route from the node to the pod (the access port is wired by the node-agent; the network policy of a published device admits only the proxy) nor a shell in the image, and it decides what "answers" means (an `httpGet` counts only 200-399, so an application answering 401 or 404 on `/` would never be Ready). It tries the loopback and every other address of the container.
- The `probe` program is a static binary of the node image (`/probe`). An init container (`cib-probe`, the node image, no capabilities, read-only root) copies it into an `emptyDir` that the device container mounts **read-only** at `/.cib/probe`, so an image without a shell works and the participant (root in the device) cannot replace it. It is the same for Deployment devices and for state Pods; the rescue pod of a device with state persistence has no probe (a shell is not the application).
- Readiness only, nothing that kills: no liveness or startup probe, so a container is never restarted by it. Timing: no initial delay, every 2 s, up to 30 s of failures before a Ready device is marked NotReady again.
- Only **new** devices get the probe: the Deployment of a running device is never rewritten. Without the node image (`NETCONFIG_IMAGE`) there is no probe.
- The scheduler's start timeout (`scheduler.startupTimeout`, 5m) counts to Ready: an application that needs longer than that to open its port, or never creates `/run/ready`, is declared failed after it, and shown Started if it becomes Ready later.
- `/run` is not part of the state snapshots (`excludePaths`), so a device restored from a snapshot has no `/run/ready` until the application creates it again.

### The WireGuard demux under hostile senders

The demux (`wg-demux`) reads one public UDP port for every team. It holds no keys (the VPN pods do; WireGuard's own handshake is the security boundary), so
everything a stranger can make it hold or spend is capped (`proxy.wg.limits`):

- **No per-packet logs.** Nothing is logged for an incoming datagram (a flood could otherwise fill the log pipeline).
- **No limit per source address.** One NAT (a venue, a campus) hides a whole event behind one address, and behind a load balancer in SNAT mode the clients of a node
  share its address: any per-address cap would cut off a whole event. What is capped is shared by everyone:
- **Table cap:** `maxEntries` conntrack entries in all (two per session). A handshake that would pass it is dropped and the client retries.
- **Handshakes in progress are short-lived:** the entry of an init is made before anything is authenticated (the mac1 key derives from the group's public key, which
  every participant has), so it lives only `partialTTL` (15 s) unless the VPN pod answers; a completed session keeps its entry for 3 minutes after its last packet.
  Spoofed inits therefore cannot hold the table for minutes. They can still use up the global handshake budget (below): that needs a rate limit by source,
  which cannot be told apart from a venue, so watch the proxy's logs and the Cilium flow logs for such a flood instead.
- **Global rates:** `globalHandshakeRate`/`globalHandshakeBurst` for handshake initiations of all sources together (each costs a scan of the groups' mac1 keys), and
  `missRate`/`missBurst` for packets, all sources together, that match no session or come from an address the session does not expect, so guessing the 32-bit
  session index is rate-limited. Per session: `sessionRate`/`sessionBurst` packets per second, so one participant cannot use the demux's CPU up.
- **Roaming works as before**, but one session changes address at most once per `roamInterval`; a packet from a third address inside the interval is dropped and
  moves nothing. An index a live session owns is never taken by another handshake (the handshake is dropped). In `userspace` the Service's policy is `Cluster`, so
  the new source port of a roamed client is a new flow that Cilium may hand to the demux of another node (chance (N-1)/N with N demux pods), which knows no session:
  the client then re-handshakes, about 15 s (as with the replicas before the demux modes). Fewer demux pods (`proxy.wg.nodeSelector` on the edge nodes) make it rarer.
- **Type 2** (the server's answer) is accepted only from the VPN pod the init was forwarded to; **type 3** (cookie reply, the peer's own load protection) is
  forwarded, only from the exact address the session expects, and never moves the session.

One overloaded VPN pod only hurts its own team: the demux caps are shared, the pods are not.

### WireGuard demux modes

`proxy.wg.mode` decides how the demux moves the WireGuard transport packets. The handshakes (and everything the fast path does not know) always go through the demux
process, which is also what keeps the session table and the caps of the section above.

| Mode | What moves the transport packets | Pod | Restart of the demux pod |
|---|---|---|---|
| `userspace` | the demux process, packet by packet | pod network, non-root (65532), no capabilities, namespace `laboratory-wg` at the restricted level | the clients re-handshake: about 15 s lost (measured on the stand: 74 of 149 pings) |
| `tc` | eBPF programs on the node's external devices (tcx), in front of Cilium's | host network, root, capabilities `BPF`, `NET_ADMIN`, `PERFMON` only (not privileged), the host's `/sys/fs/bpf`, namespace at the privileged level | nothing is lost (0 of 148 pings): the maps and links stay pinned and the kernel keeps forwarding |
| `xdp` | the same, plus an XDP program that judges and rewrites the client's packets at the device | as `tc` | as `tc` |

**`userspace` is the default, and production runs it for now.** `tc` and `xdp` are opt-in, switched on only after the load test and the XDP test, done together with the owner. There is no silent fallback: a `tc` or `xdp` pod that cannot start its
fast path (no `BPF` capability, a foreign XDP program on the device, ...) exits and crash loops, and the operator takes the mode from the same value
(`WG_DEMUX_MODE`; a value other than the three fails the render and the operator start). To go back to `userspace`, see below.

**What the fast path does.** A transport packet of a session the demux has completed is rewritten and forwarded in the kernel: client to server on the node's
network device, server to client on `cilium_host` (servers on other nodes) or on the host-side veth of the server's pod (a server on this node).
Everything else is passed on to the demux process unchanged, and `wg_demux_fastpath_packets_total` counts why: no session (`passed_miss`), a packet from another
address than the session's (`passed_source`, a roam: the process moves the session), an idle session (`passed_stale`), no route or no metadata
(`passed_route`, `passed_meta`). The per-session rate (`sessionRate`/`sessionBurst`) is enforced in the kernel with one bucket per session for both directions
(`dropped_rate`). Over-rate packets are dropped there. IPv6 stays on the slow path.

**Entry discovery.** The kernel judges only packets sent to the demux's entry addresses: each node address with the Service's UDP nodePort, and the Service's
LoadBalancer and external IPs with the UDP port (IPv4). Each demux pod watches its own Service and Node and keeps the set current; the entry set is what `/readyz` waits for.
A change of `proxy.wg.nodePort` or of the Service's address needs no restart.

**Cilium.** The programs sit in front of Cilium's on the device (tcx), and a demux resync (`DEMUX_BPF_RESYNC`, 10 s) puts them back in front when another
program takes the head: on the stand, a foreign program inserted at the head was ahead for 1-6 s and then ours were first again, with no pings lost. Cilium
holds no NAT or conntrack state for the tunnel clients (the stand checks it is empty), so the Cilium agent can restart without touching the tunnels (0 pings lost). Two consequences:

- **The server sees the node, not the client.** In `tc` and `xdp` the VPN pod sees the packets from the node's `cilium_host` address (in `userspace`, from the demux pod's address). Roaming
  still works, the demux moves the session.
- **LB-IPAM and `externalTrafficPolicy`.** In `tc` and `xdp` the policy defaults to `Local` (a packet must reach a node whose kernel holds its session), and then the demux
  Service gets an address of its own: LB-IPAM shares an address only between Services with the same policy, and the gateway's Service has no pod selector to match. With `Cluster`
  (the `userspace` default, or set by `proxy.wg.externalTrafficPolicy`) the two share one address, but only once the second Service is created again after the change (an address
  already handed out is not moved). A stand or an on-prem edge that runs `tc` or `xdp` and wants UDP 51820 and TCP 443 on one address therefore sets
  `proxy.wg.externalTrafficPolicy: Cluster` (clients' source addresses are then the nodes' ones) or gives the demux Service its own address. The fast path picks the LoadBalancer address up as soon as the Service has it
  and drops it when the Service loses it (checked on the stand).

**Limits of `xdp`.**

- It attaches in native (driver) mode and falls back to generic mode when the driver refuses: on the stand's veth devices it attaches generic (`bpftool net show` says `generic`).
- It refuses to start when the device already has an XDP program of someone else: Cilium's XDP acceleration (`loadBalancer.acceleration`) must be off on the entry devices. Use `tc` there.
- A driver without XDP metadata room costs nothing: the frame passes untouched and the `tc` program handles it as in `tc` mode (`passed_meta`).
- On AWS, the ENA driver takes native XDP only with an MTU up to 3498. This is a driver limit, not a check of ours, and it is not tested here: the load test must confirm it.

**Metrics.** `:8083` of the demux (bound to the node's address in `tc`/`xdp`; in `userspace` the network policy admits the host entity only): `wg_demux_mode{mode}`, `wg_demux_sessions`,
`wg_demux_sink_errors_total` (sessions that could not be written into the fast path; the slow path keeps forwarding them), `wg_demux_packets_total{type}` (what the demux socket took, by message type)
and, in `tc`/`xdp`, `wg_demux_fastpath_packets_total{result}`. In `tc` and `xdp` the transport packets the process still sees stay near zero.

**Pins.** The maps and links of `tc`/`xdp` are pinned under `/sys/fs/bpf/cybericebox/wg-demux` on the node (`DEMUX_BPF_PIN_PATH`). A new demux pod takes the pinned state over and restores the
sessions of the old one as soon as it knows every group. Stopping the pod leaves the pins: the kernel keeps forwarding until the next pod takes over.

**Switching back to `userspace`.**

1. Set `proxy.wg.mode: userspace` and upgrade. The rollout replaces the pods one node at a time.
2. A `userspace` pod does not remove the pins, so the old programs keep forwarding. On every node that ran `tc` or `xdp`, run `/proxy proxy-wg detach` of the same image as a one-off
   pod: `nodeName` set, `hostNetwork: true`, root, `seccompProfile: RuntimeDefault`, capabilities `BPF`, `NET_ADMIN` and `PERFMON` only, the host's `/sys/fs/bpf` mounted, in a namespace that admits that
   (`laboratory-wg` is restricted in `userspace`; use another). The manifest is in `hack/kind-demux/e2e.sh`, the `detach` step.
3. It removes every link and pin of the fast path (`bpftool net show` has no `wg_` program, and `xdp` leaves the device). The clients come back on the slow path: on the stand both tunnels were back within 60 s.

**What the load test must measure** before `tc` or `xdp` goes to production:

- The CPU of the demux and of the node at the event's number of peers and at the peak rate, in each mode, against the `userspace` numbers above (`userspace` costs about 110m at ten active peers).
- The throughput and latency of a tunnel, and `dropped_rate`, `passed_*` and `wg_demux_sink_errors_total` under load: the share of packets that stay on the slow path.
- A demux pod restart and a Cilium agent restart with the clients busy: the loss, and the tcx order afterwards.
- `xdp` on the real NIC: native or generic mode, the metadata, and the ENA MTU limit above.
- The LB-IPAM setup of the real edge (the address of the demux Service, the nodePort path behind the load balancer).

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
  server (full API, requests up to 4 MiB), and one that did not to the **Enroll server**, which knows only the Enroll call, reads messages of at most
  `agent.server.enroll.maxMessageBytes` (64 KiB), and is rate limited (`rate` 5 per second, `burst` 10, all callers together). Nothing a caller without a
  certificate sends reaches the main server or is buffered beyond 64 KiB. A caller that already has a certificate enrolls through a connection without one (the
  client library's connection without a keypair).
- **Connections**: the TLS handshake has a timeout and at most `maxHandshakes` run at once; at most `maxAnonymousConns` connections without a certificate are open
  in total. There is no per-address limit and no per-address connection rate: behind a TLS passthrough route the address seen is the gateway's, so such a limit
  would be a global one that a single anonymous host could use up to lock out every tenant. Raise `maxAnonymousConns` and `maxHandshakes` if enrollments queue up.
- **Streams and keepalive**: `maxConcurrentStreams` (64) per connection, a connection ends after `maxConnectionAge` with `maxConnectionAgeGrace`, and a client
  that pings more often than `keepaliveMinTime` (10 s) is disconnected.
- **The token is found by a label.** The operator labels a Tenant with the start of its unused token's hash (`laboratory.cybericebox.com/enrollment-token`), so an
  Enroll costs one narrow, cached list instead of a read of every Tenant. A token issued before the label existed gets it when the operator next reconciles the Tenant
  (at its start): until then that token is not found, and the admin can ask for a new one.
- **Two replicas** (`agent.replicas`, default 2) spread over nodes with a PodDisruptionBudget (`minAvailable: 1`): an agent that is restarted does not take every
  tenant down. The Monitoring journal and the prewarm progress are per replica.
- **mTLS off is development only**: the agent always serves TLS and asks for client certificates; the one switch `agent.allowInsecure=true`
  (`AGENT_ALLOW_INSECURE=true`) turns the client-certificate check off and makes every caller the default tenant. (`agent.mtls.enabled`, `AGENT_TLS_ENABLED` and
  `AGENT_MTLS_ENABLED` are gone.)

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

### zot's data directory

zot runs as the unprivileged user 65532. `fsGroup` makes a volume writable for it only when the storage provisioner honours it; a hostPath or
local-path volume, keeps its owner and zot fails with `open /var/lib/registry/cache.db: permission denied`.
The registry pod therefore has one init container, `own-data`, that runs `chown -R 65532:65532 /var/lib/registry` as root with only the CHOWN, DAC_OVERRIDE
and FOWNER capabilities and a read-only root file system, and does nothing else. It runs on every start (a no-op once the volume is owned), so no manual step is needed; on a large volume the first start takes as long as the chown.

### Registry volume: keep the data when the claim goes

The registry volume (`laboratory-registry`, `registry.size`) holds every device snapshot. `helm.sh/resource-policy: keep` protects the claim from
`helm uninstall` only. When the claim or the namespace is deleted, the fate of the data is decided by the **reclaim policy of the PersistentVolume**, which
it takes from its StorageClass: `Delete` (the default of local-path and of the AWS EBS class) deletes the volume and every snapshot with it. Use a class
with `reclaimPolicy: Retain`. The volume is ReadWriteOnce and, bound with `WaitForFirstConsumer`, stays on the node it was made on:
moving the registry to other nodes (a lab label or taint on `labWorkloads`, a new lab node pool) leaves its pod Pending until the
snapshots are moved to a new volume there, or `registry.nodeSelector` pins it to the node that holds the volume. The chart can create it:

```yaml
registry:
  retainClass:
    create: true
    provisioner: rancher.io/local-path   # k0s stand (local-path-provisioner 0.0.24 or newer, the stand has 0.0.31)
    # AWS:  provisioner: ebs.csi.aws.com, parameters: {type: gp3, encrypted: "true"}, allowVolumeExpansion: true
```

The class is named `registry.retainClass.name` (default `laboratory-registry`), kept on uninstall, and used for the claim instead of `registry.storageClass`.
The local stand (`infrastructure/local/cluster/laboratory/values.yaml.tmpl`) and the AWS cluster role already set it. Without it the chart works as before and
the volume is only as durable as the class you name in `registry.storageClass`.

- The reclaim policy is copied into a PersistentVolume when it is made: a volume that already exists keeps the policy it has. To protect one, run
  `kubectl patch pv <name> -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'` (the PV is named in `kubectl -n laboratory-system get pvc laboratory-registry`).
- Getting a retained volume back: the PV stays `Released` with a `claimRef` to the old claim. Remove it (`kubectl patch pv <name> --type json -p '[{"op":"remove","path":"/spec/claimRef"}]'`),
  and create the claim `laboratory-registry` with `volumeName: <name>` (or install the chart again, then bind by `volumeName`). zot picks the data up as it was.
- On local-path the PV is a directory on one node; node loss still loses it. Retain protects against a deleted claim, not against a lost disk. EBS volumes
  survive the node but not the zone. The snapshots are the participants' work: back the volume up (EBS snapshots, or a copy of the directory).
- Growing `registry.size` needs a class with `allowVolumeExpansion: true` (set `registry.retainClass.allowVolumeExpansion`); without it `helm upgrade` is
  refused by the API server.

### Who may read the registry

zot is shared by every team and the image cache, so what it serves is split by repository (no catch-all pattern: a repository that matches none is
denied to everyone but the writer):

- **The public image cache** (`docker.io/**`, `ghcr.io/**`, `quay.io/**`, `registry.k8s.io/**`, and the extra registries you list): anonymous read.
- **The snapshots of the labs (`lab/**`) and the shared `base` repository: not anonymous.** The `reader` account may read them, the `writer` account
  everything. Both live in the Secret `laboratory-registry` (generated once and kept across upgrades). The node-agent forwarder (`127.0.0.1:5035`) adds the reader to the node runtime's **GET and HEAD** requests of those repositories
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
| L7 proxy | non-root (65532) | read-only | all dropped, no privilege escalation | RuntimeDefault |
| wg-demux, `userspace` | non-root (65532) | read-only | all dropped, no privilege escalation | RuntimeDefault |
| wg-demux, `tc` / `xdp` (host network) | root | read-only | all dropped, then `BPF`, `NET_ADMIN`, `PERFMON`; not privileged | RuntimeDefault |
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

**Admission policy** (`operator.admissionPolicy.enabled`, default on; the chart needs Kubernetes 1.33+ anyway). Three `ValidatingAdmissionPolicy` objects, each with a
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
- TCP port 5035 free on `127.0.0.1` of every node (a constant of the images: the relay port of the registry);
- the node-agent DaemonSet mounts `containerdRoot` read-only and the host cgroup tree, and gets the
  `DAC_READ_SEARCH` capability. Set `containerdRoot` to the containerd root of your distribution (k0s:
  `/var/lib/k0s/containerd`; stock containerd: `/var/lib/containerd`);
- the node-agent container runs with `appArmorProfile: Unconfined`. On nodes with AppArmor (Ubuntu, Debian) containerd
  otherwise puts every non-privileged container under its default profile `cri-containerd.apparmor.d`, which has
  `deny mount`; the snapshot filter mounts read-only overlay views of a device's layers, so every snapshot would fail with
  `failed to mount /tmp/containerd-mount...: permission denied`. The container already has `SYS_ADMIN`, so this adds no
  capability. Kubernetes 1.30 or newer is needed for the field. On nodes without AppArmor (kind, SELinux distributions) it
  changes nothing, which is also why kind never shows the failure.

A node-agent that still runs under an enforced `cri-containerd` profile (a chart older than this fix, or a policy that
overrides the profile) checks it at startup: it logs one error `state persistence is on but snapshots cannot be taken on
this node` (it reaches the error journal), puts `AppArmor blocks snapshots: ...` on every device it follows, and takes
no snapshots there (it never freezes the containers for a snapshot that cannot work). The devices keep running; their
state is not saved until the node-agent restarts under the right profile.

### How it works

1. **Registry access without host setup.** The node-agent (host network) relays `127.0.0.1:5035` to the registry
   Service. Snapshot images are referenced as `localhost:5035/lab/<namespace>/<lab>/<device>@sha256:...`;
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
   **Owner ids.** A device pod runs in a user namespace (`hostUsers: false`), and the diff of its writable layer holds the host ids of the
   files. The node-agent translates every owner through the container's uid and gid maps (from its OCI spec), so a snapshot holds the ids
   inside the container and restores under any new id range; an id outside the map is written as 0 and logged. Without a user namespace
   nothing changes.
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

### Node loss

A snapshot-backed device is a bare Pod, so a dead node does not recreate it by itself. When the node goes NotReady, the control plane's taint
eviction sets a deletion time on the pod, but only the node's kubelet can confirm the deletion, so the pod stays `Terminating` and the device stays
down. The operator closes this gap: a device pod that has been `Terminating` for `devices.statePersistence.nodeLossForceDeleteAfter` (default `5m`,
`"0"` = never; env `STATE_NODE_LOSS_FORCE_DELETE_AFTER`) on a node that is NotReady (Ready `False` or `Unknown`) or no longer in the cluster is force-deleted
(grace period 0), and the device is recreated on another node from its last snapshot (the same path as after a crash). The timer starts at the pod's deletion time,
which is the eviction time plus the pod's grace period, so the device is down for about the taint toleration, plus the grace period, plus this timeout.
A pod on a node that is Ready is never force-deleted, however long it takes to stop. The work since the last snapshot is lost
(see "What is and is not kept"). Deployment-backed devices (no persistence) are replaced by their ReplicaSet and need none of this.

Runbook for a lost node:

1. `kubectl get nodes` shows it `NotReady`; `kubectl get pods -A -o wide --field-selector spec.nodeName=<node>` lists what was on it.
2. Wait for the timeout above. The operator log has `force-deleted a device pod stuck terminating on a lost node`; the device's `status.state` moves to a new incarnation.
3. To go faster, or for a pod that is not a device (VPN, gateway), delete it yourself: `kubectl -n <ns> delete pod <pod> --force --grace-period=0`.
   Only do this for a node that is really down: the container may still run on a node that has only lost its network.
4. If the node is dead for good: `kubectl delete node <node>`. The pods on it go at once and the devices are recreated.
5. When the node returns, its kubelet removes the leftover containers.

### Retention of deleted labs

The snapshots of a lab are not deleted with the lab. The operator runs a sweep every 10 minutes: for each snapshot
repository whose Lab no longer exists it records the time it first noticed this in the ConfigMap
`laboratory-snapshot-retention` (namespace `laboratory-system`), and once `retention` (default 168h) has passed it deletes
the lab's repositories. zot's garbage collection (`registry.gc.interval` and `registry.gc.delay`, both 1h by default)
then attempts to free unreferenced blobs. The sweep confirms manifest deletion only; physical reclaimed bytes and timing remain unavailable until separately verified GC evidence. A lab recreated under the same
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

If a device's warning says `AppArmor blocks snapshots` (or `snapshot failed: filter layer: failed to mount ...:
permission denied`), the node-agent runs under the runtime's default AppArmor profile: see "Enable" above. Check it with

```bash
kubectl -n laboratory-system exec <node-agent-pod> -c node-agent -- cat /proc/1/attr/current   # must say: unconfined
```

### Testing it on a real node

kind does not apply AppArmor, so the end-to-end suite cannot catch a profile that blocks the snapshot mounts. Before a
release that touches the snapshot path, and on every new node image (distribution, kernel, containerd), run this on a
real lab node of a test cluster with AppArmor on (for example Ubuntu with k0s):

1. Deploy the chart with `devices.statePersistence.enabled: true`. `cat /proc/1/attr/current` in the node-agent
   container says `unconfined`, and its log has no `snapshots cannot be taken` error.
2. Create a lab with a persistent device. Inside it: write a file, change a file's mode and owner, delete a file that
   comes from the image, make a symlink and a sparse file.
3. Within the debounce the node-agent logs `container frozen for the snapshot` with no `snapshot` error after it; the
   Device's `status.state` has a snapshot (image, size, layers) and no warning; the image is in the registry.
4. Delete the device's Pod. The operator recreates it with the next incarnation from the snapshot, and every change of
   step 2 is there, with the same owners and modes.
5. Stop and start the lab: the exit snapshot is taken (`mark exit`, no `exit snapshot` error) and the state comes back.
6. Limits: write past `writeQuota` (a warning, the last snapshot stays), past `maxEntries` (the same), and run a
   required capture (it succeeds and records its image).
7. With more than one lab node: drain the node, the device starts on another node and restores from the registry.
8. Afterwards: the kernel log has no overlayfs or AppArmor errors from the node-agent, and `mount | grep
   containerd-mount` in the node-agent container is empty (no mount is left behind).

---

### CPU throttling of a device

A device whose container keeps hitting its CPU limit is slow because of the limit, not because of its work. Every 30 seconds the
node-agent reads the CFS counters (`nr_periods`, `nr_throttled` of `cpu.stat`, cgroup v2) of the device containers of its node and
keeps the last five minutes of them. The share of throttled periods, rounded to steps of 10, goes to `Device.status.cpuThrottle`
(`percent`, `windowSeconds`, `observedAt`) when it changes; the lab controller copies it to `Lab.status.devices[].cpuThrottle`, and the
agent sends it on in the device status of the lab (`LabDeviceStatus.cpu_throttled_percent` and `cpu_throttle_window_seconds`, in
`ListLabs` and in the monitoring stream). The field is absent (0 on the wire) while the container is not throttled, is idle (the
counters only run while it is busy), has no CPU limit, or ran for less than about 30 CFS periods. The platform treats a device above
**50** as starved and may alert staff; the laboratory itself only exposes the data. Nothing is read for a container the node-agent
cannot find in the cgroup tree (`CGROUP_ROOT`, default `/host/sys/fs/cgroup`). Existing clusters need the new CRDs (`kubectl apply`
of the chart's `crds/`, `helm upgrade` does not apply them) and the new agent proto.

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

The registry is always installed; neither `devices.statePersistence.enabled` nor `registry.cache.enabled` decides it, so switching one off never removes zot or the snapshots in it. `statePersistence.enabled` only turns the device snapshots on, `cache.enabled` only the image cache (the full zot image, about 70 MB
compressed on amd64 and arm64; the cache needs its sync extension), both can be on. Needs `nodeAgent.enabled`.

- **How the nodes reach it.** As for snapshots: the node-agent relays `127.0.0.1:5035` on every node
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

The poller reads **informer caches** (LabGroups, Labs, LabGroupClients, access policies, traffic reports, Devices, Reservations, and the pods of devices),
not the API server: a poll costs no call to it, however many groups there are, and the pod metrics are read in one call for the whole cluster at
most every 15 s. The informers run only while somebody is subscribed (they start with the first stream and stop with the last), and a list or watch error of the API
never turns into a deletion: the caches keep the last objects they saw, a poll that cannot observe is skipped (and logged), and a failing metrics API keeps
the last usage numbers. The price is memory: the caches hold the objects of all tenants (tens of MiB at thousands of labs), so raise `agent.resources` with
the number of labs. An object written a few milliseconds ago may appear in the next poll, not in the current one. For the caches the agent's cluster role (`laboratory-agent-cluster`) has **read-only** list and watch on labs, devices, clients, access policies and traffic reports in all namespaces (and watch on pods); its writes stay inside the group namespaces, through the role the operator binds there.

### Request

| Field | Meaning |
|---|---|
| `selector` | Kubernetes label selector (`instance=a`, `tier in (gold,silver),!legacy`). Empty = everything. Invalid: `InvalidArgument`. |
| `min_interval_ms` | Least time between two messages of this stream; changes in between are merged into one message. Bounded below by 10 ms; the agent observes the platform every `agent.monitoring.pollInterval` (1s), so that is the real resolution. Default 5 s when unset. |
| `resume_after_sequence`, `agent_epoch` | Where to resume (below). 0 / empty = no resume. |

### What is sent

- The **first message** is either a full **snapshot** (`snapshot = true`: replace everything you hold) or, on a successful
  resume, the merged updates you missed (`snapshot = false`). Then deltas follow. A delta holds the changed or new
  records (`groups`, `labs`, `clients`, `policies`, `traffic`, `reservations`), `deleted_keys`, and `capacity` when it changed.
- Every message carries `agent_epoch` and `sequence`, the position of the stream up to which everything has been sent
  (or filtered out as not matching). A quiet stream sends an empty heartbeat every 30 s with the position, so that the
  stored position stays inside the journal window.
- **Selector.** It is matched against the Kubernetes labels of the object itself: LabGroup, Lab, LabGroupClient, the
  access policy and the Reservation. A deletion is delivered iff the deleted object matched (the journal keeps the labels of deleted objects).
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

## Priority and sizing of the pods

### Priority classes

Every pod the laboratory runs carries a PriorityClass (created by the chart, values `priorityClasses`), so the scheduler never evicts the
system for a lab:

| Pods | Class | Value |
|---|---|---|
| node-agent | `system-node-critical` (built in) | |
| operator, agent, L7 proxy, wg-demux, registry | `laboratory-platform` | 1000000 |
| VPN and gateway of a group | `laboratory-group` | 10000 |
| device | `laboratory-device`, `preemptionPolicy: Never` | 100 |

A VPN or a gateway may evict a device, never the other way round: a device never evicts anything. The operator sets the group and device
classes on the pods it creates (env `PRIORITY_CLASS_GROUP`, `PRIORITY_CLASS_DEVICE`, from the same values), and its admission policy lets it
create a pod only with one of these two. The chart refuses values that do not satisfy device < group < platform. `priorityClasses.create=false`
leaves the creation to you (the same names must exist). Group pods that were made before the classes existed keep running without one until
they are recreated: the operator does not roll them for this.

### Sizing of a group's own pods

The VPN and the gateway pod of a group are sized by the platform, not by the chart alone:

- **The agent reports how** (`GetFeatures`, `group_pods`): for the VPN `base + per_user * users`, at most `max_users` users; for the gateway
  `base + per_unit * labs` that use the internet, at most `max_labs` labs; CPU and memory both; the maximum size is the base plus the per-unit
  value times the maximum. Values: `vpn.sizing` (`baseCpu`, `baseMemory`, `perUserCpu`, `perUserMemory`, `maxUsers`) and `inetGateway.sizing`
  (`baseCpu`, `baseMemory`, `perLabCpu`, `perLabMemory`, `maxLabs`). Measured: a VPN about 16Mi idle and about 195Mi and 55m with 10 active peers,
  a gateway about 8Mi idle. `group_pods` also carries the default size (`vpn.resources`, `inetGateway.resources`), which must not exceed the maximum
  (the agent refuses to start otherwise).
- **The backend computes the size** for the group it plans (the VPN for the event's largest team, the gateway for the group's internet labs) and
  passes it in `CreateLabGroups` (`vpn_size`, `gateway_size`: `cpu_millicores`, `memory_bytes`). The agent refuses a size that is not positive in both
  values or is over the maximum (`INVALID_ARGUMENT`); the same request again is `EXISTS`, another size is a different spec. Without a size the pod gets
  the default.
- **The operator creates the pod Guaranteed at exactly that size** (`LabGroup.spec.vpn.size`, `spec.gateway.size`, fixed once set) and never resizes it.
  A group's reservation in the scheduler is what its pods request. An upgrade converges only the image and the pull policy of existing pods.

`GetFeatures` also reports the constants of the device model (`constants`: 16 interfaces per container device, 48 ports per switch or hub, a hard
ceiling of 64 devices per lab; the chart's `limits.lab.maxDevices`, 32 by default, is `limits.lab.max_devices`) and the quota the cluster gives the tenant
(`tenant_quota`, the quota `GetCapacity` reports against; no `has_*` flag = no limit).

## Network Layout

| Network             | Default       | Usage                                                              |
|---------------------|---------------|--------------------------------------------------------------------|
| `vpnBaseNetwork`    | `10.128.0.0/10` | Full VPN supernet; advertised as AllowedIPs to WireGuard clients   |
| Client subnet       | `10.128.0.0/24` | First /24 of vpnBaseNetwork; VPN gateway at .1, clients at .2–.254 |
| Per-lab VPN subnet  | `10.128.N.0/24` | N>=1 carved from vpnBaseNetwork                                |
| `inetBaseNetwork`   | `10.192.0.0/10` | Internet-gateway subnets                                           |
| Per-lab inet subnet | `10.192.N.0/24` | Mirrors VPN lab index                                              |

### The return route to the participants

The replies of a device reach a participant only through the VPN pod (host .1 of the lab's VPN network), so a device interface
connected to the VPN network (directly or through switches and hubs) needs a route to the client subnet via that gateway. The
operator adds it, the author writes nothing:

- **static addresses**: the operator adds `<client subnet> via <lab VPN .1>` to the interface routes it renders (the Lab template is
  not changed). The client subnet is the first /24 of `vpnBaseNetwork` (`10.128.0.0/24`), where the WireGuard clients live. Nothing
  is added when the interface already has a route that covers the client subnet or already uses the VPN gateway as its default
  gateway. An existing lab gets the route (and its device pods restart) at the first reconcile after the operator upgrade.
- **`dhcp-preset`**: the lab VPN DHCP server sends the route as a classless static route (option 121), together with the default
  route via .1 (RFC 3442: a client that understands option 121 ignores the router option). The netconfig init-container applies it.
- **`dhcp` (the image's own client)**: works only when the image's client applies option 121 (dhclient and `udhcpc` with the
  routes option do, many minimal clients do not). Otherwise the image has to add the route itself, or use a static address.

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

## Upgrade order and hardening rules

The rules below come from the second security audit (docs/security/2026-10-02-laboratory-reaudit.md). Where an existing object could
slip under a stricter check, the component that runs it clamps or ignores the bad value instead of failing.

**Upgrade order.**

1. Apply the CRDs first: `kubectl apply --server-side -f charts/laboratory/crds/`. `helm upgrade` does not update the `crds/`
   directory, so without this step the new validation patterns below are missing on an upgraded cluster (the code does not rely on
   them, they are the second line), with one exception: the capacity budget relies on the scheduling fields of the gang commit,
   so with an older CRD the operator runs without the budget (see "Maximum capacity and node autoscaling"). Apply the CRDs before
   enabling `capacity.budget`. A missing Reservation CRD does not stop the operator either, but reservations do not work
   until it is applied and the operator restarted (see "Reservations").
   A rollback to an operator without reservations leaves the `laboratory.cybericebox.com/evict-at` annotations and the
   `CapacityShortfall` condition of the Tenants behind; the old operator ignores them. To tidy up:
   `kubectl annotate labs,labgroups -A --all laboratory.cybericebox.com/evict-at-` and remove the condition from
   `Tenant.status.conditions` by a status patch.
2. Once, when upgrading from a version where the VPN Service lived in `laboratory-proxy` (before the WireGuard demux modes): delete it right before
   `helm upgrade`, outside an event: `kubectl -n laboratory-proxy delete svc laboratory-proxy-wg --ignore-not-found`. The Service moves to
   `laboratory-wg`, and Helm creates the new one before it deletes the old one, so with a fixed `proxy.wg.nodePort` the create fails ("provided port
   is already allocated") and the upgrade stops halfway. The VPN is down from this step until the new demux pods are Ready, and every client
   re-handshakes. Once migrated the step does nothing (the kind harness checks the upgrade from `develop`: `hack/kind-demux/e2e.sh upgrade`).
   The old Service is gone before a non-atomic `helm upgrade`: if that upgrade fails for another reason, the VPN has no Service until it is run again or rolled back.
3. `helm upgrade` (the operator, agent, node-agent and proxy roll out together; the operator and the agent can run in either order).
4. The notes of each item below say what, if anything, must happen in a different order. In short, before `helm upgrade`:
   - the cluster is Kubernetes 1.33 or newer (the chart refuses an older one), or `operator.admissionPolicy.enabled=false` is set on purpose (the chart refuses to skip the policies silently, and the operator exits at start when they are not enforced);
   - the tenant `default` exists (the chart creates it): with mTLS on, every certificate, the default tenant's included, needs its Tenant object;
   - apart from step 2, nothing else has to be done by hand: the operator rolls the VPN and gateway pods when their template changes, the agent runs two replicas and the node-agents roll one at a time.

### Device resources (R-2)

- A device resource value must be a positive quantity (no exponent, at most 24 characters) and at most 1024 cores or 1 TiB. The agent
  refuses `"0"`, negative, overflowing and out-of-bound values on CreateLabs with an error that names the device; the CRD has the same
  pattern. The sums use saturating arithmetic.
- The operator ignores such a value on an object that got past the check (the next candidate or the default is used) and clamps
  anything above the chart maximum to it (`limits.device.maxCpu` / `maxMemory`, passed to the operator as `DEVICE_MAX_CPU` /
  `DEVICE_MAX_MEMORY`), so a pod never runs without limits.

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
- `images.tenantDeny` still defaults to empty: the chart cannot know which repositories of an installation are private. Set it to the
  platform's private organizations.

### Names and system pods (R-19)

- **System pods are selected by `laboratory.cybericebox.com/component`** (`vpn`, `gateway`), a label under the platform prefix that no
  caller can set, never by `app`: device pods carry `app=<device name>`, so a device named `vpn` can never join the VPN Service or match
  the network policies written for the VPN. The VPN Service, the `vpn-egress` and `gateway-egress` Cilium policies, the label sync,
  the scheduler and the node-agent all use it. The web Service and web NetworkPolicy of a device select by the lab and device labels.
- **Reserved device names**: `vpn`, `gateway`, `internet` are refused on CreateLabs and by the Lab CRD (a CEL rule on the device name,
  ratcheting: a Lab that already has such a device can still be updated).
- **Device object names** are `<lab>-<device>-<hash>` (10 hex over the pair with a separator no name contains), so two pairs never
  share a name; the env Secret is `<device object name>-env`. A bare `<lab>-<device>` would be ambiguous (lab `a-b` with device `c`, lab
  `a` with device `b-c`).
- **Gateway**: each lab interface of the gateway pod may send only from its own subnet (`LABSRC`), the forward rule accepts only
  `lab+` interfaces towards the outside, and the filter is built in the order that is never open (policy DROP, then the egress and
  source filters, then the accepting rules; the egress chain is rebuilt beside the old one and the jump switched, so a restart has no
  gap). A pod without ip6tables logs that it forwards no IPv6.
- **CRDs.** Apply the CRDs first (see the upgrade order above) so the reserved-name rule and the device ceiling (`maxItems: 64`) are present.

### The admission policies are required, and checked (R-8)

- **The chart no longer skips the policies silently.** `operator.admissionPolicy.enabled` (default true) needs a ValidatingAdmissionPolicy cluster (the chart as a whole needs Kubernetes 1.33 or newer, and refuses an older one). The opt-out is explicit: `operator.admissionPolicy.enabled=false` accepts, on purpose, that the operator's ServiceAccount
  can bind its role in any namespace.
- **The operator checks at start** (`OPERATOR_REQUIRE_ADMISSION_POLICY`, set by the chart from the value above) that the three policies and their bindings exist
  and deny, and that they are enforced: it asks the API server, in dry run (nothing is written), for a RoleBinding in the namespace `default` and for a namespace
  that is no group's, and both must be refused by the policies themselves. It retries for 90 s (`OPERATOR_ADMISSION_POLICY_TIMEOUT`: the policies are applied in the
  same release and take a moment to be compiled), then exits with the reason, so a cluster that does not enforce them shows as a crash looping operator, never as a
  quiet one. It needs `get` on the policies and their bindings (in its ClusterRole). The variable is on by default; run from a developer's machine, or deployed without the chart, it is set to `false`.
- **The pod rules.** The pods the operator creates (and the templates of the Deployments it makes) must drop ALL capabilities and add only what the device profiles, the
  VPN and the gateway use (`laboratory.operatorCapabilities` in the chart helpers; a Go test keeps the list equal to what the code can add, and the "never" list out of
  it), use the RuntimeDefault seccomp profile, not unmask `/proc`, publish no host port, name no node, run only as the `default`, `vpn` or `gateway` service account,
  and use only `emptyDir`, `projected`, `downwardAPI`, `secret` and `configMap` volumes, and no host network, PID or IPC, no host path and no
  privileged container. The rules about what a pod is made of apply when it is created and to the template of a Deployment or DaemonSet; changing the labels of a
  running pod is judged only by the scope rules.
- Not done: narrowing the operator's Secret permissions in the release namespace to named Secrets (the owner's decision lists the policy check and the pod rules only).

### Lab ceilings (R-7)

- **Devices of a lab, every type counted** (container, switch, hub): at most `limits.lab.maxDevices` (default 32), which can only lower the ceiling of 64 (`names.MaxLabDevices`, also the
  Lab CRD's `maxItems`); the chart refuses a larger value, and the agent clamps one that gets through, so the agent's clean error binds before the CRD's.
- **Interfaces of a device**: 16 on a container device, 48 on a switch or hub, in the agent and as a CEL rule and `maxItems` in the CRD. There are no other caps (connections, VNIs, DHCP ranges).
- `CreateLabs` refuses a spec over them with the numbers. GetFeatures reports the effective device limit.

### Labs on the platform's domain (R-18, option B) and the CRD check

- **The L7 proxy filters what a device's response does to the site.** A cookie that is host-only, or whose `Domain` is exactly the device's own host, passes unchanged; a cookie whose `Domain` is the labs
  base domain or any parent of it is removed; a cookie named like the proxy's session cookie is removed whatever its domain (also in 1xx responses); `Clear-Site-Data`, `Strict-Transport-Security` and
  `Content-Security-Policy` are removed. Requests to a device never carry the session cookie. Not done: binding the `/_auth` handoff to the browser (a login-CSRF defence) needs a nonce the backend sets
  before it redirects, so it is a backend change, not a proxy one.
- **The agent and the operator refuse to start on CRDs older than the release** (they read the published OpenAPI schema and look for `Tenant.status.certificateEpoch`), retrying for a minute, then exit
  with `kubectl apply --server-side -f charts/laboratory/crds/` in the message. So Enroll can never burn a token against an old CRD. No extra permission is needed (discovery).

### Lifecycle native gates and admission recovery

Full group lifecycle completion requires direct UID/revision reads, durable
pre-scale service inventory and node-owned first-success cleanup acknowledgements.
Inventory binds Group UID, operation/revision, Deployment UID, Pod UID, node boot,
container IDs, cgroup paths and owned port keys. Empty API inventory, node loss,
foreign ownership or missing acknowledgements remain `Unknown` and hold capacity.
Configure the native observer only after the final source-image fixture proves
these boundaries. Lifecycle capability flags default false until their independent
native gates pass. No production sizing preset or chart default is reduced here.

Child create/start admission uses one durable pending token on the Group, updated
with the same resourceVersion CAS as group stop. Busy batch items return `FAILED`
with `retryable=true`; independent batch items still return their own result.
Replay the identical admitted operation after an agent crash. If its exact child
write exists, group stop can resolve that token; a never-written operation stays
pending. No TTL silently cancels it, and no moderator recovery endpoint is added.
Administrative recovery of an abandoned never-written operation is outside the
supported producer path.

The local five-peer source `/lab` point used 80Mi/50m for VPN and 32Mi/25m for the
gateway with 13 VPN networks, 65 applied relations and 10 gateway/NAT networks.
The repeat returned 8,200/8,200 during its ramp and 22,489/22,489 during a separate
90s hold (about 249.85pps aggregate). Gateway returned 180,000/180,000 over 180s
at 1000pps. The initial refresh burst lost 6,061 replies out of 295,200 and CPU
throttled. VPN lifetime peak 52.301Mi includes both attempts; gateway lifetime
peak 14.066Mi, with no OOM/max events. Retained flows overlapped up to 16,374.
This is conditional `TESTED_POINT` evidence, not a complete supported traffic
envelope, operator/CNI/OVS/snapshot restore proof or general capacity guarantee.
Raw evidence remains under `local-proof/` in the owner's 2026-10-08 lifecycle
outputs; retain both the successful repeat and failed burst.
