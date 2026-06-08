# Laboratory Controller Design Specification

## 1. Addressing Scheme

### Global network

```
10.128.0.0/9  — global range (configurable via operator config)
  10.128.0.0/10 — VPN segments     (first /10)
  10.192.0.0/10 — Internet/Gateway (second /10)
```

Both halves are derived from the single `/9` base. Configuration stores only the `/9`; halves are computed.

### Allocation

- Index `N` allocated from `lab-subnets` pool (namespace: LabGroup, offset=1, size=254)
- `N=0` reserved: `10.128.0.0/24` = VPN clients subnet for LabGroup
- Lab `N` gets:
  - VPN segment:      `10.128.N.0/24`
  - Internet segment: `10.192.N.0/24`
- VPN client IPs: `10.128.0.{idx}/32`, allocated from `vpn-clients` pool (offset=0, size=254)
- Client `AllowedIPs` in WireGuard config: `10.128.0.0/9` (full range, routing rules on server enforce isolation)

### Symmetry

Same index `N` for both VPN and Internet subnets of a lab — one allocation covers both.
Internet `N=0` is unused (reserved for symmetry, no pool needed).

### Configuration

`operator.Config` must expose:
```go
GlobalNetwork string `env:"GLOBAL_NETWORK" envDefault:"10.128.0.0/9"`
```
VPN base = first /10 of GlobalNetwork. Internet base = second /10.

### Interface naming convention

```
lab{N}   interface inside VPN pod     (e.g. lab1, lab2, ... lab254)
lab{N}   interface inside Gateway pod (same name, different network namespace — no collision)
```

`N` = subnet index from `lab-subnets` pool. From N, any component derives CIDRs:
- VPN:      `10.128.N.0/24` (first /10 of GlobalNetwork)
- Internet: `10.192.N.0/24` (second /10 of GlobalNetwork)

Binary extracts N from interface name by stripping prefix (`lab` or `gw`). No K8s query needed.

`ovsnames` functions take `index uint`, not `labName string`. No SHA256 hashing.

---

## 2. CRD Inventory

| CRD | Group | Who creates | Spec owner | Status owner |
|---|---|---|---|---|
| `LabGroup` | laboratory | user | main controller | main controller |
| `LabGroupClient` | laboratory | user | main controller (fills) | main + VPN binary |
| `Lab` | laboratory | user | main controller (fills) | main controller |
| `LabVPN` | laboratory | main controller (from Lab) | main controller (fills) | VPN binary |
| `LabGateway` | laboratory | main controller (from Lab) | main controller (fills) | Gateway binary |
| `Device` | laboratory | main controller | main controller | main controller |
| `Connection` | laboratory | main controller | main controller | node-agent |
| `Pool` | allocation | main controller | — | allocator |

### Lab.Status (simplified)

```go
Status:
  Phase: Pending | Configuring | Ready
  Index: uint    // N — subnet index from lab-subnets pool
                 // Derives: lab{N} interface (both VPN and Gateway pods),
                 //          10.128.N.0/24 VPN CIDR, 10.192.N.0/24 Internet CIDR
```

VPN/Internet readiness is tracked in `LabVPN.Status` and `LabGateway.Status` respectively.

---

## 3. Component Responsibilities

### Main Controller (`laboratory-system`)

| Controller | Resource | Actions |
|---|---|---|
| `LabGroupReconciler` | `LabGroup` | Namespace, VPN/Gateway Deployment, SA, RoleBindings, `lab-subnets-0` pool, VPN server keypair Secret |
| `LabReconciler` | `Lab` | Subnet IPAM (sets `Lab.Status.Index`), web Service/NP/Ingress, Device+Connection CRs, DHCP pool creation, `LabVPN`/`LabGateway` CR creation, `Lab.Status.Phase` |
| `LabGroupClientReconciler` | `LabGroupClient` | Keypair generation, Secret, IP allocation, Status fields, `cybericebox.com/controller` finalizer |
| `DeviceReconciler` | `Device` | Pod lifecycle, `Device.Status.{PodIP,NodeName,Ready}` |
| `ConnectionReconciler` | `Connection` | Reads `Connection.Status.Ports` → sets `Connection.Status.Ready` |

