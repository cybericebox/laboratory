# Laboratory lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop exactly the requested team's Lab generation after authoritative completion, preserve required snapshots/configuration, and report actual release without restarting solved or individually stopped Labs.

**Architecture:** Extend existing Lab/Device/Connection reconciliation and the existing snapshot engine. The agent writes UID/revision-fenced intent; operator and node-agent separately report capture, runtime/network teardown and retained storage. Existing group suspension stays unchanged; a separate full group lifecycle supports inter-stage service stop/start.

**Tech Stack:** Existing Go 1.27 module, protobuf/gRPC, Kubernetes CRDs/controller-runtime, containerd overlayfs, cgroup v2 freezer, native WireGuard/conntrack and OVS. No new framework or cloud resources.

**Spec:** `/Volumes/Projects/My/CyberICEBox/laboratory/docs/superpowers/specs/2026-10-08-lab-lifecycle-design.md`, approved with stage clarification at `f5c8bfb`. Exact producer proposal: `/Users/volodymyrporokhniak/.codex/outputs/2026-10-08-lab-lifecycle/laboratory-contract-proposal.md`.

## Global Constraints

- Keep one LabGroup per participation unit/team. Group sharing is not implemented.
- Acceptance means intent accepted, never actual stop completed.
- New `LabStatus` fields use fresh tags 41/42 for lifecycle and resources.
- Missing lifecycle intent means Running for existing Lab objects.
- Existing `LabGroup.spec.suspended` semantics are preserved: they currently retain VPN/gateway and must not silently become a full stop.
- A solved laboratory is terminal for participant runtime: nobody is expected to return to it, so a group resume, new stage or automatic deploy retry must never start it again.
- No required snapshot failure permits deletion or claims released capacity.
- Missing or stale observations mean Unknown, not zero usage or free capacity.
- Moderator forced per-team stop powers remain outside this work until separately decided.
- Closing a stage is a stop, not immediate deletion.
- Until meaningful lower-limit tests pass, keep existing supported presets and expose the new recommendation as unvalidated.
- This plan changes only Laboratory. Backend owns solve/stage/permission/outbox policy; it calls these interfaces after its own plan is integrated. Local commits require primary review; no push, PR or release.

## Review Focus

- A stopped intent with still-Ready status must revoke both directions immediately through desired-state predicates; snapshot capture must not leave old grants active.
- A restarted node-agent may thaw a previously acknowledged container; an old snapshot SUCCESS must fail the Pod UID/resourceVersion stop fence.
- An old terminating VPN/gateway pod must never clean the same-node replacement's OVS ports or acknowledge that replacement released.
- A required policy applied to an existing nonpersistent device must be rejected during preparation/acceptance, preserving runtime, rather than become an unexpected final-solve failure.
- Solved terminal state, a newer revision, missing node, duplicate create and group service restart must never resurrect a stopped copy or free a still-held allocation.

## Delivery and file map

Execute Tasks 1–4, the Task 6 ownership prerequisite, then Task 5 to produce the first working auto-stop producer slice. Integrate with backend/UI acceptance before describing auto-stop as complete. Task 6 fixes only the replacement ownership race exposed by stop/restart; it precedes every recovery/group claim. Task 7 supplies group/stage service lifecycle; Task 8 supplies retention/resource-profile gates and repeats native checks only after new relevant changes. Execution is gated on joint plan review.

