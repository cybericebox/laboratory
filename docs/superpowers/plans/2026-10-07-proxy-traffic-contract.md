# Proxy and Traffic Contract Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans task-by-task. Steps use checkbox syntax. Owner authorized the whole sequence; execute natively without per-task approval. Laboratory is fully verified and frozen before backend, then frontend.

**Goal:** Correct and optimize the proxy, then carry truthful traffic/coverage facts through agent and AP Backend.

**Architecture:** Keep Go demux and L7 paths. One HTTP Meter holds cumulative namespace-indexed facts; reporting restores/merges its persistent state. Agent carries explicit coverage segments; backend stores lab-driven counts separately from participant activity.

**Tech Stack:** Go1.27, net/http, controller-runtime, WireGuard UDP, protobuf, PostgreSQL/sqlc, local Linux namespaces/Docker.

**Spec:** `docs/superpowers/specs/2026-10-07-proxy-optimization-design.md` (approved).

## Global Constraints

- Laboratory branch fix/envtest-crd-warmup; daemon branch feat/proxy-traffic-contract, selected new branch in current directory.
- No UDP/ciphertext traffic ledger at proxy; no HTTP/3, payload logging, denied-rule analytics, push, PR, release, deployment or AWS creation.
- Preserve tenant/namespace access, cookies, default deny, quotas and WireGuard cryptography.
- Only local source/test/doc changes; preserve daemon's untracked AGENTS.md.
- Test first, native execution, one fresh whole-change reviewer after both repositories are complete.

## Review Focus

1. Reused receiver index or backend generation never redirects/deletes a new unrelated session.
2. Cancel/disconnect/late hijack/shutdown must preserve permitted access and close all tracked sockets.
3. Restore failure or cap exhaustion must not erase persisted totals, starve unrelated live namespaces silently, or claim complete coverage.
4. Empty selected reports and lab-only rows retain truthful coverage/counters without creating participant activity.
5. Unknown timestamps, replay, overflow, replica gaps and truncation never become false evidence of untouched labs.

## Task 1: Demux lifetime and shutdown

**Files:** internal/proxy/demux/conntrack.go, demux.go, conntrack_test.go, demux_test.go.

**Interfaces:** Produce shared session state, `RemoveGroup(uid string)`, compatible `AddPartial(ci uint32,client,backend Socket,group ...string) bool`. Mirror removal checks identity; stale lookup never refreshes expired state. Stop closes the socket and joins readers; Close exits closed reads. Header/type/reserved/length and MSG_TRUNC validation precede state mutation.

- [ ] **Step1: Add the named failing regression and boundary cases from Review Focus.**

```go
func TestExpiredIndexReusePreservesNewSession(t *testing.T) {
 c,now:=clockTrack(DefaultLimits());session(t,c,10,20,sock("127.0.0.1",1),sock("127.0.0.1",51820))
 *now=now.Add(conntrackTTL+time.Second);session(t,c,10,30,sock("127.0.0.1",2),sock("127.0.0.1",51821));c.Cleanup()
 if _,ok:=c.Expected(10);!ok {t.Fatal("new session deleted")}
}
```

Fixture helpers in the snippets are created in the same test file using the existing httptest/fake-client/store patterns; their assertions inspect real written reports or SQL rows, not callback counts.

- [ ] **Step2: Run RED.** Run `go test ./internal/proxy/demux` in its repository (backend uses the prepared task modfile). Expected: fail on the specific missing behavior before implementation.
- [ ] **Step3: Implement the Interfaces contract in the listed files.** Use namespace-indexed maps, one owner/lock for counters and publication, exact session identity and existing access predicates; keep exports/callers compatible as named above.
- [ ] **Step4: Run GREEN and race checks.** Run `go test -race ./internal/proxy/demux`. Expected: all regression and existing package cases pass; database integration must actually run.
- [ ] **Step5: Commit locally.** `fix(demux): preserve session identity and stop idle readers`. Record exact test commands/results in the ledger, then continue without another approval.

## Task 2: Relevant demux configuration

**Files:** internal/proxy/demux/table.go, table_test.go, input_changes.go; internal/cmds/proxywg/proxywg.go.

**Interfaces:** Produce `Table.OnChange func(string)` and bounded context resolver; skip identical public key/backend. Predicate compares UID, deletion, namespace, registered/public key and VPN disabled; ignore irrelevant status. Group deletion/key/backend change retires conntrack group. Resolver/read/cleanup tasks join on shutdown.

