# VPN Optimization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Preserve the execution method selected by the owner.

**Goal:** Убрать пересборку ACL от метрик, обеспечить симметричные разрешения и достоверные накопительные счётчики разрешённого трафика и инициатора.

**Architecture:** Существующий VPN-процесс остаётся владельцем фильтра. iptables получает один пакет изменений для принадлежащих VPN цепочек. Накопительные счётчики ядра читаются существующим flowacct/reporter; он остаётся единственным владельцем статистики и доставки LabTrafficReport.

**Tech Stack:** Go, controller-runtime, iptables/iptables-restore, conntrack, Kubernetes CRD, protobuf, Docker Linux network namespaces.

**Spec:** `docs/superpowers/specs/2026-10-07-vpn-gateway-dhcp-design.md`, разделы «Разрешения и обновление фильтра» и «Статистика разрешённой пары».

## Global Constraints

- Рабочая ветка, выбранная владельцем: `fix/envtest-crd-warmup`.
- Прокси остаётся следующим этапом.
- Статистика WireGuard по клиенту сохраняется.
- Статистика срабатываний запрещающих правил не нужна.
- Разрешённые пары задаются политикой. Остальной доступ запрещён по умолчанию.
- Первый этап сохраняет расположение фильтра в VPN и существующий iptables backend.
- Статистика подтверждает пересылку пакетов VPN-сервером. Она не доказывает, что приложение на устройстве получателя приняло пакет или что пользователь успешно решил задание.
- push, PR и релиз не входят в разрешённую работу.
- Новый AWS-стенд сейчас не создаётся.

## Review Focus

1. Публичный ключ/назначенный IP меняется через Spec/Status: обновление peer/ACL должно произойти, хотя обновление статистики игнорируется — Task 1.
2. Лаборатория отправляет пакет с адресом другой лаборатории: разрешение определяется реальным входным интерфейсом — Tasks 2–3.
3. Старый поток переживает отзыв разрешения или переиспользование адреса: оба направления закрываются, счётчики не переходят новому владельцу — Tasks 3–5.
4. Первый пакет повторяется либо поток заканчивается до опроса: одна инициатива и все разрешённые пакеты сохраняются в ядре — Task 3.
5. Процесс перезапускается при сохранённом netns либо новом netns: сохранение/сброс различаются, повторного сложения старых счётчиков нет, потеря интервала видна — Task 5.

## File structure and ownership

| Unit | Files | Responsibility |
|---|---|---|
| Events | `internal/vpn/reconciler/input_changes.go`, `client.go`, `access.go` | Relevant input predicates, one group reconcile key, applied-state comparison |
| Policy compiler | `internal/vpn/access_policy.go`, new `forward_plan.go`, `revoke.go` | Effective permissions, lab-interface binding, stable binding identity |
| Kernel filter | `internal/vpn/iptables.go`, new `forward_batch_linux.go`, `kernel_counters_linux.go` | Atomic owned-chain update, symmetric gate, counter preservation/readout |
| Traffic model | `internal/vpn/flowacct/collector.go`, new `counters.go` | Fold one authoritative cumulative source, epoch checkpoints, no duplicate packet journal |
| Delivery | `flowacct/reporter.go`, `reconciler/flowacct.go`, `reconciler/setup.go` | Resume, publish, compatible access-policy statistics, remove RunAccessStats loop |
| Contract | `api/laboratory/v1alpha1/labtrafficreport_types.go`, `pkg/agent/protobuf/agent.proto`, generated files, `internal/agent/grpc/convert.go` | Additive lab-origin field and private kernel checkpoints |

## Task 1: Stop configuration work triggered by statistics

**Files:** create `internal/vpn/reconciler/input_changes.go` and `input_changes_test.go`; modify `client.go`, `access.go`; create `access_inputs_test.go`.

**Interfaces:** produce `clientPeerInputsChanged(old, next *v1alpha1.LabGroupClient) bool`, `clientAccessInputsChanged(old, next *v1alpha1.LabGroupClient) bool`, and `labAccessInputsChanged(old, next *v1alpha1.Lab) bool`. Creation/deletion are always relevant. DeletionTimestamp changes are relevant. Peer inputs include PublicKey and AssignedIP. Access inputs include AssignedIP; lab inputs include readiness/CIDR. Metadata-only updates and counter updates are irrelevant.