| Area | Existing/new files and responsibility |
|---|---|
| Model/wire | Modify `api/laboratory/v1alpha1/lab_types.go`, `device_types.go`, `labgroup_types.go`; create `lifecycle_types.go`; modify `pkg/agent/protobuf/agent.proto`, generated `agent.pb.go`, `agent_grpc.pb.go`, public wrapper `pkg/agent/client/client.go`. |
| RPC/projection | Create `internal/agent/grpc/lifecycle.go`, `lifecycle_test.go`; modify `convert.go`, `features.go`, `lab.go`, `capacity.go`, `devicestate.go`. |
| Capture | Create `internal/devicestate/capture.go`, `capture_test.go`; modify `types.go`, `engine.go`, `kube.go`, `containerd_linux.go`, `cgroup.go`, `internal/nodeagent/state.go`. |
| Lab/device/queue | Create `internal/controller/laboratory/lab_lifecycle.go`, `lab_lifecycle_test.go`; modify `lab_controller.go`, `device_controller.go`, `devicestate.go`, `device_sched.go`, `sched_plan.go`, `scheduler.go`, `labstate.go`. |
| Access/fabric | Modify `internal/vpn/reconciler/input_changes.go`, `access.go`, `lab_bindings.go`, `lab.go`, `internal/gateway/reconciler.go`, `internal/nodeagent/grouppod.go`, `connection_reconciler.go`, `netattach_reconciler.go`, `internal/proxy/l7/access.go`, proxy cache/RBAC. |
| Actual resource report | Create `internal/nodeagent/lifecycle_observer.go`, `lifecycle_observer_test.go`; modify Device/Group status projections and controller aggregation. |
| Group/ownership | Modify `labgroup_controller.go`, `labgroup_sched.go`, `internal/nodeagent/ovsdb.go`, `netattach_reconciler.go`, `grouppod.go` and their focused tests. |
| Retention/features | Modify `internal/controller/laboratory/retention.go`, `internal/operator/config.go`, `internal/agent/grpc/features.go`, `internal/grouppods/sizing.go`, chart values/templates only when corresponding gates pass. |
| Generated/RBAC | Regenerate `api/laboratory/v1alpha1/zz_generated.deepcopy.go`, typed client/informer/lister output under `clientset/`, config+chart CRDs, `config/rbac/role.yaml`; update exact chart roles and node-agent admission below. |

## Task 1: Freeze additive model and public agent contract

**Interfaces:** Produce `LabLifecycleSpec`, `LabLifecycleStatus`, `RuntimeAllocation`, `DeviceCaptureRequest`, `DeviceCaptureResult`, `LifecycleFeature` and plural Stop/Start requests. Preserve every old field number, enum and legacy phase. Add `Lab.uid=13` and `LabGroup.uid=14` so existing objects without lifecycle status can be targeted safely. Add `FeaturesResponse.lifecycle=13`. Stop item has `terminal=4`; true is monotonic and StartLabs refuses it at every revision. Start never clears terminal.

- [ ] Add CRD types in native API files with validation for Running/Stopped, nonempty operation ID, revision>=1, explicit Skip/Required, bounded deadline, optional retention time. Define nil lifecycle as Running. Capture result includes exact operation/revision, Pod UID/resourceVersion, device epoch/incarnation, node-agent epoch, success/failure, saved image/time/bytes and guard state. Keep existing exit snapshot marker unchanged.

```go
type LabLifecycleSpec struct {
 DesiredState string `json:"desiredState"`
 OperationID string `json:"operationId"`
 Revision int64 `json:"revision"`
 SnapshotMode string `json:"snapshotMode,omitempty"`
 Terminal bool `json:"terminal,omitempty"`
 RetentionUntil *metav1.Time `json:"retentionUntil,omitempty"`
}
func (s *LabLifecycleSpec) IsStopped() bool {
 return s != nil && s.DesiredState == "Stopped"
}
```

- [ ] Add producer protobuf messages from the proposal; lifecycle/resources use LabStatus 41/42, group status 9/10. `StopLabsRequest`/`StartLabsRequest` use explicit repeated items, max 5000 and no bulk selector for this slice. `LifecycleFeature` advertises per-lab stop, required snapshot, confirmed runtime, retained restart and full group stop independently. Add new message aliases to public `pkg/agent/client/client.go`; embedded LabManagerClient supplies methods without duplicate connection logic.

```go
type StopLabsRequest = protobuf.StopLabsRequest
type StartLabsRequest = protobuf.StartLabsRequest
type LabLifecycleStatus = protobuf.LabLifecycleStatus
type ResourceAllocation = protobuf.ResourceAllocation
```

- [ ] Add `internal/agent/grpc/lifecycle_test.go` wire descriptor/old-field roundtrip tests and API nil-lifecycle tests. Verify stopped has false readiness/empty access while old running objects project identically. Wrong UID, equal conflicting revision, lower revision and terminal start are rejection cases, not success.

```go
func TestAbsentLifecycleRunsAndStoppedIntentDoesNot(t *testing.T) {
 var old *laboratoryv1alpha1.LabLifecycleSpec
 if old.IsStopped() { t.Fatal("legacy Lab unexpectedly stopped") }
 next := &laboratoryv1alpha1.LabLifecycleSpec{DesiredState:"Stopped",OperationID:"op",Revision:1}
 if !next.IsStopped() { t.Fatal("stop intent ignored") }
}
```

