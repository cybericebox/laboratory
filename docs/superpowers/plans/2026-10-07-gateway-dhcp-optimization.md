# Gateway and DHCP Optimization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Preserve the execution method selected by the owner.

**Goal:** Исправить штатный запуск, аренды и жизненный цикл общего DHCP, сохранив NAT и изоляцию gateway и избегая повторной настройки от неизменных данных.

**Architecture:** pkg/dhcp владеет арендами и сокетом каждого lab-facing сервера. VPN/gateway controllers читают конфигурацию соответствующей лаборатории, обрабатывают изменения Lab/Pool и отражают реальную готовность DHCP. Настройки NAT и сетевые защиты gateway остаются в текущем компоненте.

**Tech Stack:** Go, insomniacslk/dhcp, controller-runtime, Kubernetes RBAC, iptables, Docker Linux network namespaces.

**Spec:** `docs/superpowers/specs/2026-10-07-vpn-gateway-dhcp-design.md`, раздел «Gateway и общий DHCP».

## Global Constraints

- Рабочая ветка, выбранная владельцем: `fix/envtest-crd-warmup`.
- Прокси остаётся следующим этапом.
- Сохранить NAT, защиту подмены адреса и ограничения выхода лабораторий во внешние сети.
- Удаление статистических DROP-пар VPN не отменяет изоляцию gateway.
- Изменение одной лаборатории не затрагивает другую.
- Не относящееся к выдаче сообщение не должно занимать новый адрес.
- Нельзя оставлять `DHCPReady=true` после остановки обслуживающего сокета.
- push, PR и релиз не входят в разрешённую работу.
- Новый AWS-стенд сейчас не создаётся.

## Review Focus

1. Клиент выбирает другой DHCP-сервер, присылает чужой адрес или RELEASE: этот пакет не должен выдавать/освобождать чужую аренду — Task 2.
2. DISCOVER не заканчивается REQUEST: предложение не занимает адрес навсегда и не считается активной суточной арендой — Task 1.
3. Диапазон сужается при действующих арендах: занятый адрес не выдаётся другому, DNS меняется без потери действующих арендаторов — Task 3.
4. Старый сокет завершает Serve после обновления сервера: поздняя ошибка старой generation не переводит новый сервер в Failed/NotReady — Task 3.
5. Нет прав, недоступен API либо DHCP-пул не найден: NotFound означает отсутствие, остальные ошибки не маскируются как выключенный DHCP — Task 4.

## File structure and ownership

| Unit | Files | Responsibility |
|---|---|---|
| Lease allocation | `pkg/dhcp/pool.go`, new `lease_test.go` | Offers, commits, renewals, expiry, release, range changes |
| Protocol | new `pkg/dhcp/handler.go`, `handler_test.go`, modify `dhcp.go` | DHCP state/identity checks and replies |
| Server lifecycle | new `pkg/dhcp/server.go`, `server_test.go` | Config comparison, synchronous stop, real health and stale-generation protection |
| Integration | `internal/gateway/reconciler.go`, `internal/vpn/reconciler/lab.go`, command/setup files | Per-lab config watches, DHCP health notifications, unchanged-state cache |
| Permissions | `charts/laboratory/templates/operator/clusterrole-gateway.yaml`, role/namespace tests | Narrow Labs read permission and actual access proof |

## Task 1: Leases with expiry, offers and ownership checks

**Files:** modify `pkg/dhcp/pool.go`, `dhcp_test.go`; create `lease_test.go`.

**Interfaces:** retain `newIPPool(*net.IPNet, net.IP, []Range) *ipPool` and `Allocate(net.HardwareAddr) (net.IP,error)` for callers/tests. Add these operations:

```go
func (p *ipPool) Offer(net.HardwareAddr) (net.IP, error)
func (p *ipPool) Commit(net.HardwareAddr, net.IP) (net.IP, error)
func (p *ipPool) Release(net.HardwareAddr, net.IP) bool
func (p *ipPool) UpdateRanges([]Range) error
```