- [ ] **Step 1: Add input tests before changing event wiring.** Use stdlib testing and the existing Kubernetes types:

```go
func TestClientStatsDoNotChangeConfigurationInputs(t *testing.T) {
    before := &v1alpha1.LabGroupClient{}
    before.Status.AssignedIP = "10.8.0.2/32"
    after := before.DeepCopy()
    after.Status.Statistics.RxBytes = 100
    if clientPeerInputsChanged(before, after) || clientAccessInputsChanged(before, after) {
        t.Fatal("statistics triggered configuration")
    }
    after.Status.AssignedIP = "10.8.0.3/32"
    if !clientPeerInputsChanged(before, after) || !clientAccessInputsChanged(before, after) {
        t.Fatal("assigned-address update was lost")
    }
    after = before.DeepCopy()
    after.Spec.PublicKey = "new-key"
    if !clientPeerInputsChanged(before, after) {
        t.Fatal("key rotation was lost")
    }
}
```

Add table cases for create/delete, deletion timestamp, Lab readiness loss, CIDR replacement and an identical snapshot. Do not use GenerationChanged alone: it loses required allocation/readiness changes in Status.

- [ ] **Step 2: Run the failing package tests in Linux.** Build with `GOOS=linux go test -c ./internal/vpn/reconciler` for compilation; execute the package tests through the disposable Linux runner added in Task 6. Failure before helper implementation is expected.
- [ ] **Step 3: Implement predicates and use one namespace request key.** Apply this structure to client changes:

```go
clientChanges := predicate.Funcs{
    UpdateFunc: func(e event.UpdateEvent) bool {
        return clientAccessInputsChanged(
            e.ObjectOld.(*v1alpha1.LabGroupClient),
            e.ObjectNew.(*v1alpha1.LabGroupClient))
    },
    CreateFunc: func(event.CreateEvent) bool { return true },
    DeleteFunc: func(event.DeleteEvent) bool { return true },
}
```

Use `handler.EnqueueRequestsFromMapFunc` with the existing fixed `access-policy` namespace key for the client watch as well as lab/policy watches. Keep the peer controller's own client-name keys. Compare normalized desired/applied permissions before invoking firewall replacement; set the applied fingerprint only after successful kernel application. The first reconcile after process start must apply/verify regardless of persisted Policy status.
- [ ] **Step 4: Add a recording firewall boundary.** Deliver an `accessApplier` interface with `ReplaceAccessRules([]vpn.AccessRule) error`; test a real Reconcile against a fake Kubernetes client and recording applier. One hundred stats patches and unchanged snapshots produce no additional applications; one real permission/address change produces the new snapshot. A failed application must retry, never seed the successful-state cache.
- [ ] **Step 5: Run the focused tests and commit only this change.** `go test ./internal/vpn/flowacct ./internal/vpn` plus the Linux reconciler tests. Commit message: `perf(vpn): ignore telemetry changes when reconciling access`.

## Task 2: Compile allowed bindings and symmetric revocation

**Files:** modify `access_policy.go`, `revoke.go`, `reconciler/access.go`; create `forward_plan.go`, `forward_plan_test.go`; modify `revoke_test.go`. Keep `BuildAccessRules` as the compatible decision-matrix builder, including deny decisions for status, but never send its deny rows to the kernel.

**Interfaces:**

```go
type ForwardRule struct {
    ClientName, LabName string
    ClientCIDR, LabCIDR string
    LabInterface string
    BindingID string
}
type ForwardPlan struct {
    Allows []ForwardRule
    Decisions []AccessRule
}
// Extend LabAccessSnapshot with Interface string.
func CompileForwardPlan(clients []ClientAccessSnapshot,
    labs map[string]LabAccessSnapshot, policy []AccessPolicyRule) (ForwardPlan, error)
```