- [ ] **Step1: Add the named failing regression and boundary cases from Review Focus.**

```go
func TestUnchangedTableDoesNotResolveAgain(t *testing.T) {
 table:=NewTable();calls:=0;table.resolve=func(context.Context,string)(*net.UDPAddr,error){calls++;return &net.UDPAddr{IP:net.IPv4(127,0,0,1),Port:51820},nil}
 pub:=base64.StdEncoding.EncodeToString(make([]byte,32));_ = table.Update("g",pub,"vpn.example:51820");_ = table.Update("g",pub,"vpn.example:51820")
 if calls!=1 {t.Fatalf("resolves=%d",calls)}
}
```

Fixture helpers in the snippets are created in the same test file using the existing httptest/fake-client/store patterns; their assertions inspect real written reports or SQL rows, not callback counts.

- [ ] **Step2: Run RED.** Run `go test ./internal/proxy/demux` in its repository (backend uses the prepared task modfile). Expected: fail on the specific missing behavior before implementation.
- [ ] **Step3: Implement the Interfaces contract in the listed files.** Use namespace-indexed maps, one owner/lock for counters and publication, exact session identity and existing access predicates; keep exports/callers compatible as named above.
- [ ] **Step4: Run GREEN and race checks.** Run `go test -race ./internal/proxy/demux`. Expected: all regression and existing package cases pass; database integration must actually run.
- [ ] **Step5: Commit locally.** `perf(demux): ignore unchanged routing inputs`. Record exact test commands/results in the ledger, then continue without another approval.

## Task 3: Progressive namespace-indexed Meter

**Files:** internal/proxy/l7/meter.go, meter_test.go; new meter_progress_test.go.

**Interfaces:** Produce `Begin(namespace,client,lab string,start time.Time)*RequestMeter`, methods `AddIn(int64)`, `AddOut(int64)`, `Responded(time.Time)`, `Incomplete()`, `End()`; retain Record/Ledger compatibility. Namespace snapshots return ledger/truncated/partial. Saturating additions; per-namespace caps and reclaim only retired inactive rows. Resume adds persisted baseline once without overwriting current boot progress.

- [ ] **Step1: Add the named failing regression and boundary cases from Review Focus.**

```go
func TestAttemptIsVisibleBeforeCompletion(t *testing.T) {
 meter:=NewMeter("b",time.Unix(1,0));request:=meter.Begin("ns","p","lab",time.Unix(2,0));request.AddIn(5)
 rows,_:=meter.Ledger("ns");if len(rows)!=1 || rows[0].Attempts!=1 || rows[0].BytesIn!=5 {t.Fatal(rows)};request.End();request.End()
}
```

Fixture helpers in the snippets are created in the same test file using the existing httptest/fake-client/store patterns; their assertions inspect real written reports or SQL rows, not callback counts.

- [ ] **Step2: Run RED.** Run `go test ./internal/proxy/l7` in its repository (backend uses the prepared task modfile). Expected: fail on the specific missing behavior before implementation.
- [ ] **Step3: Implement the Interfaces contract in the listed files.** Use namespace-indexed maps, one owner/lock for counters and publication, exact session identity and existing access predicates; keep exports/callers compatible as named above.
- [ ] **Step4: Run GREEN and race checks.** Run `go test -race ./internal/proxy/l7`. Expected: all regression and existing package cases pass; database integration must actually run.
- [ ] **Step5: Commit locally.** `refactor(proxy): count permitted request progress by namespace`. Record exact test commands/results in the ledger, then continue without another approval.

## Task 4: HTTP and WebSocket byte scope

**Files:** internal/proxy/l7/handler.go, live.go; new websocket_meter.go and tests.

**Interfaces:** Begin only after route/auth/quota admission, finalize with defer even AbortHandler. ModifyResponse timestamps real upstream response. Own errors never add lab bytes. WebSocket framing-only incremental parser counts data/continuation payload, excludes headers/masks/control frames; compressed payload stays compressed; malformed/unsupported extension marks incomplete and forwarding stays unchanged. Preserve buffered hijack bytes and detect both directions.

- [ ] **Step1: Add the named failing regression and boundary cases from Review Focus.**