### VPN Binary (pod in LabGroup namespace)

| Controller | Resource | Actions |
|---|---|---|
| `LabGroupClientReconciler` | `LabGroupClient` | WG peer add/remove, `cybericebox.com/vpn` finalizer, `Status.Phase=Ready`, Statistics |
| `LabVPNReconciler` | `LabVPN` | `cybericebox.com/vpn` finalizer, wait for `lab{N}` interface (netlink), IP assign, WG route, DHCP start/stop, `LabVPN.Status.Phase` |

### Gateway Binary (pod in LabGroup namespace)

| Controller | Resource | Actions |
|---|---|---|
| `LabGatewayReconciler` | `LabGateway` | `cybericebox.com/gateway` finalizer, wait for `gw{N}` interface (netlink), IP assign, NAT/masquerade, DHCP start/stop, `LabGateway.Status.Phase` |

### Node-Agent (DaemonSet)

| Controller | Resource | Actions |
|---|---|---|
| `ConnectionReconciler` | `Connection` | OVS flows, Geneve tunnels, `Connection.Status.Ports`, `cybericebox.com/node-agent` finalizer |
| `DevicePortReconciler` | `Device` | OVS port lifecycle (does NOT patch Device.Status) |
| `NetworkAttachReconciler` | NetworkAttach | OVS port for Multus attachments |

---

## 4. Finalizer Map

One finalizer name per binary. Same finalizer reused across all resource types that binary owns.

| Binary | Finalizer | Applied to |
|---|---|---|
| Main controller | `cybericebox.com/controller` | `LabGroupClient`, `LabVPN`, `LabGateway` |
| VPN binary | `cybericebox.com/vpn` | `LabGroupClient`, `LabVPN` |
| Gateway binary | `cybericebox.com/gateway` | `LabGateway` |
| Node-agent | `cybericebox.com/node-agent` | `Connection` |

No finalizers on Pods. Port cleanup for VPN/Gateway pods happens naturally when pod network namespace is destroyed.

---

## 5. LabGroupClient Lifecycle

### State machine

```
Phases: Pending → Ready
Finalizers:
  cybericebox.com/controller  — main controller
  cybericebox.com/vpn         — VPN binary
```

### CREATE flow

```
1. LabGroupClient created (Spec.PublicKey may be empty)

Main controller:
  a. If Spec.PublicKey == "": generate WireGuard keypair
     → create Secret {name: "client-{lgcName}", namespace: labgroup-ns}
       ownerRef: LabGroupClient (auto-deleted with client)
       data: {privateKey, publicKey}
     → patch Spec.PublicKey = pubkey
  b. Allocate IP from vpn-clients pool → Status.AssignedIP
  c. Fill Status:
       AssignedIP, SecretRef, ServerPublicKey (from LabGroup.Status),
       VPNEndpoint (public proxy address), AllowedIPs, PersistentKeepalive
  d. Add finalizer: cybericebox.com/controller
  e. Status.Phase = "Pending"
  f. STOPS — never reconciles this resource again (until deletion)

VPN binary (triggered by cybericebox.com/controller finalizer presence):
  a. Register WG peer: Spec.PublicKey + Status.AssignedIP
  b. Add finalizer: cybericebox.com/vpn
  c. Status.Phase = "Ready"
  d. Periodically update Status.Statistics (LastHandshake, RxBytes, TxBytes)
```

### DELETE flow

```
1. DeletionTimestamp set

Main controller:
  - sees cybericebox.com/vpn present → skip, wait

VPN binary:
  - sees cybericebox.com/vpn present → remove WG peer
  - remove finalizer: cybericebox.com/vpn

Main controller (re-triggered, only cybericebox.com/controller remains):
  - release IP back to vpn-clients pool
  - remove finalizer: cybericebox.com/controller
  → Secret auto-deleted (ownerRef)
```