- [ ] Regenerate with installed pinned tools, never hand-edit generated fields:

```sh
protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative pkg/agent/protobuf/agent.proto
make generate generate-api manifests
go test ./internal/agent/grpc ./pkg/agent/client ./api/laboratory/v1alpha1 -run 'Test.*(Lifecycle|Compatibility|Projection)' -count=1
```

Expected initial failure: missing types/methods. Expected final: unchanged legacy projection, tags 41/42 present and generated config/chart schemas identical. Review generated diff and tool versions against current protobuf headers. Do not run install/deploy/push targets.

## Task 2: Explicit required capture and guarded quiescence

**Files:** Existing `internal/devicestate/*`, new `capture.go/capture_test.go`, `internal/nodeagent/state.go`; Device types from Task 1. **Interfaces:** Produce `Engine.CaptureRequired(ctx context.Context, p PodInfo, request DeviceCaptureRequest) (DeviceCaptureResult,error)`, `Cluster.SetCaptureGuard/RecordCapture/InvalidateCapture`, and a held quiescence handle owned by the existing tracked container. Required result is typed SUCCESS/FAILED, including unchanged base/latest image.

- [ ] Extend existing `newRig`/fakeRuntime/fakeCluster in `engine_test.go` with guard acknowledgement, held/thawed state and typed capture results. Test successful unchanged data, push/diff/quota/entries/missing runtime/timeout failure, cancellation, wrong incarnation and node-agent restart. Assert failures retain latest snapshot and do not emit successful exit/release.

```go
func TestRequiredCaptureQuotaNeverAcknowledgesSuccess(t *testing.T) {
 r := newRig(t, time.Hour, 1)
 r.rt.setDiff(tarOf(map[string]string{"work":"too large"}))
 req := DeviceCaptureRequest{OperationID:"op",Revision:1,PodUID:"pod-u",Epoch:0,Incarnation:1}
 result, err := r.e.CaptureRequired(context.Background(),r.pod,req)
 if err == nil || result.Result == "Succeeded" { t.Fatal("quota refusal allowed stop") }
 if result.Image != "" { t.Fatal("failed capture invented an image") }
}
```

- [ ] Use native freezer and filesystem sync. Keep it held through registry push and stop; do not change ordinary debounce snapshots or reuse `ExitSnapshotPod`. Refactor only the snapshot result path needed to distinguish valid unchanged, success, refusal and deferred capture. Enforce snapshot-policy file exclusions honestly; omitted required data is failure. Never describe filesystem layers as process-memory/network-state preservation.
- [ ] Store `laboratory.cybericebox.com/capture-guard` on the exact Pod; guard encodes operation/revision/podUID/epoch/incarnation/node-agent epoch. Before deleting, direct-read the CURRENT Pod and capture result, confirm matching held guard, then use that current Pod's UID and resourceVersion as delete preconditions. Unrelated kubelet status changes do not invalidate a still-held guard: refresh the Pod/RV and retry. The original capture RV is audit data, never an indefinitely required equality check.
- [ ] Before timeout/cancel/startup thaw, invalidate guard/result through the API so an in-flight deletion based on the older current RV conflicts. API failure preserves allocation and blocks stop; a bounded capture timeout records failure and invalidates before thaw, never silently permits deletion. Once deletion was accepted, NEVER thaw until actual task death, including crash/startup; account for grace/SIGKILL delay in stage lead time.
- [ ] Before `ThawOrphans` in `internal/nodeagent/state.go:98`, inspect this node's prior capture guards with direct API/runtime reads. Already-deleting required captures remain frozen until actual task death. Invalidate all other prior required guards before thaw and add boot epoch to fresh acknowledgements. Preserve old crash recovery for non-required live snapshots.
- [ ] Add only Pod patch privilege to `charts/laboratory/templates/node-agent/clusterrole.yaml`, constrained by a sibling admission rule in existing `node-agent/admission-policy.yaml`: bound token own-node equals Pod.spec.nodeName; only platform-owned Pod capture annotation changes; spec/status/labels/finalizers/owners unchanged. Keep existing Node-ready policy. Test allowed own-Pod guard and rejected another-node/spec/unrelated annotation writes in `test/chart/rbac_test.go` and `nodeagent_test.go`.

