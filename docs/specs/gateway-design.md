# Gateway Binary Design Specification

## Overview

Gateway binary runs as a pod in the LabGroup namespace. Manages per-lab internet connectivity:

- Assigns `10.192.N.1/24` to `lab{N}` interface
- Configures NAT (MASQUERADE) for lab subnet → internet
- Optionally starts DHCP server bound to `10.192.N.1`

Watches `LabGateway` CRs (new CRD). Does NOT watch `Lab` directly.

---

## 1. New CRD: `LabGateway`

### Go types (new file `api/laboratory/v1alpha1/labgateway_types.go`)

```go
type LabGatewaySpec struct {
    // LabName is the name of the parent Lab. Provided by user or LabReconciler.
    LabName string `json:"labName"`
    // NetworkIndex is the subnet index N, derived from lab-subnets pool.
    // Filled by main controller before adding its finalizer. Read-only after that.
    NetworkIndex uint `json:"networkIndex,omitempty"`
    // CIDR is 10.192.N.0/24, filled by main controller.
    CIDR string `json:"cidr,omitempty"`
}

type LabGatewayStatus struct {
    Phase       LabGatewayPhase `json:"phase,omitempty"`
    NATReady    bool            `json:"natReady,omitempty"`
    DHCPEnabled bool            `json:"dhcpEnabled,omitempty"`
    DHCPReady   bool            `json:"dhcpReady,omitempty"`
}

// +kubebuilder:validation:Enum=Pending;WaitingForInterface;Configuring;Ready
type LabGatewayPhase string

const (
    LabGatewayPhasePending              LabGatewayPhase = "Pending"
    LabGatewayPhaseWaitingForInterface  LabGatewayPhase = "WaitingForInterface"
    LabGatewayPhaseConfiguring          LabGatewayPhase = "Configuring"
    LabGatewayPhaseReady                LabGatewayPhase = "Ready"
)
```

### Namespace

Same namespace as the LabGroup (not `laboratory-system`). Main controller creates it in the LabGroup namespace.

### OwnerRef

`LabGateway.OwnerReference → Lab` — auto-deleted when Lab is deleted.

---

## 2. Interface naming

Replace `ovsnames.LabGWIfaceName(labName string)` with:

```go
// GatewayIfaceName returns the interface name inside the gateway pod (lab1..lab254).
// Same naming as VPN pod — both use lab{N} within their own network namespace.
func GatewayIfaceName(networkIndex uint) string {
    return fmt.Sprintf("lab%d", networkIndex)
}
```

Name derived from `LabGateway.Spec.NetworkIndex`. No K8s lookup needed.

---

## 3. Finalizers

```
cybericebox.com/controller  — main controller (set before gateway binary starts processing)
cybericebox.com/gateway     — gateway binary
```

Gateway binary rule:

- `cybericebox.com/controller` absent → skip entirely
- `cybericebox.com/controller` present + own finalizer absent → add own finalizer → proceed
- Own finalizer present → reconcile
- DeletionTimestamp + own finalizer → cleanup → remove own finalizer

---

## 4. Reconcile Logic (`internal/gateway/reconciler.go`)

```
Reconcile(ctx, req):
  Get LabGateway by req.NamespacedName
  if NotFound → return nil

  // 1. Guard: main controller must have processed first
  if !ContainsFinalizer(gw, "cybericebox.com/controller"):
    return nil  // not our turn yet

  // 2. Deletion path
  if gw.DeletionTimestamp != nil:
    if ContainsFinalizer(gw, "cybericebox.com/gateway"):
      return reconcileDelete(ctx, gw)
    return nil

  // 3. Add own finalizer
  if !ContainsFinalizer(gw, "cybericebox.com/gateway"):
    AddFinalizer(gw, "cybericebox.com/gateway")
    Update(ctx, gw)
    return nil  // requeue will fire

  // 4. Wait for lab{N} interface
  N := gw.Spec.NetworkIndex
  ifaceName := fmt.Sprintf("lab%d", N)
  if !InterfaceExists(ifaceName):
    if gw.Status.Phase != WaitingForInterface:
      patchStatus(gw, Phase=WaitingForInterface)
    return requeueAfter(5s)

  // 5. Assign gateway IP
  gwIP := firstHostIP(gw.Spec.CIDR)  // 10.192.N.1
  AssignIP(ifaceName, gw.Spec.CIDR)  // also adds connected route automatically

  // 6. NAT
  IPT.AddMasquerade(gw.Spec.CIDR)

  // 7. DHCP (optional)
  poolName := fmt.Sprintf("dhcp-inet-%s-0", gw.Spec.LabName)
  dhcpEnabled := poolExists(ctx, poolName, gw.Namespace)
  if dhcpEnabled:
    DHCP.Start(gw.Spec.LabName, Config{
      Iface:   ifaceName,
      Subnet:  gw.Spec.CIDR,
      Gateway: gwIP,
      BindIP:  gwIP,          // bind to 10.192.N.1:67, not 0.0.0.0:67
      DNS:     cfg.DHCPDNS,
    })

  // 8. Update status
  patchStatus(gw, Phase=Ready, NATReady=true, DHCPEnabled=dhcpEnabled, DHCPReady=dhcpEnabled)
  return nil
```