### Immutability

Validation webhook: reject `Spec` changes when `cybericebox.com/controller` finalizer is set.
Exception: `Spec.PublicKey` may be set on first admission if empty.

### Security

- VPN binary RBAC: NO access to `secrets` resource
- Secret with private key stays in LabGroup namespace, accessible only to main controller + end user
- VPN binary reads only `Spec.PublicKey` (public key only)

---

## 6. LabVPN / LabGateway Lifecycle

### CRD structure

```yaml
# LabVPN
spec:
  labName:      string  # ref to Lab — provided by user or LabReconciler
  networkIndex: uint    # N — filled by main controller before adding its finalizer
  cidr:         string  # 10.128.N.0/24 — filled by main controller

status:
  phase:       Pending | WaitingForInterface | Configuring | Ready
  dhcpEnabled: bool    # true if dhcp-vpn-{labName}-0 pool exists
  dhcpReady:   bool
```

```yaml
# LabGateway — same structure, cidr = 10.192.N.0/24, plus natReady bool
spec:
  labName:      string
  networkIndex: uint
  cidr:         string

status:
  phase:       Pending | WaitingForInterface | Configuring | Ready
  natReady:    bool
  dhcpEnabled: bool
  dhcpReady:   bool
```

### Finalizers

```
cybericebox.com/controller  — main controller
cybericebox.com/vpn         — VPN binary      (LabVPN only)
cybericebox.com/gateway     — Gateway binary  (LabGateway only)
```

### CREATE flow

```
Main controller (from LabReconciler, when Lab gets Status.Index):
  1. Verify uniqueness: one LabVPN per labName
  2. Create LabVPN with spec.labName; fill spec.networkIndex, spec.cidr
  3. Patch VPN pod annotations: add OVS port annotation for lab{N}
  4. Add finalizer: cybericebox.com/controller
  5. LabVPN.Status.Phase = Pending

VPN binary (sees cybericebox.com/controller):
  1. Add finalizer: cybericebox.com/vpn
  2. Status.Phase = WaitingForInterface
  3. Loop: check lab{N} interface via netlink
     if absent → Status.Phase = WaitingForInterface → requeueAfter(5s)
  4. Assign IP 10.128.N.1/24 to lab{N}
  5. Add kernel route 10.128.N.0/24 via lab{N}
  6. Check dhcp-vpn-{labName}-0 pool → if exists, start DHCP bound to 10.128.N.1
  7. Status.Phase = Ready
```

Same pattern for LabGateway / Gateway binary with `lab{N}` (own network namespace) and `10.192.N.x`.

### DELETE flow

```
DeletionTimestamp set on LabVPN/LabGateway

VPN/Gateway binary (sees own finalizer + DeletionTimestamp):
  1. Stop DHCP server (if running)
  2. Remove kernel route
  3. Remove IP from interface
  4. Remove own finalizer (cybericebox.com/vpn or cybericebox.com/gateway)

Main controller (only cybericebox.com/controller remains):
  1. Patch pod annotations: remove OVS port annotation
  2. Remove finalizer: cybericebox.com/controller
```

### VPN/Gateway binary reconcile logic

```
DeletionTimestamp + own finalizer present → cleanup → remove own finalizer
No deletion + cybericebox.com/controller present + own finalizer absent → add own finalizer → configure
No deletion + own finalizer present → reconcile / update status
No deletion + cybericebox.com/controller absent → skip
```

### Immutability

Validation webhook: reject `Spec` changes when `cybericebox.com/controller` finalizer is set.
Applies to: `LabGroupClient`, `LabVPN`, `LabGateway`.

---

## 7. DHCP Pool Management

### Who creates