```sh
go test ./internal/devicestate ./internal/snapshot -run 'Test.*(RequiredCapture|CaptureGuard|Mapped|Quota|Exit)' -count=1
go test ./test/chart -run 'Test.*(NodeAgent|RBAC|Admission)' -count=1
```

Expected: required capture tests initially fail; after implementation ordinary snapshots still pass, every failure retains runtime and stale guard preconditions fail. Native frozen capture/stop/userns restore is required in Task 8 before supported capability is advertised.

## Task 3: Per-Lab stop, scheduling and immediate desired-state access closure

**Interfaces:** Produce `LabReconciler.reconcileLifecycle(ctx,*Lab) (handled bool,result ctrl.Result,err error)` and native `DeviceReconciler.deviceStopped(ctx,*Device) (bool,error)`. Every ordinary workload/network path consumes the parent Lab stopped intent, independently of group suspension. Required snapshot policy is validated at preparation/CreateLabs/StopLabs before intent acceptance; only persistence-enabled supported container devices enter the Required barrier. Switches have no writable snapshot and no phantom snapshot requirement.

- [ ] Add failing tests in `lab_lifecycle_test.go`, `devicestate_test.go`, `device_sched`/scheduler tests: two Labs in one group; stop one leaves sibling running; queued/not-yet-materialized and switch-only Labs stop; repeat reconcile, agent restart, group resume and retry never recreate stopped devices. Required unsupported device is rejected before changing intent; platform lacks freezer/registry capability is an explicit preparation error.

```go
// The existing fake client/deployment fixtures are used here, not a new lifecycle framework.
stopped := &lab.Lab{ObjectMeta:metav1.ObjectMeta{Name:"a",Namespace:"g"},
 Spec:lab.LabSpec{Lifecycle:&lab.LabLifecycleSpec{DesiredState:"Stopped",OperationID:"op",Revision:1}}}
sibling := &lab.Lab{ObjectMeta:metav1.ObjectMeta{Name:"b",Namespace:"g"}}
// Reconcile each twice; assert a's Deployment stays replicas=0 and b's remains replicas=1.
```

- [ ] Run lifecycle handling before `ensureWebServices/materializeDevices/materializeConnections`. Required capture waits for every matching SUCCESS; any failure records StopFailed and stops none of the original running devices. Skip mode scales Deployments/deletes bare Pods. Retain Device CR snapshot fields, Lab/Connection definitions, CIDRs, device codes, secrets and image digests. Device recovery must check parent stopped intent before current queued/recreate branches.
- [ ] In `internal/vpn/reconciler/input_changes.go:29` make `labAccessReady` false for desired Stopped even while status is Ready. Update `lab_bindings.go/access.go` snapshots/predicates so the new spec update revokes established client→lab and lab→client traffic before capture/deletion. Do not depend on delayed backend ACL dirty propagation. Await actual applied policy/network fence before beginning Required capture; record observed timestamps so no zero-latency claim is made.

```go
func labAccessReady(l *lab.Lab) bool {
 return l.DeletionTimestamp.IsZero() && !l.Spec.Lifecycle.IsStopped() &&
  l.Status.Phase == lab.PhaseReady && l.Status.VPN.Ready
}
```

- [ ] Add stopped checks to native per-lab VPN/gateway DHCP/routes and `GroupPodAttachments`, ConnectionReconciler and NetAttach inputs. Stop detaches only this Lab's runtime fabric; configuration remains. Add Lab watches to Device/Connection/network reconcilers so stopped intent itself wakes the correct owners. Scheduler stops dispatching stopped/pending-stop Labs; explicit nonterminal Start requeues owned runtime exactly once.
- [ ] Proxy `AccessReader.Allowed` in `internal/proxy/l7/access.go` must read the target Lab lifecycle as well as membership/policy, reject stopped/deleting/unknown access and retain that minimal field in `CompactCacheObject`. Add Labs get/list/watch to `charts/laboratory/templates/proxy/clusterrole.yaml` and manager cache setup in `internal/cmds/proxyl7/proxyl7.go`. Cached cookies/routes do not override desired stop; in-flight proxied sessions are closed through existing handler connection ownership or documented measured revocation boundary, never silently considered instant.