Resolve interfaces from LabVPN objects by `Spec.LabName` and `names.LabIfaceNameByIndex(Spec.NetworkIndex)`. Add a LabVPN watch for interface/index changes. `BindingID` is a deterministic hash of names, canonical addresses and actual lab interface; it changes when an address is reissued. An allow without a trustworthy interface mapping returns an error and is not installed.

- [ ] **Step 1: Add deny-precedence and 20 × 20 tests.** A concrete basic case is:

```go
func TestForwardPlanContainsOnlyAllowedPairs(t *testing.T) {
    clients := []ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.2/32"}}
    labs := map[string]LabAccessSnapshot{
        "a": {VPNCIDR: "10.8.1.0/24", Ready: true, Interface: "lab1"},
        "b": {VPNCIDR: "10.8.2.0/24", Ready: true, Interface: "lab2"},
    }
    plan, err := CompileForwardPlan(clients, labs,
        []AccessPolicyRule{{Action: AccessAllow, ClientNames: []string{"p1"}, LabNames: []string{"a"}}})
    if err != nil || len(plan.Allows) != 1 || len(plan.Decisions) != 2 {
        t.Fatalf("plan = %+v, %v", plan, err)
    }
    if plan.Allows[0].LabInterface != "lab1" { t.Fatal("wrong physical lab binding") }
}
```

Generate twenty clients/labs with twenty one-to-one allow assignments and assert twenty kernel bindings and four hundred decision rows. Add broad allow plus explicit deny, unknown interface, not-ready lab and reordered inputs. Changing assigned IP must change BindingID; changing peer statistics must not.
- [ ] **Step 2: Run RED tests.** `go test ./internal/vpn -run 'TestForwardPlan|TestRevokedFlows'` on Linux.
- [ ] **Step 3: Implement the pure compiler.** Compile with the existing `BuildAccessRules`, keep Decisions, select only `Action == AccessAllow`, parse prefixes using netip, validate the interface, compute BindingID, then sort by ClientName/LabName. Extend `RevokedFlows` to recognize both original directions. Add the current client-address set as an explicit input to revocation so unrelated pod traffic is not mistaken for client traffic. Preserve the rule that no policy enlarges an allowed destination beyond the existing VPN subnet semantics.
- [ ] **Step 4: Test both flow orientations and reassignment.** Use these tuples in the revocation table:

```go
forward := ConnFlow{ID: 1, Src: netip.MustParseAddr("10.8.0.2"), Dst: netip.MustParseAddr("10.8.1.2")}
reverse := ConnFlow{ID: 2, Src: netip.MustParseAddr("10.8.1.2"), Dst: netip.MustParseAddr("10.8.0.2")}
// Removing p1/a revokes both, but leaves a pod-to-API tuple outside these identities alone.
```

For routed/spoofable lab source addresses, use the established lab binding/conntrack metadata rather than trusting the source prefix to identify the lab. Include this in the namespace test of Task 3.
- [ ] **Step 5: Run compiler/revocation tests and commit.** Commit: `refactor(vpn): compile allowed lab bindings separately from policy status`.

## Task 3: Atomic symmetric filter and cumulative kernel counters

**Files:** modify `iptables.go`, `netns_linux_test.go`; create `forward_batch_linux.go`, `forward_batch_linux_test.go`, `kernel_counters_linux.go`, `forward_accounting_netns_linux_test.go`; adapt `reconciler/access.go` to the new applier.

**Interfaces:** consume ForwardPlan. Produce:

```go
type RuleCommand interface {
    Save(context.Context) ([]byte, error)
    Restore(context.Context, []byte) error
}
type ApplyResult struct { Changed bool }
func (m *IPTablesManager) ApplyForwardPlan(context.Context, ForwardPlan) (ApplyResult, error)
func (m *IPTablesManager) ReadPairCounters(context.Context) (flowacct.CounterSnapshot, error)
```

`CounterSnapshot` is defined in Task 4. It contains a generation for each binding and unsigned cumulative packets/bytes and client/lab initiation counts. Retired bindings are read one final time before owned counter chains are deleted. The applier offers `BeforeRetire func(flowacct.CounterSnapshot) error`; if the collector cannot accept the final snapshot, retirement stops with the old deny/allow-safe state intact.

