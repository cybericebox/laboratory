# Implementation decisions — 2026-10-07

Owner approved native implementation in fix/envtest-crd-warmup. No merge, push, release or proxy optimization.

## 2026-10-07-vpn-optimization

- Ruling: work in the existing clean branch without another worktree — explicit owner selection overrides the worktree default — cost if wrong: task commits share this branch.

- Ruling: execute 1,2,4,3,5,6 and prepare the Task 6 disposable runner with Task 1 — model declarations precede kernel code and Linux tests must run before claiming completion — cost if wrong: one task ordering change.

- Ruling: use an immutable source snapshot copied into an owned disposable Linux container, rather than changing Docker sharing — environment failure is outside product code — cost if wrong: test runner preparation and cleanup.

- Ruling: fix and verify the real counter-comment parser before the unchanged-reconcile regression — this is a production accounting defect, not a fixture error — cost if wrong: small parser change with a real-kernel regression.

- Ruling: accessApplier includes AccessCounters as well as replacement, and accessRevoker is an interface — the real reconciler can test failure/retry behavior at the kernel boundary — cost if wrong: a small internal interface change, no external contract.

- Ruling: prepare copied-source test runner now, including no-xattrs tar on macOS — host bind and provenance xattrs failed before tests — cost if wrong: local test tooling change, no production network change.

- Ruling: extend revocation with an optional explicit known-client list while retaining the legacy call shape — distinguish VPN clients from pod traffic and keep both directions — cost if wrong: internal interface compatibility adaptation.

- Ruling: private raw kernel checkpoint counters are decimal strings in CRD and uint64 internally — preserve all uint64 values through Kubernetes/JSON instead of unsafe signed/float numeric conversion — cost if wrong: private checkpoint serialization detail, public totals unchanged.

- Ruling: preserve lab interface identity in conntrack mark bits 8-23 alongside counted bit 31 — routed source addresses cannot identify their lab for revocation by themselves — cost if wrong: reserved private-netns mark bits, verified in kernel tests.

- Ruling: new namespace fixtures explicitly delete their root-side veth interfaces at cleanup — full-suite run found a retained wg0 although the isolated test passed — cost if wrong: test fixture cleanup only. Owned native meter chains are cleaned with the VPN filter.

- Ruling: retirement needs two kernel commits — detach the old gate first, then read/quiescent counters and persist before chain cleanup; a pre-commit snapshot can lose accepted packets — cost if wrong: an extra cleanup batch only on removed bindings, no work on unchanged rules. Failed retirement retains detached counters and retries; forwarding is already closed.

- Ruling: coverage ends at the last successful counter read, not the last attempted API publication — avoid a false heartbeat across read failures — cost if wrong: report timestamp semantics becomes more precise.

- Ruling: control proof is split across a real 20×20 reconciler with recording access boundary and native RuleCommand/kernel tests — no production test-only constructor added merely to expose command injection — cost if wrong: two focused proofs instead of one composite test. Retirement command count is two batches per the Task 5 ruling.

## 2026-10-07-gateway-dhcp-optimization

- Ruling: work in the existing clean branch without another worktree — explicit owner selection overrides the worktree default — cost if wrong: task commits share this branch.

- Ruling: health notification overflow coalesces to a full named-object resync (Name empty), queried through Healthy — guarantees current-state delivery without a persistent dispatcher goroutine or lost final transition — cost if wrong: extra reconciles only during clustered health transitions. Synchronous Stop waits for old Serve completion before creating its replacement, so a late old generation cannot overwrite new state.

- Ruling: reuse the existing Helm role assertion helpers under test/chart, plus actual namespace access proof under internal/controller/laboratory — avoid duplicating chart parser infrastructure — cost if wrong: proof is split across render and live authorization tests.

- Ruling: shared labdhcp Desired/watch mapping drives both VPN and gateway instead of duplicating controllers' lookup logic — exact pool-name + Lab UID ownership and API failure semantics stay identical — cost if wrong: one shared helper package now imports the Linux controller event adapter.

- Ruling: register the reporter as a manager Runnable so shutdown waits for its final read/report before IPT cleanup — bare goroutine allowed cleanup to race final accounting — cost if wrong: manager shutdown includes a bounded report flush.

- Ruling: preserve the missing-Kind e2e failure as an explicit validation limitation, run the canonical make test gate plus real network namespace and envtest authorization tests — the repository itself separates test-e2e from test — cost if wrong: full installed-cluster scaffold smoke test remains unverified.

- Final: Ruling: pre-existing envtest warmup, proxy optimization, stale-peer cleanup and startup ordering are outside this implementation range — preserve earlier behavior while fixing all introduced effectful paths — cost if wrong: those existing paths need their own work. AWS/x86 capacity is an explicitly unperformed experiment, and all test results are independently verified in the main session.

- Final: Ruling: atomically close the pre-existing empty startup gate as part of cold-identity correctness, superseding the earlier startup exclusion — a return-through interval could bypass accounting/permissions — cost if wrong: one atomic base-chain batch at startup instead of serial rules. Startup kernel regression RED→GREEN.

- Final: Ruling: keep the owner-selected branch and local commits without an integration menu — owner explicitly selected this branch and prohibited unsolicited push/PR/release — cost if wrong: integration remains the owner's next action.

## Deferred minors

None. All eight Important review findings were addressed in the one fix pass.