```go
func TestProxyErrorIsNotLabBytes(t *testing.T) {
 // Use the existing signed-cookie handler fixture against a closed upstream.
 h,meter:=failedUpstreamHandler(t);response:=httptest.NewRecorder();h.ServeHTTP(response,permittedRequest(t))
 rows,_:=meter.Ledger(GroupNamespace("g"));if response.Code!=502 || rows[0].BytesIn!=0 {t.Fatal(rows)}
}
```

Fixture helpers in the snippets are created in the same test file using the existing httptest/fake-client/store patterns; their assertions inspect real written reports or SQL rows, not callback counts.

- [ ] **Step2: Run RED.** Run `go test ./internal/proxy/l7` in its repository (backend uses the prepared task modfile). Expected: fail on the specific missing behavior before implementation.
- [ ] **Step3: Implement the Interfaces contract in the listed files.** Use namespace-indexed maps, one owner/lock for counters and publication, exact session identity and existing access predicates; keep exports/callers compatible as named above.
- [ ] **Step4: Run GREEN and race checks.** Run `go test -race ./internal/proxy/l7`. Expected: all regression and existing package cases pass; database integration must actually run.
- [ ] **Step5: Commit locally.** `fix(proxy): account active HTTP and WebSocket payload traffic`. Record exact test commands/results in the ledger, then continue without another approval.

## Task 5: Restore, compact access inputs and coordinated stop

**Files:** internal/proxy/l7/reporter.go, live.go, limits.go; new access.go; internal/cmds/proxyl7/proxyl7.go; related tests.

**Interfaces:** Produce per-namespace restored report state, reporter preparation before readiness/admission, serialized publication and failure retry. Constant-time quota counts and grouped access checks. One route/service read and compact cache transforms drop unused statuses. Prewarm required informer types, reject deleting groups/clients. `Handler.Shutdown(ctx)` stops admission, closes/waits tracked sockets; final report follows settled counters. Cleanup only absent namespaces after active requests end.

- [ ] **Step1: Add the named failing regression and boundary cases from Review Focus.**

```go
func TestSamePodRestartKeepsTotals(t *testing.T) {
 // Fake K8s report holds8; new Meter has1 live request before restore.
 writer,store:=restartFixture(t,8);writer.Meter.Record("ns","p","lab",time.Unix(3,0),true,1,0)
 if err:=writer.Publish(context.Background(),"ns",time.Unix(4,0));err!=nil {t.Fatal(err)}
 if got:=readReport(t,store).Status.Ledger[0].Attempts;got!=9 {t.Fatal(got)}
}
```

Fixture helpers in the snippets are created in the same test file using the existing httptest/fake-client/store patterns; their assertions inspect real written reports or SQL rows, not callback counts.

- [ ] **Step2: Run RED.** Run `go test ./internal/proxy/... ./internal/cmds/proxyl7` in its repository (backend uses the prepared task modfile). Expected: fail on the specific missing behavior before implementation.
- [ ] **Step3: Implement the Interfaces contract in the listed files.** Use namespace-indexed maps, one owner/lock for counters and publication, exact session identity and existing access predicates; keep exports/callers compatible as named above.
- [ ] **Step4: Run GREEN and race checks.** Run `go test -race ./internal/proxy/... ./internal/cmds/proxyl7`. Expected: all regression and existing package cases pass; database integration must actually run.
- [ ] **Step5: Commit locally.** `fix(proxy): restore counters and drain before final reporting`. Record exact test commands/results in the ledger, then continue without another approval.

## Task 6: Agent public coverage contract

**Files:** api/laboratory/v1alpha1/labtrafficreport_types.go; pkg/agent/protobuf/agent.proto; internal/agent/grpc/convert.go, traffic.go, monitoring_filter.go, tests; generated files.

**Interfaces:** Add repeated public coverage spans (from/to/partial), preserve scalar fields for old clients; additive field numbers checked against current proto. Emit one span per replica report, deduplicate source identity, saturate merged counters. Empty ledger preserves authorized group/selected-lab coverage, never leaks another tenant. Truncation marks completeness false. Private raw checkpoints never enter public protobuf.

- [ ] **Step1: Add the named failing regression and boundary cases from Review Focus.**

```go
func TestEmptyAuthorizedTrafficKeepsHeartbeat(t *testing.T) {
 f,_:=newSelectorFilter("","tenant");r:=&protobuf.TrafficReport{LabGroupName:"g",Namespace:"ns",Kind:"vpn",CoveredFromUnixMs:1,CoveredToUnixMs:2}
 if got:=filterAuthorizedEmpty(t,f,r);got==nil {t.Fatal("idle coverage lost")}
}
```