- [ ] Freeze actual ACL fence fields: additive `LabGroupAccessPolicy.operation_id=5`, `desired_revision=6`, `generation=7`, `policy_uid=8`, `expected_group_uid=9`; labels 10 unchanged. CRD spec carries operationID/revision. Add policy status `applied_revision=6`, `operation_id=7`, `vpn_boot_id=8`; AccessReconciler writes Applied only after both-direction conntrack retirement succeeds. Consumer confirms exact desired/applied revision+op, observed_generation==current policy generation and current GroupUID, not SetLabGroupAccess acceptance. For stopped desired intent independently of backend ACL lag, write exact-op `LabVPN.status.accessFence` after effective stopped rules/retirement; aggregate `LabLifecycleStatus.access_fenced=13`, `access_fenced_unix_ms=14`, `access_fence_vpn_boot_id=15`. Required capture waits this producer fence. Add tests that old Applied generation/revision/boot is not a fresh stop fence.

```sh
go test ./internal/controller/laboratory -run 'Test.*(Lifecycle|Stopped|SnapshotBarrier|Scheduling)' -count=1
go test ./internal/proxy/l7 -run 'Test.*(Access|Stopped|Cookie)' -count=1
go test ./internal/vpn/reconciler -run 'Test.*(Access|Stopped|Revoke)' -count=1
```

Run the final command inside the Linux fixture, not as a cross-compiled binary on macOS. The native gate must test both established directions and stale proxy credentials. A successful desired-intent update is not proof the asynchronous firewall has already converged.

## Task 4: Agent acceptance, UID/revision idempotence and projections

**Files:** New `internal/agent/grpc/lifecycle.go/lifecycle_test.go`; existing `lab.go`, `labgroup.go`, `convert.go`, `features.go`, `pkg/agent/client/client.go`. **Interfaces:** Exact plural StopLabs/StartLabs, StopLabGroups/StartLabGroups signatures from generated LabManagerServer. All responses are accepted per-item BatchResult; actual state comes only from matching List/Monitoring projections.

- [ ] Add RPC tests using existing `newTestHandler`, `readyGroup`, `specJSON`: identical duplicate accepted; conflicting equal revision, lower revision, wrong/recreated UID, another tenant, terminating Lab, 5001 targets, duplicate refs, required unsupported device and terminal start refused. Existing actor preparation tests ensure required policy cannot be attached to an unsupported existing deployment at final solve.

```go
func TestStopUsesObjectUIDAndIsIdempotent(t *testing.T) {
 h,k8s:=newTestHandler(t);ctx:=context.Background()
 readyGroup(t,h,k8s,"g","g",nil)
 created,err:=h.CreateLabs(ctx,&protobuf.CreateLabsRequest{
  Variants:[]*protobuf.LabVariant{{VariantId:"v",SpecJson:specJSON("web")}},
  Items:[]*protobuf.LabItem{{LabGroup:"g",Name:"l",VariantId:"v"}}})
 wantStates(t,created,err,stCreated)
 current,err:=h.cs.LaboratoryV1alpha1().Labs("g").Get(ctx,"l",metav1.GetOptions{});if err!=nil{t.Fatal(err)}
 target:=&protobuf.LabLifecycleTarget{Ref:&protobuf.ItemRef{LabGroup:"g",Name:"l"},ExpectedLabUid:string(current.UID),OperationId:"op",LifecycleRevision:1}
 req:=&protobuf.StopLabsRequest{Items:[]*protobuf.StopLabItem{{Target:target,SnapshotMode:protobuf.StopSnapshotMode_STOP_SNAPSHOT_MODE_SKIP,Terminal:true}}}
 for n:=0;n<2;n++ { got,err:=h.StopLabs(ctx,req);wantStates(t,got,err,stUpdated) }
 target.ExpectedLabUid="different";got,err:=h.StopLabs(ctx,req);wantStates(t,got,err,stFailed)
}
```

- [ ] Resolve group/tenant through existing namespace resolver and use RetryOnConflict over live CR resourceVersion. Reject terminal Start before any mutation. Treat identical desired intent as idempotent even after status advances. Never compare mutable lifecycle when determining CreateLabs immutable-spec identity; repeat create cannot reset stopped intent or secrets. Expose UID even for legacy Labs.
- [ ] Project lifecycle/resource observations identically in List and Monitoring; monitor continues omitting spec/env/private keys. Absence of observed fields is Unknown with nonzero held allocation, not Released. Add capabilities only when producer/node support is actually configured.