The pool has `now func() time.Time`, initialized to time.Now; tests replace it. Constants are `leaseDuration = 24*time.Hour` (same as the advertised existing lease) and `offerDuration = 30*time.Second`. Keep offers and active leases distinct; expiry is checked under the existing mutex before allocation/commit/release. Allocate is a compatible Offer+Commit convenience, not the DHCPDISCOVER implementation.

- [ ] **Step 1: Write expiry/ownership RED tests.** Use one available address so exhaustion/reuse is observable:

```go
func TestExpiredLeaseCanBeReused(t *testing.T) {
    _, subnet, _ := net.ParseCIDR("10.9.4.0/24")
    p := newIPPool(subnet, net.ParseIP("10.9.4.1"), []Range{{Start:2, End:2}})
    now := time.Unix(1000, 0)
    p.now = func() time.Time { return now }
    a := net.HardwareAddr{0,0,0,0,0,1}
    b := net.HardwareAddr{0,0,0,0,0,2}
    ip, err := p.Allocate(a)
    if err != nil { t.Fatal(err) }
    if _, err := p.Allocate(b); err == nil { t.Fatal("live lease reused") }
    now = now.Add(leaseDuration + time.Second)
    other, err := p.Allocate(b)
    if err != nil || !other.Equal(ip) { t.Fatalf("reuse = %v, %v", other, err) }
}
```

Add tests where wrong MAC RELEASE cannot free a lease, correct release makes it reusable, renewal extends expiry, abandoned offers expire after 30 s, reserved gateway/network/broadcast addresses never allocate, and concurrent requests never share an address.
- [ ] **Step 2: Run RED.** `go test ./pkg/dhcp -run 'TestExpiredLease|TestLeaseRelease|TestOfferExpiry|TestConcurrentLeases'`.
- [ ] **Step 3: Implement typed lease state.** Store address, owner MAC, expiration and offered/committed state. Purge expired records before searches. Commit requires the same owner and requested address, or a valid free address for INIT-REBOOT; it never takes an address from another active lease. UpdateRanges validates/copies ranges and retains active reservations outside the new range until original expiration without extending them. Keep maps bounded by the /24 and validate nil/empty hardware identifiers.
- [ ] **Step 4: Run race and legacy tests.** `go test -race ./pkg/dhcp`. Preserve existing configured-range and sticky-lease tests. Commit `fix(dhcp): expire leases and reclaim released addresses`.

## Task 2: Correct DHCP message processing

**Files:** create `pkg/dhcp/handler.go`, `handler_test.go`; modify `dhcp.go` to call the pure handler. Keep current server4 transport and interface binding.

**Interfaces:**

```go
func replyForMessage(cfg Config, pool *ipPool,
    msg *dhcpv4.DHCPv4) (*dhcpv4.DHCPv4, error)
```

Return nil,nil when another server was selected or when the message needs no reply. Config stays the existing structure. Handler error is logged/returned to diagnostics without terminating the serving socket. Confirm exact option getter/setter names from the pinned DHCP library before editing; do not guess its API.

- [ ] **Step 1: Add table-driven RED cases.** Include DISCOVER→OFFER, selected-server REQUEST→ACK, other-server REQUEST→no reply/no commit, INIT-REBOOT requested-address validation, RENEW/REBIND with ciaddr, correct RELEASE→no reply/free address, wrong-owner RELEASE→no effect, DECLINE→quarantine rather than an immediately reusable collision, INFORM→configuration ACK without a new lease, unknown type→no allocation, malformed server/identity/address options→no allocation.

The test ownership assertion uses the pool interface from Task 1:

```go
leased, err := pool.Allocate(ownerMAC)
if err != nil { t.Fatal(err) }
if pool.Release(otherMAC, leased) { t.Fatal("other client released this lease") }
if _, err := pool.Allocate(otherMAC); err == nil { t.Fatal("occupied address was reused") }
```