Fixture helpers in the snippets are created in the same test file using the existing httptest/fake-client/store patterns; their assertions inspect real written reports or SQL rows, not callback counts.

- [ ] **Step2: Run RED.** Run `go test ./internal/agent/grpc ./internal/proxy/l7` in its repository (backend uses the prepared task modfile). Expected: fail on the specific missing behavior before implementation.
- [ ] **Step3: Implement the Interfaces contract in the listed files.** Use namespace-indexed maps, one owner/lock for counters and publication, exact session identity and existing access predicates; keep exports/callers compatible as named above.
- [ ] **Step4: Run GREEN and race checks.** Run `go test -race ./internal/agent/grpc ./internal/proxy/l7`. Expected: all regression and existing package cases pass; database integration must actually run.
- [ ] **Step5: Commit locally.** `feat(agent): preserve traffic coverage and safe cumulative totals`. Record exact test commands/results in the ledger, then continue without another approval.

## Task 7: Laboratory verification, profiling, review and frozen contract

**Files:** hack/test-netns.sh; proxy/agent/backend tests; docs/specs/proxy.md; docs/superpowers/reports/2026-10-07-proxy-validation.md; raw output archive.

**Interfaces:** Run laboratory focused/race/full/native/Linux tests before any backend product edits. Real authenticated WG transport test is separately distinguished from synthetic demux routing; namespace runner owns all privileged resources. Profile the agreed group/client/connections/pps matrix without collecting user UDP analytics. One fresh whole-laboratory reviewer, one TDD fix pass, then freeze the complete laboratory contract in a local commit; archive full tables/raw results then independently remove only owned test resources. Keep local branches; no merge/push/release.

- [ ] **Step1: Add the named failing regression and boundary cases from Review Focus.**

```go
func BenchmarkNamespaceLedger(b *testing.B) {
 for _,groups:=range []int{1,10,50,200} {b.Run(strconv.Itoa(groups),func(b *testing.B){meter:=populatedMeter(groups,200);b.ReportAllocs();b.ResetTimer();for i:=0;i<b.N;i++ {meter.Ledger("ns0")}})}
}
```

Fixture helpers in the snippets are created in the same test file using the existing httptest/fake-client/store patterns; their assertions inspect real written reports or SQL rows, not callback counts.

- [ ] **Step2: Run RED.** Run `go test ./internal/proxy/... ./internal/agent/grpc` in its repository (backend uses the prepared task modfile). Expected: fail on the specific missing behavior before implementation.
- [ ] **Step3: Implement the Interfaces contract in the listed files.** Use namespace-indexed maps, one owner/lock for counters and publication, exact session identity and existing access predicates; keep exports/callers compatible as named above.
- [ ] **Step4: Run GREEN and race checks.** Run `go test -race ./internal/proxy/... ./internal/agent/grpc`. Expected: all regression and existing package cases pass; database integration must actually run.
- [ ] **Step5: Commit locally.** `test(proxy): verify lifecycle accounting and traffic contract`. Record exact test commands/results in the ledger, then continue without another approval.


## Task 8: Backend nullable action times and lab initiatives

**Files:** daemon/internal/model/labTraffic/traffic.go; monitoring/lab/traffic.go; repository/labTrafficRepo; migration0159, queries/lab_traffic.sql, generated sqlc/mocks; tests.

**Interfaces:** Consume new protobuf through a task-specific modfile replace, no push/pin to unavailable SHA. Add LabInitiatedAttempts and nullable participant first/last times; retain old time fields compatibility at model edges. Ingest lab-only packet rows; do not create participant activity timestamps. Store distinct directions in replay cache, reject negative facts, preserve min positive time and cumulative max. New migration reversible for current non-null legacy rows.

- [ ] **Step1: Add the named failing regression and boundary cases from Review Focus.**

```go
func TestLabOnlyTrafficIsStoredWithoutUserActivity(t *testing.T) {
 report:=labOnlyReport(t);store:=newTrafficStoreFake();ingest:=NewTrafficIngest(store)
 if err:=ingest.ApplyTraffic(context.Background(),[]*labpb.TrafficReport{report});err!=nil {t.Fatal(err)}
 if len(store.touches)!=1 || store.touches[0].Attempts!=0 || store.touches[0].LabInitiatedAttempts!=1 {t.Fatal(store.touches)}
}
```