```sh
go test ./internal/agent/grpc ./pkg/agent/client -run 'Test.*(Lifecycle|Stop|Start|Monitoring|Tenant|LabsEndToEnd)' -count=1
```

Review public aliases/fields with backend implementer before consumer code. Joint-plan integration uses the approved backend-scoped checked-in protobuf/SDK snapshot generated from this exact producer contract; normal published builds still pin Laboratory v1.0.0. Verify that scoped snapshot build separately from local go.work. No separate publication approval is needed for this approved local integration; release/publication remains outside this task.

## Task 5: Confirmed runtime/resource observations and first-slice proof

**Files:** New native `internal/nodeagent/lifecycle_observer.go`; Device status, Lab/Group status aggregation, `capacity.go`, `convert.go`, `lab.go` admission tallies. **Interfaces:** Node-agent `ObserveOwnedRuntime(ctx,podUID,containerID,epoch,incarnation) RuntimeAllocation` reports present/terminating/absent/unknown plus OVS/CNI owner cleanup. Controller publishes Released only when every identity-matching process and attachment is confirmed absent. CPU/memory metrics remain optional and cannot replace release acknowledgement.

- [ ] Add pure aggregation tests and native observer fakes: terminating process, API force deletion with task alive, absent node, old Pod UID, cleanup error, new start generation. Every unknown/error retains held resource amounts. Existing stopped config consumes no active compute after release, while retained snapshot quota still counts.

```go
// Aggregation consumes the exact current operation, never an earlier release.
want:=lab.RuntimeAllocation{RuntimeState:"Released",OperationID:"new",Revision:2}
old:=lab.RuntimeAllocation{RuntimeState:"Released",OperationID:"old",Revision:1}
if allocationMatches(want,old) { t.Fatal("old release freed new runtime") }
```

- [ ] Sum configured requests/limits, actually allocated requests, measured usage, retained uncompressed snapshot quota and physically known registry bytes separately. Do not divide group VPN/GW overhead across Labs. Existing `overLimits` total retained-Lab cap stays total; introduce explicit active/pending compute tally for starts rather than silently treating every retained config as runtime.
- [ ] Node-agent patches only its own observation fields with optimistic status merge. Device status existing patch privilege suffices; Group runtime reports need group read/watch and group/status patch in node-agent role, own-Pod/node identity checks and admission protection for that report subfield. Keep operator phase/scheduling status ownership distinct. Source/task absence without a current node observation is Unknown.
- [ ] Before first-slice completion, run NATIVE capture→guarded stop→container/OVS release→userns writable-layer restore proof with actual containerd/freezer/registry and Task 6 ownership fence. Include unchanged capture, quota/push/guard failure, unrelated Pod RV change, accepted-deletion crash without thaw, old terminating+replacement Pod and forced API deletion with task alive. Existing host snapshots or the /lab memory point cannot substitute. Task 8 repeats this only when later changes invalidate its scope.
- [ ] Run first-slice end-to-end with backend worker: shared multi-question final solve, ACL withdrawal, accepted stop, required successful capture, actual teardown/credit, UI settled closure, sibling/team unchanged. Repeat after lost wake-up/process restart/outage and with capture failure. Do not advance to a broader coordinator completion claim until this passes.

```sh
go test ./internal/controller/laboratory ./internal/agent/grpc -run 'Test.*(Allocation|Capacity|Lifecycle|Limits|Monitoring)' -count=1
```

Evidence must include exact LabUID/operation/revision, capture pod fence, observed stop and released amounts. Existing host snapshot tests alone do not satisfy this task.

## Task 6: Replacement-owner cleanup fence prerequisite

**Files:** `internal/nodeagent/netattach_reconciler.go:136`, `grouppod.go:95`, `ovsdb.go`, `openflow.go`, focused group/netattach tests. **Interfaces:** `DelVethWithFlowsOwned(key string, ownerUID types.UID) error` and `GroupPortsPresentOwned(namespace,component string,podUID types.UID,owners map[string]types.UID) []string`. Keep existing stable port names; store exact owning Pod UID in OVS external IDs and verify it before cleanup/acknowledgement.

