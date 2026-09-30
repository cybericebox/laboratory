# Laboratory

Laboratory is the lab-infrastructure layer of the Cyber ICE Box platform. It runs isolated, per-team virtual networks
on a Kubernetes cluster and gives each team access to its own lab over WireGuard or through an authenticated HTTPS
front for web challenges.

It consists of:

- a **Kubernetes operator** that reconciles a small CRD graph (`LabGroup`, `Lab`, `Device`, `Connection`,
  `LabGroupClient`, `Pool`) into namespaces, pods, services and network policies;
- a **management agent**, a gRPC API (mTLS) through which a platform backend drives the operator;
- a **node-agent** DaemonSet that programs Open vSwitch (OVS) on every node and plugs lab devices into L2 segments;
- a **WireGuard VPN** server per lab group, reached through a shared **VPN demultiplexer**;
- a shared **L7 HTTPS proxy** that fronts web challenges;
- a per-group **internet gateway** (egress-only NAT, optional DHCP);
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

- A **LabGroup** is one tenant (for example a team). It gets its own namespace, WireGuard server, internet gateway and
  address space. Lab networks of different groups are isolated from each other.
- A **Lab** declares devices and connections between them. The operator materialises **Device** pods and **Connection**
  segments; the node-agent plugs each device into an OVS L2 segment, across nodes if needed.
- A **LabGroupClient** is one WireGuard peer. The operator generates its key pair and a ready-to-use client config.
- **Pool** objects allocate VNIs, IPs and lab subnets.
- The **proxy** runs two containers on one public IP. `proxy-wg` demultiplexes incoming WireGuard UDP to the right
  group's VPN server without terminating VPN traffic. `proxy-l7` terminates TLS for `*.<baseDomain>`, verifies the
  participant's session (seeded from an Ed25519-signed lab access link issued by the platform) and routes the
  request to the challenge's web service.

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

- Every push to `develop` builds all images and tags them `develop` and `sha-<short commit>` (workflow `develop-images.yml`).
- Publishing a GitHub release `vX.Y.Z` builds them with the tags `X.Y.Z` and `latest`, packages the Helm chart with
  `version` and `appVersion` set to `X.Y.Z`, pushes it to `oci://registry-1.docker.io/cybericebox/laboratory` and
  attaches the `.tgz` to the release (workflow `release.yml`).
- Chart image tags default to the chart `appVersion`; set `*.image.tag` in values to pin another tag (for example `develop`).

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
- **cert-manager** (chart dependency) for the proxy wildcard certificate and the agent's mTLS certificates. With an
  ACME issuer the wildcard certificate needs a DNS-01 solver (Cloudflare is supported by the chart); a self-signed
  issuer is available for development.
- `kubectl` 1.28+, `helm` 3.12+.
- To build from source: the Go version in `go.mod`, Docker, and `make`.

## Quick start (Helm)

The chart is in [`charts/laboratory`](charts/laboratory). It must be installed into the `laboratory-system`
namespace; `templates/validate.yaml` rejects anything else and fails early when a required value is missing.

1. Generate the lab access key pair. The backend signs lab access links with the private key; the proxy verifies them
   with the public key.

   ```bash
   make lab-access-keys   # /tmp/lab-access-private.pem, /tmp/lab-access-public.pem
   kubectl create namespace laboratory-system
   kubectl -n laboratory-system create secret generic lab-access-public-key \
     --from-file=public.pem=/tmp/lab-access-public.pem
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
     publicVPNEndpoint: vpn.example.com:51820     # REQUIRED: host:port handed to WireGuard clients
     supportEmail: support@example.com            # shown on the VPN probe page
   agent:
     enabled: true
     domain: agent.example.com                    # REQUIRED when the agent is enabled
   proxy:
     wg:
       nodePort: 31820                            # optional fixed NodePort of the WireGuard service
   imagePullSecrets:
     - name: registry-credentials
   ```

   All options are documented in [`charts/laboratory/values.yaml`](charts/laboratory/values.yaml).

5. Install:

   ```bash
   helm dependency update charts/laboratory
   helm install laboratory charts/laboratory \
     --namespace laboratory-system --create-namespace \
     -f my-values.yaml
   ```

6. Create a lab group and a lab. [`DEPLOY.md`](DEPLOY.md) walks through `LabGroup`, `LabGroupClient` and `Lab`
   examples, upgrades and uninstalling (CRDs are not removed by `helm uninstall`).

## Management agent API

With `agent.enabled`, the chart deploys a gRPC server (default port 5454, TLS with mutual authentication; client
certificates are issued by the chart for the CNs listed in `agent.clients`). The service is `LabManager`, defined in
[`pkg/agent/protobuf/agent.proto`](pkg/agent/protobuf/agent.proto). It covers `Ping`, CRUD for `LabGroup`, `Lab` and
`LabGroupClient`, suspend and VPN-disable switches for a group, access-policy reconciliation, a `Monitoring` stream
and `GetCapacity`. Go bindings are generated next to the proto file; a Go client is in [`pkg/agent/client`](pkg/agent/client).

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
  sibling infrastructure repository, configured by `LOCAL_K0S` (default `../infra/local/cluster`).

## Documentation

- [`DEPLOY.md`](DEPLOY.md): install, configure, upgrade, use, uninstall.
- [`docs/specs/`](docs/specs/): component specifications (Russian): [operator](docs/specs/operator.md),
  [controller design](docs/specs/controller-design.md), [node-agent](docs/specs/node-agent.md),
  [CNI plugins](docs/specs/cni-plugins.md), [VPN](docs/specs/vpn.md), [proxy](docs/specs/proxy.md),
  [gateway](docs/specs/gateway.md), [domains](docs/specs/domains.md).

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