Fixture helpers in the snippets are created in the same test file using the existing httptest/fake-client/store patterns; their assertions inspect real written reports or SQL rows, not callback counts.

- [ ] **Step2: Run RED.** Run `go test ./internal/model/labTraffic ./internal/monitoring/lab ./internal/delivery/repository/labTrafficRepo ./internal/delivery/repository/postgres` in its repository (backend uses the prepared task modfile). Expected: fail on the specific missing behavior before implementation.
- [ ] **Step3: Implement the Interfaces contract in the listed files.** Use namespace-indexed maps, one owner/lock for counters and publication, exact session identity and existing access predicates; keep exports/callers compatible as named above.
- [ ] **Step4: Run GREEN and race checks.** Run `go test -race ./internal/model/labTraffic ./internal/monitoring/lab ./internal/delivery/repository/labTrafficRepo ./internal/delivery/repository/postgres`. Expected: all regression and existing package cases pass; database integration must actually run.
- [ ] **Step5: Commit locally.** `feat(backend): retain lab-initiated traffic without user activity`. Record exact test commands/results in the ledger, then continue without another approval.

## Task 9: Backend coverage and analytics interpretation

**Files:** daemon/internal/monitoring/lab/traffic.go; model/labTraffic/traffic.go; repository/labTrafficRepo; analytics and tests; contract docs.

**Interfaces:** Consume explicit spans when supplied, fallback scalar for older senders; Partial/Truncated imply incomplete evidence. Missing response observation with actual packet/byte evidence cannot be classified as untouched. Lab-only traffic remains separate from participant access. Deduplicate replay, nullable times in queries, contract scope documents compressed application bytes/inner IP bytes, no ciphertext totals.

- [ ] **Step1: Add the named failing regression and boundary cases from Review Focus.**

```go
func TestTruncatedIdleReportCannotProveUntouched(t *testing.T) {
 sink:=newTrafficStoreFake();report:=emptyCoveredReport(t);report.Truncated=true
 _ = NewTrafficIngest(sink).ApplyTraffic(context.Background(),[]*labpb.TrafficReport{report})
 if len(sink.coverage)!=1 || !sink.coverage[0].Partial {t.Fatal(sink.coverage)}
}
```

Fixture helpers in the snippets are created in the same test file using the existing httptest/fake-client/store patterns; their assertions inspect real written reports or SQL rows, not callback counts.

- [ ] **Step2: Run RED.** Run `go test ./internal/model/labTraffic ./internal/monitoring/lab ./internal/useCase/eventAnalytics` in its repository (backend uses the prepared task modfile). Expected: fail on the specific missing behavior before implementation.
- [ ] **Step3: Implement the Interfaces contract in the listed files.** Use namespace-indexed maps, one owner/lock for counters and publication, exact session identity and existing access predicates; keep exports/callers compatible as named above.
- [ ] **Step4: Run GREEN and race checks.** Run `go test -race ./internal/model/labTraffic ./internal/monitoring/lab ./internal/useCase/eventAnalytics`. Expected: all regression and existing package cases pass; database integration must actually run.
- [ ] **Step5: Commit locally.** `fix(backend): preserve incomplete traffic evidence in analytics`. Record exact test commands/results in the ledger, then continue without another approval.


## Task 10: Frontend contract consumption and final integration

**Files:** the actual analytics/API client and its uk/en UI consumers located from backend DTO references; select that independent repository branch before edits.

**Interfaces:** consume the already-frozen backend DTO/schema. Distinguish participant attempts, lab-initiated flows, unknown/incomplete coverage and unavailable byte scopes. No fabricated action dates or zero-to-unknown conflation. All UI labels use existing uk/en i18n.

- [ ] Step1: Trace the changed backend DTO to its client and write a failing fixture test for lab-only and incomplete rows. Expected: existing decoder/view loses or mislabels the new fields.
- [ ] Step2: Update the smallest responsible API mapping and UI, preserving the current layout and shared loaders/empty/error components.
- [ ] Step3: Run client type/build/i18n and focused rendering/contract gates; run real database backend checks and end-to-end data fixture from the frozen lab contract. Expected: new facts display correctly, old-client defaults remain safe.
- [ ] Step4: Commit locally, record full checks/results, and preserve raw evidence plus owned-resource teardown. No push/PR/release/deployment.