Construct messages with the pinned `dhcpv4.NewDiscovery`/modifiers after confirming their signatures; test both broadcast and unicast destinations. The reference for state distinctions is [RFC 2131 sections 4.3 and 4.4](https://www.rfc-editor.org/rfc/rfc2131.html).
- [ ] **Step 2: Run RED.** `go test ./pkg/dhcp -run 'TestReplyForMessage|TestRequestOtherServer|TestInformDoesNotAllocate'`.
- [ ] **Step 3: Implement the message switch before allocation.** DISCOVER calls Offer; REQUEST verifies option 54, ciaddr/requested-IP and ownership before Commit/renewal; RELEASE verifies this server and current owner before Release. DECLINE validates ownership and quarantines the conflicting address for a bounded 10-minute interval; it does not allocate a new address. INFORM reads cfg only. Keep broadcast-capable 0.0.0.0:67 socket and per-interface SO_BINDTODEVICE behavior. Advertise leaseDuration consistently with the pool clock.
- [ ] **Step 4: Run packet-level tests.** Decode generated bytes back into DHCP messages and assert server identifier, router, lease lifetime, ranges and optional DNS. Keep VPN DNS suppression through labdhcp.Settings. `go test -race ./pkg/dhcp ./internal/labdhcp`; commit `fix(dhcp): validate request release and renewal state`.

## Task 3: Reconfigure and stop DHCP without stale state

**Files:** create `pkg/dhcp/server.go`, `server_test.go`; modify `dhcp.go` while retaining public `NewManager()`, `Start(name, Config) error`, `Stop(name)` signatures. Both existing VPN/gateway callers remain valid.

**Interfaces:**

```go
type serverRunner interface { Serve() error; Close() error }
type StateChange struct { Name string; Running bool; Err error }
func (m *Manager) Healthy(name string) bool
func (m *Manager) Changes() <-chan StateChange
```

Inject a `newServer` factory in the package-private Manager for tests. Config is normalized/canonicalized before comparison. Each active entry has a generation and a lease pool; the handler reads a synchronized current config snapshot.

- [ ] **Step 1: Add lifecycle RED tests using a deterministic fake.**

```go
type fakeServer struct { done chan error; closeOnce sync.Once; closed atomic.Bool }
func (s *fakeServer) Serve() error { return <-s.done }
func (s *fakeServer) Close() error {
    s.closeOnce.Do(func() { s.closed.Store(true); close(s.done) })
    return nil
}
```

Factory records starts. Identical Start calls create one server. DNS/range changes on unchanged bindings update the handler/pool without losing a live lease. Stop synchronously closes the socket; repeated Stop succeeds. Unexpected Serve exit makes Healthy false and emits a named change. Releasing an old fake's Serve after a new generation starts must not mark the new server unhealthy.
- [ ] **Step 2: Run RED.** `go test ./pkg/dhcp -run 'TestManagerSameConfig|TestManagerReconfigure|TestManagerServeFailure|TestManagerOldGeneration'`.
- [ ] **Step 3: Implement state/config ownership.** Preserve lease state for updates on the same network. For changed interface/subnet bindings, validate the new config before stopping the old socket and ensure the old socket is closed before opening the replacement. A failed replacement is unhealthy and reported; do not return nil with stale configuration. Emit state changes only on transitions, coalesce pending changes by lab name and deliver current state so readiness cannot remain stale if changes are rapid. Stop closes socket/goroutines; dormant active leases are retained until expiration for disable/enable of the same lab/network, and are removed when the lab is deleted.
- [ ] **Step 4: Run race/lifecycle tests and commit.** Validate a RELEASE and renew processed during DNS/range update; two lab servers remain isolated; no retained goroutine after Stop. `go test -race ./pkg/dhcp`; commit `fix(dhcp): apply configuration changes and report server health`.

## Task 4: Gateway/VPN DHCP watches, permissions and unchanged-state handling

**Files:** modify `internal/gateway/reconciler.go`, `internal/vpn/reconciler/lab.go`, `internal/cmds/gateway/gateway.go`, `internal/vpn/reconciler/setup.go`, `charts/laboratory/templates/operator/clusterrole-gateway.yaml`; create `internal/gateway/reconciler_test.go`, `dhcp_events.go`/tests in each relevant controller package. Add a role contract test under `internal/controller/laboratory`.

**Interfaces:** produce `gatewayDHCPInputsChanged(old,next *v1alpha1.Lab) bool`, analogous VPN helper, and mapping functions from Lab/Pool to namespaced LabGateway/LabVPN requests. DHCP failure events are driven by Manager.Changes; controller-runtime watches those events and queries Healthy before writing Ready.

- [ ] **Step 1: Write RED integration cases.** Start from a fake Kubernetes client with an allocated LabGateway and configured Lab. Verify a change in Lab.Spec.Internet.DHCPServer requeues the correct gateway; VPN DHCP changes requeue only its VPN lab service. Pool create/delete changes affect the corresponding network. A forbidden/read-failed Lab or Pool returns an error and Ready=false, while a genuinely missing optional pool closes DHCP normally. Unknown pool naming/owner references do not select another lab.

For the actual gateway role, render the Helm chart and assert:

```text
apiGroups: [laboratory.cybericebox.com]
resources: [labs]
verbs: [get, list, watch]
```

The binding remains namespaced and targets the actual gateway service account; no cluster-wide binding is added.
- [ ] **Step 2: Run RED tests on Linux and the role contract.** `go test ./internal/controller/laboratory -run 'TestGatewayRoleReadsLabs'`; execute Linux controller tests with envtest/fakes through the local runner. Missing permission/read errors must be the observed reason, not a generic timeout.
- [ ] **Step 3: Implement reliable watches and health.** Add the narrowly scoped Labs read verbs to the existing gateway ClusterRole used by a namespaced RoleBinding. Watch only relevant Lab DHCP changes and Pool changes. Split pool lookup into `(bool,error)` so only apierrors.IsNotFound is false,nil. On disabled DHCP call both DenyDHCP and Manager.Stop; on deletion drop retained lease state. Map Manager changes into controller events and set DHCPReady from actual health. Use existing Configuring phase and Ready condition reason `DHCPFailed` when the socket is unavailable; keep NATReady accurate independently.

Cache per-lab applied network parameters. An identical reconcile skips address/NAT/filter reprogramming; startup always establishes the actual state. Remove old NAT/anti-spoof/address settings when the binding changes before advertising new Ready. No cache prevents retries after failed configuration or recognition of a replaced interface. Status patches are emitted only when the represented state changed.
- [ ] **Step 4: Prove real DHCP and gateway boundaries.** Extend `hack/test-netns.sh` to include Linux DHCP tests in pkg/dhcp and controller tests. Run DISCOVER/REQUEST/RENEW/RELEASE with real UDP sockets in two isolated lab namespaces; validate DNS/range changes, offer/lease expiry, disable/enable, deletion and unexpected socket failure. Re-run gateway NAT, private-destination deny, anti-spoof, INPUT and IPv6 tests from `internal/gateway/netns_linux_test.go`.
- [ ] **Step 5: Complete checks and record measured scope.** Run `go test -race ./pkg/dhcp ./internal/labdhcp`, controller/envtest tests, `make test-netns`, Helm lint/render and `go vet` on changed packages. Regenerate only required outputs and inspect diff. Record outcomes in `docs/superpowers/reports/2026-10-07-gateway-dhcp-validation.md`; no throughput maximum is claimed from idle tests. Commit `fix(gateway): reconcile DHCP configuration with scoped permissions`.

## Final integration and resource validation

- [ ] Run both plans' kernel tests together. Their shared DHCP implementation must work for VPN and Internet networks with correct DNS behavior and independent pools. Proxy tests must continue to pass without proxy implementation changes.
- [ ] Verify API/protobuf generated diffs, permissions, namespace ownership and stats reset/coverage semantics against the approved spec.
- [ ] Repeat local management-load checks for 1/10/20 lab interfaces, both DHCP modes, simultaneous configuration updates and unchanged reconciles. Report actual CPU/RAM/command counts with the architecture named. This does not establish NAT throughput.
- [ ] Before any external comparison, describe equivalent x86 resources and the workload (packets/s, concurrent connections, new connections/s, DHCP burst). Use full node CPU/softirq/conntrack metrics alongside container measurements. Preserve tables and raw data, delete all created experiment infrastructure and independently verify teardown afterward.
- [ ] Finish with a whole-branch review, validation record and explicit remaining limitations. Do not push or deploy. Stop before beginning proxy optimization.
