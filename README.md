# Laboratory

Laboratory runs isolated training networks on a Kubernetes cluster. It builds the machines and networks of a task,
keeps every team apart from every other team, and gives people controlled ways in: a WireGuard VPN, a browser, and a
filtered path to the internet.

It is the lab layer of the Cyber&nbsp;ICE&nbsp;Box platform, and it also works on its own. Laboratory is a
**constructor**: it offers generic parts (devices, switches, gateways, cables, access rules, checks). It knows nothing
about events, courses, rankings or scores. A platform backend drives it through one gRPC API and keeps all of that
for itself.

## Contents

- [What it does](#what-it-does)
- [How it is built](#how-it-is-built)
- [Objects](#objects)
- [Building a lab](#building-a-lab)
- [Ways in](#ways-in)
- [Isolation](#isolation)
- [Checks and readiness](#checks-and-readiness)
- [Starting labs: scheduler, capacity, reservations](#starting-labs-scheduler-capacity-reservations)
- [Stopping, starting and retiring](#stopping-starting-and-retiring)
- [Device state and images](#device-state-and-images)
- [Bandwidth protection](#bandwidth-protection)
- [Tenants and enrollment](#tenants-and-enrollment)
- [Management agent API](#management-agent-api)
- [Activity reports](#activity-reports)
- [Feature switches](#feature-switches)
- [Requirements](#requirements)
- [Install](#install)
- [Images](#images)
- [Development](#development)
- [What it does not do](#what-it-does-not-do)
- [License](#license)

## What it does

| Area | What you get |
|---|---|
| Topology | Labs made of containers or micro-VMs, switches, hubs and cables, with static or DHCP addressing |
| Isolation | One namespace, one address space and one set of keys per team; layer-2 segments that reach nobody by default |
| VPN access | A WireGuard server per team behind one shared UDP port; access rules per client and per network segment |
| Browser access | A device's web page or its console in a browser, through an HTTPS proxy, without entering the lab network |
| Internet | Outbound web access through a filtering gateway; nothing inbound, nothing but web |
| Checks | Automatic checks of what is inside a device: files, processes, ports, commands |
| Scheduling | Labs start through a window, in a declared order, only where there is room |
| Capacity | Current and maximum capacity, per-tenant quotas, reservations ahead of time, maintenance windows |
| State | Optional snapshots of a device's writable layer that survive a crash or the loss of a node |
| Images | Optional pull-through image cache with pinned digests and prewarming |
| Multi-tenancy | Several backends on one cluster; none can see another's objects |
| Monitoring | One resumable stream that carries every state change |

## How it is built

```
                       platform backend
                             | gRPC + mutual TLS
                             v
                      +-------------+  objects  +-----------------+
                      |    agent    |---------->|    operator     |
                      +-------------+           |  (controllers)  |
                                                +--------+--------+
                                                         | creates and reconciles
        participants                                     v
   +----------------------+        +----------------------------------------+
   | WireGuard client     |  UDP   |  namespace of one team (lab group)     |
   |                      |------->|  +-------------+   +-----------------+ |
   +----------------------+   |    |  | VPN server  |   | internet gateway| |
   | browser              |   |    |  +------+------+   +--------+--------+ |
   +-----------+----------+   |    |         |   lab networks    |          |
               | TCP 443      |    |     +---+-------------------+---+      |
               v              |    |     |  device pods (attacker,   |      |
   +-----------------------+  |    |     |  targets, switches ...)   |      |
   | proxy                 |  |    |     +---------------------------+      |
   |  - HTTPS front        |--+--->+----------------------------------------+
   |  - WireGuard demux    |            ^
   +-----------------------+            | Open vSwitch segments, Geneve between nodes
                                +-------+--------+
                                |   node-agent   |  one per node
                                +----------------+
```

| Component | Runs as | Role |
|---|---|---|
| Operator | one Deployment | Turns lab objects into namespaces, pods, services and network policies; schedules the start of pods |
| Management agent | one Deployment | The gRPC API for platform backends; each caller sees only its own objects |
| Node-agent | DaemonSet on lab nodes | Programs Open vSwitch on its node and plugs every device into its segments, across nodes when needed |
| VPN server | one pod per team | WireGuard server of the team: peers, access rules, traffic accounting |
| Internet gateway | one pod per team | Outbound web access for the team's labs, DHCP for lab segments |
| Proxy | one Deployment | The HTTPS front for device web pages and consoles, and the WireGuard demultiplexer that shares one UDP port between all teams |
| Checker | DaemonSet, optional | Runs the checks of the labs of its node and writes the results into the lab status |
| Device access service | DaemonSet, optional | Serves the browser console of devices |
| Node-state | DaemonSet on lab nodes | Takes device snapshots and watches CPU throttling |
| Registry | one pod, optional | Stores device snapshots and works as a pull-through image cache |

The VPN pod and the gateway pod of a team exist only while the team needs them: each has a switch on the lab group.

## Objects

Everything is a Kubernetes custom resource in the API group `laboratory.cybericebox.com`.

| Object | Meaning |
|---|---|
| `Tenant` | One platform backend. Carries its quota and its device-state policy |
| `LabGroup` | One unit of isolation, usually a team or a single user. Gets its own namespace, VPN server, internet gateway and address space |
| `Lab` | One task instance: its devices, connections, gateways and checks |
| `Device` | A machine, a switch or a hub of a lab |
| `Connection` | A cable between two ports |
| `LabGroupClient` | One WireGuard peer of a person in a group. The operator generates its keys and a ready client config |
| `LabGroupAccessPolicy` | Who in the group may reach what |
| `Reservation` | Room held ahead of time for labs that will start later |
| `MaintenanceWindow` | A period the cluster administrator announces for maintenance |
| `Pool` | Allocates network identifiers, addresses and lab subnets |

A few more kinds are internal: the operator creates and removes them itself.

## Building a lab

| Part | What it is |
|---|---|
| Device | `container` (the default; runs as a container or as a micro-VM), `unmanaged-switch` or `hub` |
| Gateway | `vpn` or `internet`, up to 10 per lab. Each is attached to a network by an ordinary connection and gets its own /24 |
| Connection | A cable between two ports. Switches and hubs join ports into one broadcast domain |
| Addressing | An interface is `static` or `dhcp`. A static address may be a reference such as "host 10 of the network of gateway `attack`", so the author never needs to know the subnet |
| DHCP | A server per gateway, with ranges inside its /24. An internet gateway also advertises DNS |
| Routes | A device may route by gateway references as well |

Rules the operator enforces:

- At most one gateway per broadcast domain; a gateway has one port.
- A lab with no gateway is valid.
- Names are DNS labels. `vpn`, `gateway` and `internet` are reserved.

**Devices.** A device runs a container image. It has a security preset: `standard` (no extra capabilities) or
`extended` (adds `NET_ADMIN` for networking tools). A device may carry a bootstrap, which is a set of files and a
script that runs once, and may ask for state persistence.

**Internet per device.** A device marked `internet: deny` cannot share a broadcast domain with an internet gateway;
a lab that wires it there is refused. The guarantee covers the broadcast domain only: another device that forwards
with NAT can still route out.

## Ways in

There are three. Everything else is denied.

| Way | Real-world picture | Reaches | Direction |
|---|---|---|---|
| VPN | A cable into one network segment | The addresses of that segment, any port | Participant to lab; the lab only answers |
| Browser | You stand at the device: its screen and keyboard | One device, without entering its network | Browser to device, through the proxy |
| Internet gateway | An uplink to the public internet | The open web, through a filtering proxy | Lab to outside only |

### VPN

- A VPN gateway of a lab gives access to **its own /24 and nothing behind it**. A switch does not extend it and a
  router inside the lab is not a route for the client. Deeper networks are reached by pivoting from a device on the
  segment, as in a real engagement.
- A client may hold any number of segments at once. The subnets are unique inside a group, so they never overlap.
- **Access is per client and per gateway**, never per whole lab. A rule may narrow access to part of the segment,
  down to one address. Several rules add up. Deny wins.
- The client config lists exactly what the rules allow, and the VPN server enforces the same rules whatever the
  client routes.
- By default the lab can only answer: it cannot open a new connection to a participant's machine. A gateway may
  allow it (`reverseConnections: true`) for tasks that need a callback.
- All teams share one public UDP port. A demultiplexer routes each packet to the right team by its public key and
  never decrypts VPN traffic.

### Browser

A device may publish a web port, a console, or both. Each published thing has its own host name,
`<device>-<code><kind>.<base domain>`, and nothing about the device appears in a path or a query.

| Kind | Status | Notes |
|---|---|---|
| Web | Works | The device's web port behind the proxy |
| Console | Works, optional (`terminal.enabled`) | A shell in the browser. Text only, no file transfer. Limits per user, lab and node; idle and total time limits |
| Remote desktop (VNC, RDP) | Not served yet | The lab description accepts it; nothing serves it |

How a session is opened:

- The backend signs a short **access link** with its own key. The link is valid for 60 seconds and is bound to one
  host name. The proxy checks the signature, the group and the access policy.
- A web session is a cookie for that one host only. It opens that device and no other, and the device cannot set or
  clear it.
- A console session uses no cookie, so the console can be shown in a frame of another site. Which sites may frame it
  is a setting (`terminal.embedding.allow`); the default is none.
- A live session is cut within seconds when the policy denies the client or the lab stops.

### Internet

A lab with an internet gateway goes out through the gateway pod of its team:

1. The forward policy is drop. A lab can send out nothing that is not listed here.
2. **TCP 80 and 443** go to a transparent web proxy. It reads the host name (the `Host` header or the TLS server
   name, without decrypting TLS), resolves that name itself and opens a new connection to the address it found. The
   address the lab chose is never used, so a hosts file, the lab's own DNS server or a bare address change nothing.
3. It refuses a request with no name, a bare address, Encrypted ClientHello, a name with no public address, and a
   short list of tunnel services. A connection lives at most 10 minutes.
4. **DNS** on any port 53 ends in the gateway's own resolver: public IPv4 records only.
5. Ping passes at a low rate. Every other protocol is dropped.
6. Nothing comes in from the outside. Private, link-local, cloud metadata and cluster address ranges are never
   reachable. This is a constant of the code with no exception.
7. Each lab has download and upload caps, and there is a cluster-wide upload total.

## Isolation

| Between | How |
|---|---|
| Teams | A namespace per team, default-deny network policies, their own VPN and gateway pods, separate WireGuard keys |
| Labs | Each lab has its own network identifier on the Open vSwitch bridge. The bridge runs in secure mode: a port with no flow reaches nobody |
| Labs and the cluster | A gateway reaches the public internet only. Node, pod, service and metadata ranges are denied in the firewall, in the proxy and in the cluster network policy |
| Tenants | One identity per tenant. Another tenant's object looks like "not found" |
| Devices and participants | A published device accepts only the proxy on its published port. A participant may be root inside a device and still cannot reach the pods of Laboratory |
| Device and node | Device pods run in a user namespace; a micro-VM profile adds a hardware boundary |

A lab pod is placed only on a node whose node-agent is ready, so a device can never start with plain cluster
networking and no layer-2 wiring.

## Checks and readiness

**Readiness.** A device with a published web port is Ready when that port answers. An author may also ask for a
ready file inside the device. A lab is Ready when all its devices are; until then the proxy shows a "starting" page.
A device that does not become Ready within the start timeout (5 minutes by default) is declared failed, with a reason.

**Checks** (optional, `checks.enabled`). The checker watches the labs of its node and looks inside devices through a
small probe. The expected values stay in the checker; the probe only reports facts.

| Check | Looks at |
|---|---|
| `file` | A file's presence or content |
| `directory` | A directory |
| `process` | A running process |
| `port` | A listening port |
| `kernel_param` | A kernel parameter |
| `permission` | Ownership and mode |
| `request` | The answer to a request |
| `custom` | The result of a command |

| Mode | Meaning | Typical use |
|---|---|---|
| `once` | Passes once and stays passed | Something was achieved: a file was planted, a service was reached |
| `continuous` | The current state matters | A hardening task; a service that must keep running |

A result is `passed`, `not_passed`, `error` or `unknown`. `error` means the check could not be decided: the previous
state is kept and it never counts as passed. Results go to the lab status and to the monitoring stream. A backend
can ask for a run at once; such requests have a cooldown.

## Starting labs: scheduler, capacity, reservations

**Scheduler.** A burst of new labs does not start all at once. Pods start through a window (`scheduler.maxPods`, 20 by
default), one object after another.

- Objects of one deploy group start together, and a group may name other groups that must finish first.
- Before a group starts, its images are pulled onto the nodes.
- A pod waits while no node has room for it, and while its tenant is at its quota.
- A pod that is not Ready in time fails with a reason (`ImagePull`, `CrashLoop`, `Unschedulable`, `StartupTimeout`,
  `PodNotCreated`, `DoesNotFit`) and frees its slot.
- The place in the queue and the reason are visible in the object status and through the API.

**Capacity.** Laboratory reports two numbers for the lab nodes: what fits now, and the maximum the cluster may grow
to when it has autoscaled nodes. It knows nothing about the autoscaler itself; the maximum comes from a configured
budget. Each tenant may have a CPU and memory quota, absolute or as a share of the cluster.

**Reservations.** A backend can reserve room for a period ahead of time. Labs that belong to a reservation are
admitted as a group, so a team's labs either all get in or wait together.

**Maintenance windows.** The cluster administrator announces when maintenance may happen. Laboratory refuses and
delays nothing during a window; backends read the windows through the API and plan around them.

**Team overhead.** Every team with a VPN or a gateway costs a fixed amount of CPU and memory. The API reports that
amount so a backend can size a reservation.

## Stopping, starting and retiring

- **Stop and start a lab.** A stopped lab keeps its definition and frees its resources. With state persistence its
  devices come back as they were.
- **Stop and start a team.** Stopping a team requires all its labs to be stopped first; it then removes the team's
  VPN and gateway pods. Starting a team brings back those services only. Labs stay stopped until each is started.
- **Retire.** Retiring a stopped lab or team removes its definitions, secrets and snapshots for good and leaves a
  small record of what existed. A retired object cannot be started again.
- Every such request names the exact object and carries an operation id and a revision, so a repeated or late
  request cannot undo a newer one.
- A stop is reported as complete only after the node confirms that the processes, their resources and their network
  attachments are gone. The absence of a pod in the API is not taken as proof.

Which of these a cluster offers is advertised through the API and is off by default; an administrator turns each
capability on in the chart.

## Device state and images

**State persistence** (optional, `devices.statePersistence.enabled`). The writable layer of a device is saved as
snapshots, so the device survives a crash or the loss of its node. Each device opts in. The cluster and the tenant
bound it: a write quota, a maximum file size, excluded paths. Snapshots are stored in the built-in registry, and a
backend can download the latest state of a device as one archive.

**Reset and rescue.** For a device with saved state, a backend can discard the state and restart the device from its
image, or start it from its latest state with a shell in place of its normal start command, to repair a
configuration that makes the service crash.

**Image cache** (optional, `registry.cache.enabled`). The built-in registry works as a pull-through cache.

- The operator pins the digest of every image when a lab is created, so a moving tag cannot change an image in the
  middle of an event.
- A backend can prewarm the cache before an event, so a burst of labs starts without pulling.
- Every pull through the cache is checked with the credentials of that pull, so a tenant cannot start another
  tenant's private image that happens to be on the node already.

## Bandwidth protection

On by default (`bandwidth.enabled`). No participant, lab or team can saturate a node or the cluster network. The
usable bandwidth of a node is divided between three classes: the entry (VPN and browser traffic), the internet
gateways, and traffic between nodes. Inside a class everyone gets a fair share and never more than a cap.

## Tenants and enrollment

One cluster serves several backends that must not see each other.

- A tenant is declared in the chart's `tenants:` values. The tenant `default` always exists.
- **The identity of a tenant is the name in its client certificate.** The cluster never hands out a private key.
  The operator creates a one-time enrollment token per tenant; the cluster administrator passes it to the backend.
- The backend calls `Enroll` with the token, a certificate request and the public half of its own Ed25519 access
  key. The agent signs a client certificate and burns the token. From then on the backend renews its certificate
  and rotates its access key itself.
- **Access links** for the browser are signed by the tenant with that access key. The proxy holds the public keys
  of all tenants and also checks that the team belongs to the tenant that signed. No key is shared between tenants.
- Every object the agent creates is stamped with its tenant, so every call sees only the caller's objects.

## Management agent API

With `agent.enabled` the chart deploys a gRPC server with mutual TLS. The service is `LabManager`, defined in
[`pkg/agent/protobuf/agent.proto`](pkg/agent/protobuf/agent.proto). A Go client is in
[`pkg/agent/client`](pkg/agent/client).

Laboratory never calls a backend and sends nothing anywhere by itself. The API is the only link.

| Group | Calls |
|---|---|
| Lab groups | `CreateLabGroups`, `ListLabGroups`, `UpdateLabGroups`, `DeleteLabGroups` |
| VPN clients | `CreateLabGroupClients`, `ListLabGroupClients`, `UpdateLabGroupClients`, `DeleteLabGroupClients` |
| Access | `SetLabGroupAccess` |
| Labs | `CreateLabs`, `ListLabs`, `UpdateLabs`, `DeleteLabs` |
| Lifecycle | `StopLabs`, `StartLabs`, `StopLabGroups`, `StartLabGroups`, `RetireLabs`, `RetireLabGroups` |
| Reservations | `CreateReservations`, `ListReservations`, `UpdateReservations`, `DeleteReservations` |
| Devices | `ResetDevices`, `RescueDevices`, `RunChecks`, `ExportDeviceSnapshot` |
| Cluster | `Ping`, `GetFeatures`, `GetCapacity`, `ListMaintenanceWindows`, `PrewarmImages` |
| Events | `Monitoring` (a stream), `GetLabGroupActivity` |
| Enrollment | `Enroll`, `RenewCertificate`, `RotateAccessKey`, `RemoveAccessKey` |

Rules of the API:

- Calls are plural: a list goes in and a result per item comes out. One object is a list of one.
- Calls that select many objects are guarded by an expected count, so a wrong selector changes nothing.
- Objects carry labels, and lists and the stream can be filtered by them.
- The `Monitoring` stream carries every state change and can be resumed from where a client stopped.
- The contract has a version, `major.minor`. A caller built against `2.N` works with an agent of major 2 and a
  minor not lower than `N`.

## Activity reports

Off by default (`activityReports.enabled`). This is not telemetry: the data goes only to your own backend, and only
when it asks. Laboratory aggregates nothing and keeps a short ring only, at most 1000 events and one hour.

| Fact | What is kept |
|---|---|
| Internet use | Per lab and minute: source address, destination address and port, bytes, connections |
| Console sessions | When a client opened and closed the console of a device |
| Devices touched | Per client, lab address and minute: bytes both ways |

A source address names an address, not a device: an author may set any addresses inside a lab. Attribution is best
effort; only the fact of internet use per lab is reliable.

## Feature switches

What a cluster serves is set in the chart, once for the whole cluster. A lab that needs something that is off is
refused with a stable reason. The refusal comes from the Kubernetes API server itself, so it also holds for objects
created with `kubectl`.

| Chart value | Default | When off | Reason |
|---|---|---|---|
| `vpn.enabled` | on | No WireGuard at all; labs with a VPN gateway and VPN clients are refused | `VPN_UNAVAILABLE` |
| `inetGateway.enabled` | on | No internet gateways; labs with one are refused | `INTERNET_UNAVAILABLE` |
| `proxy.enabled`, `proxy.l7.enabled` | on | No browser access of any kind | `ACCESS_UNAVAILABLE` |
| `terminal.enabled` | off | No browser console | `ACCESS_UNAVAILABLE` |
| `checks.enabled` | off | No checks; labs that declare checks are refused | `CHECKS_UNAVAILABLE` |
| `scheduler.enabled` | on | Every pod starts as soon as it exists | - |

A backend reads the current state with `GetFeatures` before it offers a feature to its users.

## Requirements

- **Kubernetes 1.35 or newer**, **containerd 2.x**, and **Cilium** as the cluster network. Nodes are split into two pools by the label
  `node-restriction.kubernetes.io/cib-pool` (`lab` for the nodes that run labs, `lab-control` for the control of the laboratory), set before the install.
  The chart refuses an older cluster.
- **Linux nodes** with kernel 6.3 or newer, cgroup v2, and these modules available: `openvswitch`, `geneve`,
  `wireguard`, `br_netfilter`, `nf_conntrack`, `nf_conntrack_netlink`. Nothing else is installed on the host: Open
  vSwitch runs inside the node-agent, which also loads the modules and sets the kernel parameters it needs.
- **cert-manager**, for the wildcard certificate of the proxy and the server certificate of the agent. With a public
  certificate authority the wildcard certificate needs a DNS challenge; a self-signed issuer is enough for development.
- **DNS**: a wildcard record for the lab domain, a name for the VPN endpoint and a name for the agent.
- **Load balancers** made by the infrastructure: the proxy (TCP 443), the agent, the VPN (UDP 51820) and the registry gate (internal, for the lab nodes)
  target `NodePort` Services with `externalTrafficPolicy: Local`, so the client address reaches the pod.
- `kubectl` 1.35 or newer and `helm` 3.12 or newer.

Laboratory targets plain Linux machines and is developed against k0s. A `kind` cluster works for local development.

| Name | Example | Points to |
|---|---|---|
| `*.<operator.baseDomain>` | `*.labs.example.com` | The proxy |
| `operator.publicVPNEndpoint` | `vpn.example.com` | The WireGuard demultiplexer, UDP 51820 by default |
| `agent.domain` | `ctl.labs.example.com` (the default) | The management agent |

## Install

The chart is in [`charts/laboratory`](charts/laboratory). It must be installed into the namespace
`laboratory-system`; it fails early when a required value is missing.

1. Label the nodes into their pools (`kubectl label node <node> node-restriction.kubernetes.io/cib-pool=lab` or `=lab-control`). The proxy holds no
   secret of its own: a web session is a grant signed by the tenant, which the proxy verifies with the tenant's public key.

2. Write a values file. These are the only values an installation has to give:

   ```yaml
   # my-values.yaml
   operator:
     baseDomain: labs.example.com        # base domain of the lab hosts
     publicVPNEndpoint: vpn.example.com  # the address WireGuard clients connect to
     supportEmail: support@example.com
   registry:
     gate:
       address: registry.lab.internal:5443   # the internal load balancer the lab nodes dial to reach the registry gate
   proxy:
     l7:
       platformOrigins: [https://app.example.com]   # the origins of the platform pages that post a grant
   agent:
     enabled: true
   tenants:
     platform: { }                       # one entry per backend; the name is its identity
   ```

   Every other option has a default and is described in [`charts/laboratory/values.yaml`](charts/laboratory/values.yaml).

3. Install:

   ```bash
   helm install laboratory oci://registry-1.docker.io/cybericebox/laboratory --version X.Y.Z \
     --namespace laboratory-system --create-namespace -f my-values.yaml
   ```

4. Give the backend its enrollment token:

   ```bash
   kubectl -n laboratory-tenants get secret tenant-platform-enrollment -o jsonpath='{.data.token}' | base64 -d
   ```

Notes:

- Images in a private registry need pull secrets, listed under `imagePullSecrets`. The operator copies them into the namespace
  `laboratory-upstream`, where only the registry gate reads them; no pod of a lab and no lab node holds a registry credential.
- `helm upgrade` does not update custom resource definitions. Apply the definitions from `charts/laboratory/crds`
  with `kubectl` before an upgrade.
- `helm uninstall` does not remove the definitions or the labs. Delete the labs first, then the definitions.
- Laboratory can be used without a backend: create `LabGroup`, `LabGroupClient` and `Lab` objects with `kubectl`.

Every setting, upgrade rule and operating procedure is described in [DEPLOY.md](DEPLOY.md).

## Images

Images are published for `linux/amd64` and `linux/arm64`. One `Dockerfile` builds all of them.

| Image | Contents |
|---|---|
| `cybericebox/laboratory-controller` | The operator |
| `cybericebox/laboratory-agent` | The management agent |
| `cybericebox/laboratory-proxy` | The HTTPS front and the WireGuard demultiplexer |
| `cybericebox/laboratory-node` | The node-agent, the checker, the device access service, Open vSwitch and the network plugins |
| `cybericebox/laboratory-lab` | The VPN server and the internet gateway |

The chart uses the image tag of its own version. It refuses the tag `latest`. The release cycle is described in
[CONTRIBUTING.md](CONTRIBUTING.md).

## Development

To build from source you need [mise](https://mise.jdx.dev), Docker and `make`. `mise install` puts the pinned tools in place (Go, kind,
kubectl, helm, kustomize, controller-gen, golangci-lint, protoc); `mise tasks` lists the commands. The Makefile holds the targets; run
them through mise so that they see those tools.

```bash
mise run generate    # regenerate resource definitions, access rules, generated code and API clients
mise run proto       # regenerate the Go code of the gRPC contracts
mise run lint        # run the linter (for Linux, as CI does)
mise run build       # build the operator binary into ./bin/manager
mise run test        # unit tests and tests against a real API server
mise run cluster-up  # the kind cluster of the end-to-end tests
mise run e2e         # end-to-end tests on that cluster
mise run make help   # every other Makefile target: mise run make <target>
mise run make docker-build    # build all images
mise run make kind-deploy     # build, load into a kind cluster and install
```

| Path | Contents |
|---|---|
| `api/` | Types of the custom resources |
| `cmd/` | Entry points of the binaries |
| `internal/` | Controllers, the agent, the node-agent, the proxy, the VPN server, the gateway, the checker |
| `pkg/agent/` | The API definition and the Go client |
| `clientset/` | Generated typed client for the custom resources |
| `charts/laboratory/` | The Helm chart |
| `kernel/` | Kernel configuration of the micro-VM profile |
| `hack/`, `scripts/` | Development and check scripts |
| `test/` | Chart rendering tests and end-to-end tests |

`make test` downloads an API server and etcd by itself, so controller tests need no cluster.

## What it does not do

| Not done | What to do instead |
|---|---|
| Layer 2 from outside (ARP spoofing from your own machine) | The VPN is layer 3. Put a device on the segment and work from it |
| Routing over the VPN to networks behind a lab router | A VPN gateway is one flat /24. Reach deeper networks by pivoting |
| Remote desktop (VNC, RDP) | Not served yet |
| Names for lab hosts | There is no built-in DNS for labs. Addresses are fixed; run a DNS server inside the lab if you want names |
| Scores, statistics, history | Laboratory hands over raw facts when asked. The backend aggregates |
| Phoning home | No telemetry, no update check, no report. The only traffic out of the cluster is image pulls and what lab devices send through an internet gateway |
| Unrestricted internet for a lab | Only the web path described above. Nothing inbound, no other protocol |

## License

Apache License, Version 2.0. See [LICENSE](LICENSE).
