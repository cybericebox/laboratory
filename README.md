# Laboratory

Laboratory is the lab-infrastructure layer of the Cyber ICE Box platform. It runs isolated, per-team virtual networks
on a Kubernetes cluster and gives each team access to its own lab over WireGuard or through an authenticated HTTPS
front for web challenges.

It consists of:

- a **Kubernetes operator** that reconciles a small CRD graph (`Tenant`, `LabGroup`, `Lab`, `Device`, `Connection`,
  `LabGroupClient`, `Pool`, plus a few internal kinds) into namespaces, pods, services and network policies, and
  schedules the start of pods;
- a **management agent**, a gRPC API (mTLS) through which one or more platform backends (**tenants**) drive the
  operator, each seeing only its own objects;
- a **node-agent** DaemonSet that programs Open vSwitch (OVS) on every node and plugs lab devices into L2 segments;
- a **WireGuard VPN** server per lab group, reached through a shared **VPN demultiplexer**;
- a shared **L7 HTTPS proxy** that fronts web challenges;
- a per-group **internet gateway** (egress-only NAT, optional DHCP);
- an optional platform **registry** ([zot](https://zotregistry.dev)) that stores device state snapshots and works as a
  pull-through image cache;
- a **Helm chart** that installs all of the above.

## Architecture

```
                       platform backend
                             | gRPC + mTLS
                             v
                      +-------------+   CRDs    +-----------------+
                      |    agent    |---------->|    operator     |
                      +-------------+           | (controllers)   |
                                                +--------+--------+
                                                         | creates / reconciles
        participants                                     v
   +----------------------+        +----------------------------------------+
   | WireGuard client     |  UDP   |  namespace labgroup-<id> (one per team)|
   |                      |------->|  +-------------+   +-----------------+ |
   +----------------------+   |    |  | VPN server  |   | internet gateway| |
   | browser (web tasks)  |   |    |  +------+------+   +--------+--------+ |
   +-----------+----------+   |    |         |  lab networks   |           |
               | TCP 443      |    |     +---+-----------------+---+       |
               v              |    |     |  device pods (attacker, |       |
   +-----------------------+  |    |     |  targets, switches ...) |       |
   | laboratory-proxy      |  |    |     +-------------------------+       |
   |  - proxy-l7 (HTTPS)   |--+--->+----------------------------------------+
   |  - proxy-wg (demux)   |            ^
   +-----------------------+            | OVS L2 segments, Geneve between nodes
                                +-------+--------+
                                |   node-agent   |  (DaemonSet: OVS sidecar, CNI gate,
                                +----------------+   device port reconciler)
```

- A **Tenant** is a client of the management agent (a platform backend). Tenants are isolated from each other and have
  their own device-persistence policy and resource quota (see [Tenancy](#tenancy-and-enrollment)).
- A **LabGroup** is one team (or any unit of isolation) of a tenant. It gets its own namespace, WireGuard server, internet gateway and
  address space. Lab networks of different groups are isolated from each other.
- A **Lab** declares devices and connections between them. The operator materialises **Device** pods and **Connection**
  segments; the node-agent plugs each device into an OVS L2 segment, across nodes if needed.
- A **LabGroupClient** is one WireGuard peer. The operator generates its key pair and a ready-to-use client config.
- **Pool** objects allocate VNIs, IPs and lab subnets.
- The **proxy** runs two containers on one public IP. `proxy-wg` demultiplexes incoming WireGuard UDP to the right
  group's VPN server without terminating VPN traffic. `proxy-l7` terminates TLS for `*.<baseDomain>`, verifies the
  participant's session (seeded from a lab access link: a JWT signed with the Ed25519 access key of the tenant that owns
  the lab group) and routes the request to the challenge's web service.

## Components

| Component | Image | Role |
|---|---|---|
| `manager` (operator) | `laboratory-controller` | controller-runtime manager with all reconcilers |
| `agent` | `laboratory-agent` | gRPC management API (`LabManager`) in front of the CRDs |
| `node-agent` | `laboratory-node` | per-node OVS programming, device port reconciliation, gRPC socket for the CNI gate |
| `install-cni`, `cni-gate` | `laboratory-node` | installs and runs the meta-CNI that keeps the cluster CNI in charge of `eth0` |
| `netconfig` | `laboratory-node` | init container that configures lab interfaces inside a device pod |
| `vpn` | `laboratory-lab` | per-group WireGuard server, peer management, access policy, flow accounting |
| `gateway` | `laboratory-lab` | per-group internet egress NAT and optional DHCP for lab segments |
| `proxy-l7`, `proxy-wg` | `laboratory-proxy` | shared HTTPS front and WireGuard demultiplexer |
| `ovs-diag` | - | diagnostic tool for moving OVS ports into pod network namespaces |

The single `Dockerfile` in the repository root builds all images (`--target controller|agent|proxy|node|lab`). Component specifications (in Russian) are in
[`docs/specs/`](docs/specs/).

## Releases & images

Images are published to Docker Hub for `linux/amd64` and `linux/arm64`:

| Image | Contents | Base | Target of `Dockerfile` |
|---|---|---|---|
| `cybericebox/laboratory-controller` | operator (`/manager`) | distroless static | `controller` |
| `cybericebox/laboratory-agent` | gRPC management API (`/agent`) | distroless static | `agent` |
| `cybericebox/laboratory-proxy` | `/proxy` with `proxy-l7` and `proxy-wg` | distroless static | `proxy` |
| `cybericebox/laboratory-node` | `/node` (node-agent, install-cni, netconfig, cni-gate), Open vSwitch, CNI plugins | alpine 3.24 | `node` |
| `cybericebox/laboratory-lab` | `/lab` (vpn, gateway), iptables (nft), iproute2, wg | alpine 3.24 | `lab` |

Each image carries only the code of its own domain. The `proxy`, `node` and `lab` binaries start the component named by
their first argument (`/node node-agent`); the `cni-gate` copy the node-agent installs on the host is the `node` binary
started under that name. Open vSwitch is 3.7.1 from alpine 3.24; OVS 4.0.0 arrives with the next alpine stable release
(we do not use edge).

- The cycle (PR check, `develop`, pre-release, promote) is described in [CONTRIBUTING.md](CONTRIBUTING.md). A merge into `develop` publishes `sha-<7>` of every image
  (`develop.yml`); a merge into `main` publishes `vX.Y.Z-rc.N` (`prerelease.yml`).
- Promoting an rc (`promote.yml`) adds `vX.Y.Z` and `latest` to the same images without a rebuild, packages the Helm chart with `version` `X.Y.Z` and
  `appVersion` `vX.Y.Z`, pushes it to `oci://registry-1.docker.io/cybericebox/laboratory` and attaches the `.tgz` to the GitHub release.
- Chart image tags default to the chart `appVersion` (`vX.Y.Z`); set `*.image.tag` in values to pin another exact tag (for example `sha-<short commit>`). The chart refuses `latest`.

```bash
helm install laboratory oci://registry-1.docker.io/cybericebox/laboratory --version X.Y.Z \
  --namespace laboratory-system --create-namespace -f my-values.yaml
```

The workflows need two repository secrets (Settings, Secrets and variables, Actions): `DOCKERHUB_USERNAME` and
`DOCKERHUB_TOKEN` (a Docker Hub access token with write access to the `cybericebox` organisation).

## Requirements

Laboratory targets bare Linux VMs running Kubernetes (developed against [k0s](https://k0sproject.io)).

- **Kubernetes** with the Gateway API and **Cilium** as the cluster CNI (the chart creates a Cilium `Gateway`
  with TLS passthrough and shares one load-balancer IP between the Gateway and the WireGuard service through
  Cilium LB-IPAM). A `kind` cluster works for local development.
- **Node kernel** (cgroup v2) with these modules available: `openvswitch`, `geneve`, `wireguard`, `br_netfilter`,
  `nf_conntrack`, `nf_conntrack_netlink`. Nothing else has to be installed on the host: Open vSwitch runs inside the
  node-agent DaemonSet (an `ovs` sidecar), and a host-prep init container loads the modules and sets the required
  sysctls.
- **cert-manager** (chart dependency) for the proxy wildcard certificate, the agent's server certificate and the
  client CA of the agent. Tenant client certificates are not issued by cert-manager: the agent signs them when a tenant
  enrolls (see [Tenancy](#tenancy-and-enrollment)). With an ACME issuer the wildcard certificate needs a DNS-01 solver (Cloudflare is supported by the chart); a self-signed
  issuer is available for development.
- Kubernetes 1.33+ (the chart refuses an older cluster: user namespaces for device pods, `spec.hostUsers`), containerd 2.x and a node kernel 6.3+ for them; `kubectl` 1.33+, `helm` 3.12+.
- To build from source: the Go version in `go.mod`, Docker, and `make`.

## Quick start (Helm)

The chart is in [`charts/laboratory`](charts/laboratory). It must be installed into the `laboratory-system`
namespace; `templates/validate.yaml` rejects anything else and fails early when a required value is missing.

1. Create the namespace. Lab access links are signed by the tenant's own access key, registered when the tenant
   enrolls ([Enrollment & access keys](DEPLOY.md#enrollment--access-keys)); there is no shared key to create.

   ```bash
   kubectl create namespace laboratory-system
   ```

2. Create the proxy session secret (32+ bytes) in the proxy namespace:

   ```bash
   kubectl create namespace laboratory-proxy
   kubectl -n laboratory-proxy create secret generic proxy-session \
     --from-literal=sessionSecret="$(openssl rand -base64 48)"
   ```

3. If your images are in a private registry, create `kubernetes.io/dockerconfigjson` secrets in
   `laboratory-system` (and in `laboratory-proxy` / `laboratory-agent`, which run in their own namespaces) and list
   them under `imagePullSecrets`. The operator copies them into every lab group namespace.

4. Write a values file:

   ```yaml
   # my-values.yaml
   operator:
     baseDomain: lab.example.com                  # REQUIRED: base domain of lab web hosts
     publicVPNEndpoint: vpn.example.com            # REQUIRED: host (or host:port) handed to WireGuard clients; the port is proxy.wg.publicPort, 51820 by default
     supportEmail: support@example.com            # shown on the VPN probe page
   agent:
     enabled: true                                # its host is ctl.<baseDomain> unless agent.domain is set
   proxy:
     wg:
       nodePort: 31820                            # optional fixed NodePort of the WireGuard service
   tenants:
     platform: { }                                # one Tenant per platform backend; its name is the certificate CN
   imagePullSecrets:
     - name: registry-credentials
   ```

   These are the only values an installation has to give (and `certManager.email` with an ACME issuer). One release tag drives every image
   (`image.tag`, empty = the chart `appVersion`); everything else has a default that suits any installation and stays overridable. All options
   are documented in [`charts/laboratory/values.yaml`](charts/laboratory/values.yaml); only the ports inside the cluster network are constants
   of the images, they are listed in [`DEPLOY.md`](DEPLOY.md) ("The inputs"). The port clients connect to outside the cluster is
   `proxy.wg.publicPort` (51820 by default).

5. Install:

   ```bash
   helm dependency update charts/laboratory
   helm install laboratory charts/laboratory \
     --namespace laboratory-system --create-namespace \
     -f my-values.yaml
   ```

6. Hand the tenant its enrollment token and let its backend enroll (see
   [Tenancy and enrollment](#tenancy-and-enrollment)):

   ```bash
   kubectl -n laboratory-tenants get secret tenant-platform-enrollment -o jsonpath='{.data.token}' | base64 -d
   ```

7. For direct use without a backend, create a lab group and a lab. [`DEPLOY.md`](DEPLOY.md) walks through `LabGroup`, `LabGroupClient` and `Lab`
   examples, upgrades and uninstalling (CRDs are not removed by `helm uninstall`).

## Scheduler

A burst of new Labs and LabGroups does not start all at once. The scheduler starts their pods through a window
(`scheduler.maxPods`, 20 by default), one object after another (the pod-slot conveyor). Objects with the same deploy
group are started together, and a group can declare other groups that must complete first (`deploy-after`). A pod that
is not Ready within `scheduler.startupTimeout` (5 minutes) is declared failed with a warning and a reason
(`ImagePull`, `CrashLoop`, `Unschedulable`, `StartupTimeout`, `DoesNotFit`) and frees its slot. Before a group starts,
its images are pulled onto the nodes by the node-agents through the container runtime (an `ImagePull` request; no
tenant code runs); a prepull gates only its own group and gives up at once on an image that cannot be pulled. A pod is held back while no node has room for it, and while its tenant is at
its resource quota. Device pods are Guaranteed (requests equal limits) and each lab group has a PodDisruptionBudget.
The queue place and reason are in `status.scheduling` and in the agent's `LabStatus.queue`. See "Scheduler" in
[`DEPLOY.md`](DEPLOY.md).

## Tenancy and enrollment

One cluster serves several backends (tenants) that must not see each other.

- A tenant is a cluster-scoped `Tenant` resource, declared in the chart's `tenants:` values (the tenant `default` always
  exists). It carries the device-persistence policy (`persistence.allowed`, `writeQuota`, `maxFileSize`) and an optional
  CPU and memory `quota` (absolute or a percentage of what the lab nodes allocate). The agent stamps every object it
  creates with the tenant, so every RPC sees only the caller's objects; another tenant's object is `NOT_FOUND`.
  `GetCapacity` and the `Monitoring` stream report only the caller's quota, reserved and used resources.
- **The tenant identity is the CN of the client certificate.** The tenant never receives a private key from the
  cluster. The operator creates a one-time enrollment token per tenant (Secret `tenant-<name>-enrollment` in
  `laboratory-tenants`); the cluster administrator hands it over. The tenant calls `Enroll` (plain TLS, no client
  certificate yet) with the token, a certificate request and the public half of its own **Ed25519 access key**; the agent
  signs a client certificate (CN = the tenant name) and burns the token. Afterwards the tenant calls `RenewCertificate`,
  `RotateAccessKey` and `RemoveAccessKey` over mTLS.
- **Lab access links** are JWTs signed by the tenant with its access key (`iss` = tenant, `kid` = key id,
  `aud` = `laboratory-proxy`, `sub` = the VPN client, `nbf`/`exp`). The proxy reads the public keys of all tenants from
  `laboratory-tenants` and also checks that the lab group belongs to the issuer. There is no key shared between tenants.

Details, limits and the manual fallback are in [DEPLOY.md](DEPLOY.md#tenancy).

## Device state, image cache and group overhead

- **Device state persistence** (optional, `devices.statePersistence.enabled`): the writable layer of a device survives a
  crash or node loss. Each device chooses it in its topology (`persistence.enabled`, `debounce`); the cluster and the
  tenant policy bound it (`writeQuota`, `maxFileSize`, `excludePaths`). Snapshots go to the platform registry; the agent can
  export the latest state of a device as one archive (`ExportDeviceSnapshot`). See
  [Device state persistence](DEPLOY.md#device-state-persistence-optional) and [Snapshot export](DEPLOY.md#snapshot-export).
- **Image cache** (optional, `registry.cache.enabled`): zot is a pull-through cache for the images of labs. The operator pins
  the digest of each image when a lab is created, so a moving tag cannot change an image during an event, and the agent
  can prewarm the cache before an event (`PrewarmImages`). See [Image cache](DEPLOY.md#image-cache-optional).
- **Group overhead**: every lab group runs a VPN pod and a gateway pod; their resources are chart values
  (`vpn.resources`, `inetGateway.resources`, Guaranteed) and `GetCapacity` reports their sum
  (`group_overhead_cpu_millicores`, `group_overhead_memory_bytes`) for sizing a reservation.

## Domains

Three names are set by configuration (chart values); the agent's has a derived default:

| Name | Value | Points to |
|---|---|---|
| `*.<operator.baseDomain>` | for example `*.labs.<zone>` | the L7 proxy; web devices are `<device>-<code>.<baseDomain>` |
| `operator.publicVPNEndpoint` | for example `vpn.<zone>:51820` | the WireGuard demultiplexer |
| `agent.domain` | default `ctl.<operator.baseDomain>` | the management agent (gRPC, TLS) |

See [`docs/specs/domains.md`](docs/specs/domains.md) (Russian) for the naming rules and the planned region sub-domains.

## Management agent API

With `agent.enabled`, the chart deploys a gRPC server (default port 5454, TLS with mutual authentication; the tenant is
the CN of the client certificate, obtained by enrolling, see [Tenancy and enrollment](#tenancy-and-enrollment)). The service is
`LabManager`, defined in [`pkg/agent/protobuf/agent.proto`](pkg/agent/protobuf/agent.proto). The CRUD API is plural-only
(every call takes a list; one object is a list of one): `CreateLabGroups` / `ListLabGroups` / `UpdateLabGroups` /
`DeleteLabGroups`, the same four for `LabGroupClients` and `Labs`, `SetLabGroupAccess` (access policies of many groups),
`ResetDevices` / `RescueDevices`, `ExportDeviceSnapshot`, the enrollment calls `Enroll` / `RenewCertificate` /
`RotateAccessKey` / `RemoveAccessKey`, plus `Ping`, `GetCapacity`, `PrewarmImages` and a resumable, label-selectable
`Monitoring` stream (see [Monitoring stream](DEPLOY.md#monitoring-stream)). Every mutating call answers with a per-item
result, takes labels (request-level and per item), and selector calls are guarded by `expected_count`. The full list, the
rules and examples are in [Management agent API](DEPLOY.md#management-agent-api); device state calls are in
[Device state persistence](DEPLOY.md#device-state-persistence-optional). `Lab.scheduling` and `LabGroup.scheduling` carry
the scheduler state.
Go bindings are generated next to the proto file; a Go client is in [`pkg/agent/client`](pkg/agent/client).

## Development

```bash
make help            # list all targets
make manifests       # regenerate CRDs and RBAC (controller-gen)
make generate        # regenerate deepcopy code
make fmt vet         # format and vet
make lint            # golangci-lint (downloaded into ./bin)
make build           # build the operator binary into ./bin/manager
make test            # unit and envtest tests; writes cover.out
make test-e2e        # e2e tests against a kind cluster (see test/e2e)
```

- **Tests.** `make test` downloads the `kube-apiserver` and `etcd` binaries with `setup-envtest` into `./bin` and runs
  every package except the e2e suite, so controller tests run against a real API server without a cluster. Chart
  rendering tests live in [`test/chart`](test/chart).
- **Images.** `make docker-build` builds all five images (`make docker-build-controller|agent|proxy|node|lab` one of them); `make kind-deploy`
  builds them, loads them into a kind cluster and installs everything.
- **Clients.** Typed clientset and apply configurations are generated with `hack/update-codegen.sh`.
- **Local cluster.** The Makefile expects the local k0s/Lima cluster kit (VMs, Cilium Gateway, test scenarios) in a
  sibling infrastructure repository, configured by `LOCAL_K0S` (default `../infrastructure/local/cluster`).

## Documentation

- [`DEPLOY.md`](DEPLOY.md): install, configure, upgrade, use, uninstall, scheduler, management agent API, tenancy and enrollment, device state persistence, image cache and prewarm, monitoring stream, snapshot export.
- [`docs/specs/`](docs/specs/): component specifications (Russian): [operator](docs/specs/operator.md),
  [controller design](docs/specs/controller-design.md), [node-agent](docs/specs/node-agent.md),
  [CNI plugins](docs/specs/cni-plugins.md), [VPN](docs/specs/vpn.md), [proxy](docs/specs/proxy.md),
  [gateway](docs/specs/gateway.md), [domains](docs/specs/domains.md).

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