- [ ] **Step 1: Add recording-command tests.** Define a test `recordingRuleCommand` implementing Save/Restore with `restores [][]byte`, `saved []byte`, `restoreErr error`. Assert one Restore on changed permissions, zero on identical input, and an error without a successful fingerprint on failed Restore. Insert an unrelated chain in saved input and assert it is untouched. Assert twenty assigned pairs never generate the other 380 DROP pairs.
- [ ] **Step 2: Add namespace RED tests using existing nstest helpers.** Extend the existing topology to two clients and two labs. Explicitly allow pa/lab1 only. Assert pa reaches lab1, lab1 reaches pa, lab1 cannot reach pb, lab2 cannot reach pa even with a spoofed lab1 source, client-to-client and lab-to-lab remain blocked. Existing INPUT/probe/DHCP/IPv6 tests stay enabled. Open TCP and UDP flows, remove the permission, and assert existing traffic stops in both original orientations.
- [ ] **Step 3: Implement batch application with a safe gate.** Put the wg/lab access gate before generic established forwarding. Each allowed pair matches the client address and the actual lab interface; the client-to-lab direction also retains the allowed destination CIDR. A terminal common DROP covers unmatched VPN/lab traffic. Remove the old unconditional lab-to-wg ACCEPT. Only VPN-owned dynamic chains are changed using `iptables-restore --noflush --counters`; preserve unchanged counter chains/counters and do not flush unrelated tables. Use exec.CommandContext argument arrays, not shell interpolation. Validate canonical prefixes and interface names before emitting restore text.

The counter rules count accepted packets in both directions. Initial-flow accounting uses conntrack original direction plus a reserved per-netns connmark bit `0x80000000`, set on the first accounted allowed original packet. Already-confirmed pre-existing flows must not become new initiatives after installation. A NEW match alone is insufficient. A representative first-flow branch is:

```text
-m conntrack --ctstate NEW --ctdir ORIGINAL ! --ctstatus CONFIRMED
-m connmark ! --mark 0x80000000/0x80000000
```

The final implementation emits a separate counting branch and sets the bit before returning to the accepted packet path. Reading the initiation-branch counter yields one attempt per new record. Counter rule correctness is decided by the real-kernel cases below, including parallel first packets; do not accept a renderer-only proof of uniqueness. Reserve this bit explicitly, leave every other connmark bit intact and preserve INPUT/WireGuard guard code.
- [ ] **Step 4: Prove counters on Linux.** Send a known number of UDP datagrams with a fixed payload in each direction; compare accepted packet counts and IP-layer byte counts after settling. Add established TCP transfer, lab-originated TCP, repeated SYN before reply, repeated UDP without reply, a flow created/deleted between sampler ticks, parallel first datagrams, and a flow already present before a rule update. Reply packets must not increment lab initiatives. If the kernel path fails the uniqueness cases, keep this task incomplete and correct the first-packet mechanism before exposing the metric.
- [ ] **Step 5: Prove no gaps and preservation.** Change one pair while transmitting on another; unchanged pair counters continue monotonically. Remove/re-add a binding and change its address: the epoch changes and old values remain with the old identity. Failed restore leaves no excess permission; startup without the first policy is closed. Run `make test-netns`, focused Linux tests and `go test ./internal/vpn/flowacct`; commit `perf(vpn): batch symmetric access rules and account accepted traffic`.

## Task 4: Add the compatible statistics contract

**Files:** modify `api/laboratory/v1alpha1/labtrafficreport_types.go`, `pkg/agent/protobuf/agent.proto`, `internal/agent/grpc/convert.go`, the Touch/Report models in `flowacct/collector.go` and model conversion/resume in `flowacct/reporter.go`; regenerate `agent.pb.go`, DeepCopy and the LabTrafficReport CRD in config/chart; modify `internal/agent/grpc/traffic_test.go`. Create `internal/vpn/flowacct/counters.go`, `counters_test.go`. This contract task runs before Task 3.

**Interfaces:**