- [ ] Reproduce old terminating and replacement VPN/GW pods on the same node. The existing namespace/component sweep currently selects every group leg; a terminating old pod must not delete replacement ports or release its current allocation. Add a focused failing ownership test:

```go
func TestTerminatingOldGroupPodKeepsReplacementPorts(t *testing.T) {
 key:=names.VPNHostPortKey("g",1)
 owners:=map[string]types.UID{key:"replacement"}
 got:=GroupPortsPresentOwned("g",names.ComponentVPN,"old",owners)
 if len(got)!=0 { t.Fatal("old pod selected replacement port") }
 got=GroupPortsPresentOwned("g",names.ComponentVPN,"replacement",owners)
 if len(got)!=1 || got[0]!=key { t.Fatal("replacement lost its own port") }
}
```

- [ ] Narrow deletion and actual cleanup ACK to matching external owner UID/incarnation; remove port's t0 flow before port deletion as current code does. Legacy unowned ports may be cleaned only when no live replacement owns/needs the stable key; otherwise report Unknown and do not claim recovery/release.
- [ ] Native OVS test with old terminating owner, replacement owner, recycled ofport and controller/node-agent restart proves replacement remains connected. Do not expand into unrelated switch/OVS refactoring.

```sh
go test ./internal/nodeagent -run 'Test.*(Group|PortOwner|NetAttach)' -count=1
```

Run Linux/native OVS variants inside the owned Linux fixture. Failure leaves group restart/recovery support unverified and blocks corresponding capability claims.

## Task 7: Full group stop and stage service preparation

**Files:** `labgroup_types.go`, `labgroup_controller.go`, `labgroup_sched.go`, RPC from Task 4, group tests. **Interfaces:** separate Group lifecycle intent with `requireAllLabsStopped=true`; stopped group prevents creates/starts until explicit group Start. Start starts VPN/GW only and never changes a child's stopped/terminal state.

- [ ] Test legacy suspended keeps VPN/GW; full group stop refuses Running/Snapshotting/Stopping/StopFailed/Unknown/pending-start child; all released children allow service stop; repeated group start never restarts solved or manually stopped child. Same UID/revision rules apply to service pods and owned OVS cleanup.

```go
lg:=&lab.LabGroup{Spec:lab.LabGroupSpec{Suspended:true}}
// Existing suspended-only fixture must still produce VPN/GW replicas=1.
lg.Spec.Lifecycle=&lab.GroupLifecycleSpec{DesiredState:"Stopped",OperationID:"group-op",Revision:1,RequireAllLabsStopped:true}
// With any child Running, assert Group lifecycle reason WaitingForLabs and no service scale-down.
```

- [ ] After all matching children stopped/released, scale both service Deployments to zero, retain namespace/keys/client secrets and wait for actual service/attachment release. Group start requeues service scheduling and readiness. It never clears child stop intent.
- [ ] Backend stage coordinator sequences stop unresolved retained copies → wait actual stopped → full group stop; before next stage's deployment-lead window, group start → restore only unresolved explicitly needed Labs → wait real VPN/GW/snapshot/network readiness → publish by all_ready/as_ready. Laboratory supplies observations; no event/stage policy is duplicated here. Required capture/stop/restore and graceful termination times feed lead estimates.

```sh
go test ./internal/controller/laboratory ./internal/agent/grpc -run 'Test.*(GroupLifecycle|Suspended|Terminal|Scheduling)' -count=1
```

Stage controls and progressive manual permissions remain backend/UI tasks. Strict mode exposes no manual per-Lab stop/start. No moderator force-stop endpoint is added.

## Task 8: Retention, reduced profile and final Linux evidence

**Files:** `retention.go/retention_test.go`, resource projections, sizing feature/chart only after validation; repository docs `README.md`/`DEPLOY.md`. **Interfaces:** retention deadline comes from accepted per-Lab intent. Stopped Lab remains readable until expiry; retirement is explicit and manifest deletion never claims blob GC completion. Sizing v2 adds U/L/I/P/F and traffic envelope, preserving legacy fields and defaults until supported gates pass.