Main controller (`LabReconciler`) creates DHCP pool when `Lab.Spec.{VPN,Internet}.DHCPServer.Enabled = true`.

```
Pool name:   dhcp-vpn-{labName}-0   (for VPN segment)
             dhcp-inet-{labName}-0  (for Internet segment)
Namespace:   LabGroup namespace
OwnerRef:    Lab → auto-deleted when Lab deleted
Size:        252 (subnet /24 minus network, broadcast, gateway)
Offset:      2   (skip .0 network address and .1 gateway IP)
```

### Who uses

- VPN binary: checks if `dhcp-vpn-{labName}-0` exists → if yes, start DHCP server on lab{N} interface
- Gateway binary: checks if `dhcp-inet-{labName}-0` exists → if yes, start DHCP server on gw{N} interface

### Persistence

Pool CRD `Status.BitMap` (base64 bitset) persists IP allocations across pod restarts.
MAC→IP mapping stored in `ConfigMap dhcp-{segment}-{labName}` (same namespace, same ownerRef as Pool).

### DHCP server bind

Bind to interface-specific IP, not `0.0.0.0:67`:
- VPN DHCP: bind to `10.128.N.1:67`
- Gateway DHCP: bind to `10.192.N.1:67`

---

## 8. VPN Pod iptables Rules

Startup (once on pod start, before any lab is configured):

```
iptables -P FORWARD DROP
iptables -A FORWARD -i wg0  -o lab+ -j ACCEPT   # client → lab
iptables -A FORWARD -i lab+ -o wg0  -j ACCEPT   # lab → client
iptables -A FORWARD -i wg0  -o wg0  -j DROP     # client → client (block)
```

These rules are static. Per-lab routes are added/removed by `LabVPNReconciler` via kernel route table.

---

## 9. RBAC Design

### VPN binary ServiceAccount (`vpn`)

```yaml
Role vpn-access (namespace-scoped, dynamic resourceNames):
  - allocation.cybericebox.com/pools: get, update, patch
    resourceNames: [dhcp-vpn-{lab}-0, ...]
  - laboratory.cybericebox.com/labgroupclients: get, list, watch, update, patch
  - laboratory.cybericebox.com/labgroupclients/status: get, update, patch
  - laboratory.cybericebox.com/labvpns: get, list, watch, update, patch
  - laboratory.cybericebox.com/labvpns/status: get, update, patch
  - "" (core)/secrets: FORBIDDEN
```

### Gateway binary ServiceAccount (`gateway`)

```yaml
Role gateway-access (namespace-scoped, dynamic resourceNames):
  - allocation.cybericebox.com/pools: get, update, patch
    resourceNames: [dhcp-inet-{lab}-0, ...]
  - laboratory.cybericebox.com/labgateways: get, list, watch, update, patch
  - laboratory.cybericebox.com/labgateways/status: get, update, patch
  - "" (core)/secrets: FORBIDDEN
```

### Why resourceNames works here

`list` + `watch` cannot be filtered by `resourceNames` in K8s RBAC. Mitigation:
- Allocator uses `get` by known name (not `list`) when the pool name is deterministic
- Pool names follow fixed convention → always computable from context

---

## 10. Known Bugs / Conflicts (fix before new features)

- [ ] `AllowedIPs` in LabGroupClient config: hardcoded `10.8.0.0/16`. Must change to `{GlobalNetwork}` (e.g. `10.128.0.0/9`).
- [ ] Addressing: `vpnSubnetOctet2=8`, `inetSubnetOctet2=9` are hardcoded. Must be derived from configurable `GlobalNetwork`.
- [ ] DHCP binds to `0.0.0.0:67` — conflicts when multiple labs have DHCP on same pod. Fix: bind to interface-specific IP.

Resolved by architecture (no longer bugs):
- ~~`Lab.Status` concurrent write~~ → VPN/Gateway now write only to their own LabVPN/LabGateway CRDs

---

## 11. Open Design Questions