```go
type PairCounters struct {
    Key
    BindingID, Epoch string
    PacketsOut, PacketsIn, BytesOut, BytesIn uint64
    Attempts, LabInitiatedAttempts uint64
}
type CounterSnapshot struct {
    Rows []PairCounters
    At time.Time
    Partial bool
}
type CounterReader interface {
    ReadPairCounters(context.Context) (CounterSnapshot, error)
}
// Pure delta helper, implemented in counters.go and used by Task 5.
func PairCounterDelta(current, previous PairCounters) (PairCounters, bool, error)
```

The CRD adds `LabInitiatedAttempts int64` with `json:"labInitiatedAttempts,omitempty"`. The public protobuf appends the following free field, retaining numbers 1–14:

```proto
int64 lab_initiated_attempts = 15;
```

Private `LabTrafficReportStatus.KernelCheckpoints` contains Subject/LabName, BindingID/Epoch and the six last raw counters. It contains no addresses. It is used for resume and is not relayed as a public traffic ledger. `Report.Partial` is added to the internal report and maps to the existing CR/protobuf Partial field.

- [ ] **Step 1: Add serialization RED tests.** A traffic row with Attempts=2, LabInitiatedAttempts=3, PacketsOut=300 and PacketsIn=200 must retain all values through CR status, ToStatus, protobuf conversion and marshal/unmarshal. An old serialized row with no field 15 decodes with zero lab initiatives. Proxy conversion keeps the optional new field zero.
- [ ] **Step 2: Add checkpoint/delta tests for PairCounterDelta.** With Key{Subject:"p1", Lab:"a"}, Epoch="e1", an identical second reading has all six deltas zero. A larger reading produces the differences. A new epoch produces its new counters and a discontinuity flag; different owner keys are an error. Task 5 folds these deltas into cumulative totals and clears an expected retirement discontinuity only when the final snapshot was acknowledged.
- [ ] **Step 3: Implement additive model/conversion fields.** Map the new field without reinterpreting Attempts, keep Out/In client-relative, and use uint64 for raw kernel counters with checked conversion/addition into public int64 totals. Saturation sets Partial instead of wrapping negative. Persist raw checkpoints in the same report write as cumulative totals.
- [ ] **Step 4: Regenerate and verify.** Use `protoc --go_out=. --go_opt=paths=source_relative pkg/agent/protobuf/agent.proto`; run `make generate manifests`, inspect generated diffs and keep only outputs related to this contract. Run `go test ./internal/agent/grpc ./internal/vpn/flowacct`, using the repository envtest assets when needed. Confirm consumers of the protobuf fields continue compiling. Commit `feat(vpn): expose lab-initiated traffic counters compatibly`.

## Task 5: Consolidate flow accounting and compatible policy status

**Files:** modify `flowacct/collector.go`, `collector_test.go`, `reporter.go`, `reporter_test.go`, `conntrack_linux.go`, `reconciler/flowacct.go`, `reconciler/setup.go`, `reconciler/access.go`.

**Interfaces:** extend the existing Collector with `ObserveCounters(CounterSnapshot) error`, restore checkpoint state in `Resume`, and inject the CounterReader into Reporter. Retain the existing Source.Dump for timestamps/reply metadata only; it is no longer authoritative for packet totals or initiation totals.

- [ ] **Step 1: Add RED collector cases.** Normalize raw reverse flows into the same client/lab Key. A lab-origin flow contributes to PacketsIn and LabInitiatedAttempts; it never creates a client Attempt or FirstRespondedMs. A client-origin flow's reply contributes PacketsIn and may set FirstRespondedMs. A short vanished flow still appears through cumulative kernel counters.
- [ ] **Step 2: Add restart/identity/privacy cases.** Resume the persisted total and raw checkpoint; read an unchanged retained-netns epoch and assert no doubled totals. A new epoch/new netns retains old totals but flags the lost coverage interval. Reissued IP creates a new BindingID; old traffic never moves to its new owner. Unknown kernel epochs, missing accounting, read failure, overflow and MaxRows=512 truncation are visible. No public IP, probe, DHCP or outer encrypted WireGuard tuple enters the report.
- [ ] **Step 3: Implement one owner of the counters.** On each sample read all native pair counters once, fold checked deltas, then optionally enrich existing client-origin timestamps from conntrack. Feed the same aggregate into allowed Policy.Status.Rules Packets/Bytes. Deny decisions remain decisions only. Remove `go RunAccessStats` from Setup and remove its independent counter-read loop. Keep RunStats for WireGuard. Reports continue on the existing minute cadence; reading counters never calls ApplyForwardPlan or revocation.