- [ ] Tests with controlled clock: retained stopped Lab no longer cancels its explicit expiry, no deletion before deadline, partial deletion retry, newer nonterminal start fences expiry, solved terminal copy never returns, and physicalStorageBytes remains unavailable until verified GC evidence. Keep old deleted-Lab retention compatible.
- [ ] Use actual native reduced-limit report under `/Users/volodymyrporokhniak/.codex/outputs/2026-10-08-lab-lifecycle/local-proof/`; preserve successful paced and failed burst attempts. Full /lab API/WG/conntrack/gateway process proof does not certify operator/CNI/OVS/snapshot restore or general capacity. Do not modify production defaults from a single point or Go-only allocation test. Publish candidate as unvalidated/limited-scope until matching native boundary tests and traffic/CPU criteria pass.

Current actual `/lab` point evidence: five native WG peers, 13 LabVPN networks, 65 applied ACL relations and 10 gateway/NAT networks against isolated Kubernetes 1.37 API. At limits 80Mi/50m and 32Mi/25m, the repeat's unpaced ramp returned 8,200/8,200 over about 1.76–1.79s per client; its separate 90s hold returned 22,489/22,489 at about 249.85pps aggregate. Combined 30,689/30,689 has zero errors. Retained flows transiently overlapped the previous attempt up to 16,374. VPN 52.301Mi is the same cgroup's lifetime peak across both attempts, not a reset paced-window peak. Gateway 180s at 1000pps returned 180,000/180,000 with zero errors; lifetime peak 14.066Mi. No OOM/max events. The initial 8200-flow refresh bursts lost 6,061 replies out of 295,200; CPU throttling occurred. Preserve that failure and label the result conditional on the tested paced/burst envelope. A single local point does not change supported presets or prove stage/lifecycle readiness.

- [ ] Freeze sizing feature at `GroupPodsFeature.sizing_v2=5`: `GroupPodsSizingV2.profiles=1`; profile fields id1/support_state2/max_inputs3/vpn4/gateway5/validation_provenance6/scope7. `GroupSizingInputs` fields max_users1/max_active_labs2/internet_labs3/allowed_relations4/envelope5. `GroupTrafficEnvelope` fields vpn_retained_flows1/gateway_retained_flows2/vpn_new_flows_per_second3/gateway_new_flows_per_second4/vpn_packets_per_second5/gateway_packets_per_second6/vpn_payload_mbps7/gateway_payload_mbps8. `GroupPodFormula` PodSize fields base1/per_user2/per_active_lab3/per_internet_lab4/per_allowed_relation5/per_retained_flow6/floor7/round_to8. Each CPU/memory dimension sums/rounds up; R/Q/B bound eligibility. Only SUPPORTED profiles select reduced sizes; missing/unbounded envelope or CANDIDATE/TESTED_POINT retains legacy formulas/presets. Current actual point is TESTED_POINT, not general supported profile.
- [ ] Build exact source images for owned Linux fixture and execute capture/stop/restore with real containerd, cgroup freezer, userns IDs, OVS and controller restart. Exercise guard conflict/error, node loss, both established access directions, stale proxy session, group replacement ownership and stage preparation. Save resource requests/limits, UID ownership, native memory.peak/OOM/CPU/flows, achieved rates and revocation timestamps.
- [ ] Required checks after changes, once focused tests pass:

```sh
go test ./internal/agent/grpc ./internal/controller/laboratory ./internal/devicestate ./internal/snapshot ./internal/grouppods ./internal/proxy/l7 ./pkg/agent/client ./test/chart -count=1
go vet ./internal/agent/grpc ./internal/controller/laboratory ./internal/devicestate ./internal/snapshot ./internal/grouppods ./internal/proxy/l7
make generate generate-api manifests
git diff --check
```

Run Linux-only packages and focused native integration in the owned Linux fixture; record host versus Linux scope separately. Verify generated files stable on a second generation run only if first generation changed them. Shared base images/cache volumes remain intact; clean only owned proof containers/tags/fixture after saving raw evidence.

- [ ] Self-review spec coverage: Task 1 wire/legacy; Task 2 snapshot barrier; Task 3 immediate access and no resurrection; Task 4 idempotent durable consumer seam; Task 5 confirmed capacity/first slice; Task 6 replacement fence; Task 7 full group/stage producer; Task 8 retention/sizing/native gates. Consumer solve/outbox/mode/admission and frontend settled states belong their separate domain plans and are integration gates, not claims of completed work here.
- [ ] Present exact producer/consumer test evidence to primary. Only after review, locally commit the tested cohesive slice with explicit files; no automatic commits, push, PR or release during plan preparation.