### Q1: Who creates `LabGroupClient` resources?

External API/frontend, or main controller auto-creates them? Current code: external creation assumed. Not yet decided for auto-provisioning flows.

### Q2+Q3: VPN routing isolation + LabGroupClient↔Lab association ✅ RESOLVED

Each client has access to ALL labs in the LabGroup. No per-client lab restriction.

Isolation rules:
- Client → any lab in group: **ALLOW** (AllowedIPs = full `GlobalNetwork`)
- Client → other clients: **BLOCK** via iptables on VPN pod: `FORWARD -i wg0 -o wg0 -j DROP`
- Client → other LabGroups: **no routes** (each group has its own VPN pod, no cross-group routing)

Client WireGuard config `AllowedIPs = {GlobalNetwork}` (e.g. `10.128.0.0/9`).

No `Spec.LabName` field needed on `LabGroupClient`.

### Q4: `resourceNames` + `list` — Allocator redesign ✅ RESOLVED (Option B)

Redesign allocator to `Get` by sequential names (`{prefix}-0`, `{prefix}-1`, ...) — no `list` needed. Full `resourceNames` isolation.

Requires changing `pkg/api/pool/pool.go` allocator logic.

### Q5: `vpn-clients` pool — main controller or VPN binary? ✅ RESOLVED

Main controller owns `vpn-clients` pool. VPN binary has NO access to `vpn-clients` pool.

### Q6: DHCP persistence — Pool CRD + ConfigMap, or ConfigMap only?

Option A: Pool CRD (bitmap for IP tracking) + ConfigMap (MAC→index map). Consistent with other allocation pools.
Option B: Single ConfigMap with `{mac: ip}` JSON. Simpler, self-contained.

**DECISION NEEDED.**

---

## 12. Implementation Tasks

### New CRDs
- [ ] Define `LabVPN` CRD (spec: labName, index, cidr; status: phase, dhcpEnabled, dhcpReady)
- [ ] Define `LabGateway` CRD (spec: labName, index, cidr; status: phase, natReady, dhcpEnabled, dhcpReady)
- [ ] Validation webhook: immutable `Spec` for `LabGroupClient`, `LabVPN`, `LabGateway` after `cybericebox.com/controller` finalizer set

### Lab.Status
- [ ] Remove VPN.CIDR, VPN.Ready, Internet.CIDR, Internet.Ready from Lab.Status
- [ ] Add `Lab.Status.Index` (uint)

### Main Controller
- [ ] `GlobalNetwork` config field, derive `/10` halves, replace hardcoded octets
- [ ] `LabGroupClient` new lifecycle: finalizer-based (cybericebox.com/controller)
- [ ] `LabGroupClientStatus`: add `Phase`, `ServerPublicKey`, `VPNEndpoint`, `AllowedIPs`, `PersistentKeepalive`
- [ ] `LabReconciler`: create `LabVPN` + `LabGateway` CRs, set `Lab.Status.Index`
- [ ] Dynamic Role patching: when DHCP pool created → add `resourceNames` to VPN/Gateway Role
- [ ] Allocator redesign: `Get`-only by sequential names (Q4 resolved)

### VPN Binary
- [ ] Switch from watching `Lab` → watch `LabVPN`
- [ ] Implement `LabVPNReconciler` with finalizer-based lifecycle
- [ ] Startup iptables FORWARD rules
- [ ] Fix AllowedIPs: use `GlobalNetwork` instead of hardcoded `10.8.0.0/16`

### Gateway Binary
- [ ] Switch from watching `Lab` → watch `LabGateway`
- [ ] Implement `LabGatewayReconciler` with finalizer-based lifecycle
- [ ] Startup iptables FORWARD + NAT rules

### Both Binaries
- [ ] Fix DHCP bind: interface-specific IP instead of `0.0.0.0:67`
- [ ] DHCP persistence: Pool CRD + ConfigMap (pending Q6 decision)