```go
snapshot, err := reporter.CounterReader.ReadPairCounters(ctx)
if err != nil { collector.MarkPartial(now); return err }
if err := collector.ObserveCounters(snapshot); err != nil { return err }
```

Deliver `Collector.MarkPartial(time.Time)` in this task; it preserves totals and starts a new coverage interval when reads recover. Client-initiative timestamps remain distinct from lab-only traffic. Publish new lab counts without making a lab-only row look like a student action. Report delivery is retryable and isolated from packet forwarding.
- [ ] **Step 4: Verify integration and retirement.** Exercise BeforeRetire through the real applier and collector, then remove a pair. Its final totals survive chain deletion and do not increment on later denied traffic. Concurrent sampling, policy changes and publish calls run under `go test -race` without duplicate ownership. Run Linux reconciliation tests, agent contract tests and `make test-netns`. Commit `refactor(vpn): publish one authoritative traffic ledger`.

## Task 6: Run the complete VPN gates and prepare the resource comparison

**Files:** modify `hack/test-netns.sh`; create `hack/test-vpn-control.sh`, `internal/vpn/reconciler/control_netns_linux_test.go`; update `docs/specs/vpn.md`; create `docs/superpowers/reports/2026-10-07-vpn-validation.md` during execution with actual results.

- [ ] **Step 1: Extend the disposable runner.** Add `vpn/reconciler` to compiled test packages so Linux-only controllers run too. Continue invoking each binary in `unshare -n` with `CICE_NETNS_TESTS=1`; the container is removed on exit. Install needed iptables/conntrack tools inside this container only. Give controller tests the required cached envtest assets without creating an external cluster.
- [ ] **Step 2: Run focused then whole relevant gates.** Use `make test-netns`; `go test -race ./internal/vpn/flowacct ./pkg/dhcp`; Linux `go test -race ./internal/vpn/...`; agent/envtest tests; `go vet` for modified packages; CRD and proto regeneration checks. Record actual pass/fail/skips. A skip of the kernel tests is an incomplete gate, not a pass.
- [ ] **Step 3: Add a control-load test.** Fake Kubernetes client with 20 peers and 20 labs; after initial configuration issue 100 statistics updates, then one permission change. RecordingRuleCommand must show zero additional Restore for stats and one relevant update for the permission. Run CPU/alloc benchmarks for 10/20 peers × 10/20 labs and unchanged-plan fingerprints. These are local control-path benchmarks, not estimates of AWS throughput.
- [ ] **Step 4: Prepare the repeat measurement protocol.** Reuse the preserved original 57-stage data as the baseline. Specify pinned images, CPU limits, architecture, sampling periods, real handshakes and counter-correctness controls. Linux ARM results establish functionality and local cost only. An equivalent x86 two-node measurement is required for a numerical comparison to the old c7i series. Describe its resources before any new external creation and preserve full tables/raw logs with verified teardown.
- [ ] **Step 5: Write the actual validation record and commit.** Include filter correctness, exact counting scope, no metric-triggered applications, protocol delivery, counter reset/coverage behavior and measured command-count change. Do not invent a new group capacity or resource minimum from these tests. Commit `test(vpn): verify isolation accounting and idle reconciliation`.

## Completion and handoff

Task order is 1 → 2 → 4 → 3 → 5 → 6: the model/contract exists before the kernel reader consumes it. Task 6's disposable Linux runner support may be prepared alongside Task 1 to execute Linux-only tests; the complete final gates remain after Task 5. Partial commits remain local. Final review must trace client identity, lab-interface identity, rule application, revocation, authoritative counters, report resume and gRPC delivery end to end. Then proceed to `2026-10-07-gateway-dhcp-optimization.md` with the same selected execution method. The owner approved implementation on 2026-10-07; execution is native in this chat.