### reconcileDelete

```
reconcileDelete(ctx, gw):
  DHCP.Stop(gw.Spec.LabName)
  if gw.Spec.CIDR != "":
    IPT.DelMasquerade(gw.Spec.CIDR)
    // kernel route removed automatically when IP is removed from interface
    RemoveIP(fmt.Sprintf("lab%d", gw.Spec.NetworkIndex), gw.Spec.CIDR)
  RemoveFinalizer(gw, "cybericebox.com/gateway")
  Update(ctx, gw)
  return nil
```

### Status writes

All status writes use `Status().Patch()`, never `Status().Update()`.

---

## 5. iptables Rules

### Startup rules (set once on pod start, before reconcile loop)

```
iptables -P FORWARD DROP
iptables -A FORWARD -o {extIface} -j ACCEPT
iptables -A FORWARD -i {extIface} -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
```

Effect:

- `lab{N} → eth0`: ALLOW (lab → internet)
- `eth0 → lab{N}`: ALLOW only established/related (return traffic)
- `lab{N} → gw{M}`: DROP (cross-lab blocked)

`{extIface}` = `Config.ExternalInterface` (default `eth0`, effectively hardcoded for gateway pod).

### Per-lab NAT rule (dynamic, added on LabGateway create, removed on delete)

```
iptables -t nat -A POSTROUTING -s 10.192.N.0/24 -o {extIface} -j MASQUERADE
```

One rule per lab. Never modified after creation.

---

## 6. DHCP pool check

Gateway binary needs K8s client to check pool existence.

```go
// Check if DHCP pool exists (Get by known name — no list needed)
var pool allocationv1alpha1.Pool
err := r.Get(ctx, types.NamespacedName{
    Name:      fmt.Sprintf("dhcp-inet-%s-0", gw.Spec.LabName),
    Namespace: gw.Namespace,
}, &pool)
dhcpEnabled := err == nil  // NotFound = no DHCP
```

RBAC: Gateway SA has `get` on pools with `resourceNames: [dhcp-inet-*]`.

---

## 7. DHCP bind fix

Current bug: `net.UDPAddr{Port: 67, IP: net.ParseIP("0.0.0.0")}` — conflicts across labs.

Fix: bind to interface-specific IP:

```go
// In pkg/dhcp: add BindIP to Config
laddr := &net.UDPAddr{Port: 67, IP: net.ParseIP(cfg.BindIP)}
```

`BindIP = gwIP` (first host IP of subnet, e.g. `10.192.3.1`).

---

## 8. SetupWithManager

```go
func (r *LabGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&laboratoryv1alpha1.LabGateway{}).
        Complete(r)
}
```

Watch `LabGateway`, not `Lab`.

---

## 9. Config changes

`internal/gateway/config.go` — no changes needed. Gateway binary already has:

- `Namespace` — LabGroup namespace (for pool lookup)
- `ExternalInterface` — default `eth0`
- `DHCPDNS`

---

## 10. Files to create/modify

| File                                               | Action                                                                       |
|----------------------------------------------------|------------------------------------------------------------------------------|
| `api/laboratory/v1alpha1/labgateway_types.go`      | CREATE — LabGateway CRD types                                                |
| `api/laboratory/v1alpha1/zz_generated.deepcopy.go` | REGENERATE — `make generate`                                                 |
| `internal/ovsnames/names.go`                       | MODIFY — add `GatewayIfaceName(n uint)`, deprecate `LabGWIfaceName`          |
| `internal/gateway/reconciler.go`                   | REWRITE — watch LabGateway, finalizer lifecycle                              |
| `internal/gateway/iptables.go`                     | MODIFY — add `SetupForwardRules()` startup method                            |
| `pkg/dhcp/dhcp.go`                                 | MODIFY — add `BindIP` to Config, fix `0.0.0.0` bind                          |
| `cmd/gateway/main.go`                              | MODIFY — register LabGateway scheme, call `IPT.SetupForwardRules()` on start |

---

## 11. What main controller must do (LabReconciler changes — implement later)

When `Lab.Status.Index` is set:

1. Create `LabGateway` in LabGroup namespace with:
    - `spec.labName = lab.Name`
    - `spec.networkIndex = lab.Status.Index`
    - `spec.cidr = 10.192.{N}.0/24`
    - `ownerRef = Lab`
2. Add finalizer `cybericebox.com/controller`
3. Patch VPN pod annotations to add OVS port `lab{N}`

When `LabGateway.Status.Phase == Ready` → contribute to `Lab.Status.Phase` aggregation.

---

## 12. Open questions

- **Q6 (DHCP persistence)**: Pool CRD bitmap + ConfigMap vs ConfigMap-only. DHCP still uses in-memory `byMAC map`.
  Decision needed before implementing DHCP persistence.
- **Q_forward_rules**: Does Gateway pod need `NET_ADMIN` capability and `privileged: true` for iptables? Verify current
  pod SecurityContext in LabGroup Deployment template.
