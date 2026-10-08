# Lab lifecycle and one-team resource sizing

Status: technical specification for owner review. The owner approved the product rules in conversation and requested parallel implementation. Product implementation starts after review of this contract and its implementation plan. No release or remote publication is part of this work.

## Product boundary

Keep one LabGroup per participation unit/team. Group sharing is not implemented. A laboratory is a separate lifecycle entity; several questions of one exercise may use the same team's laboratory. Academic limits such as three laboratories and event limits based on team size remain configurable policy values, not constants in the agent. This work extends existing products; it does not create an academy portal.

Participant-facing logical closure is separate from actual infrastructure state. On completion the participant sees the settled closed state, with scores and history retained. Internal snapshot/stop progress is available to the backend and operator, not a participant-facing shutting-down state. Actual capacity is credited only after identity-matching release confirmation.

## Delivery boundaries

1. First working delivery: additive per-Lab lifecycle and mandatory configured snapshot barrier; confirmed resource observations; durable backend auto-stop after every pinned objective of the shared laboratory is solved; immediate authoritative UI closure; conservative resource sizing validated against smaller limits.
2. Subsequent delivery: wire existing `TaskRevealMode` values `all_ready` and `as_ready` into stage publication/admission, progressive manual stop/restart, configurable active-lab limits and retention, full group stop at the end of its work. These are part of the requested model, not replacements for existing participation and stage semantics.

Each delivery receives its own implementation tasks and acceptance checks. The first slice must work end to end before claiming completion. The broader model is not declared complete merely because its first slice is finished.

## Automatic completion invariant

Stop exactly one team's shared Lab generation only when every pinned question/flag that depends on that generation is complete. Solving the first question of a multi-question laboratory must not close it. Other laboratories and other teams remain accessible. Practice completion may close its own environment without changing the existing distinction between rated and practice scores.

Backend locks the canonical shared-laboratory aggregate before the question mutation. The answer transaction atomically records completion, logical access withdrawal, the desired lifecycle revision/operation ID and the ACL dirty revision. It performs no snapshot, agent RPC or runtime deletion synchronously. A durable record drives an idempotent background worker even if the process restarts or an immediate wake-up is lost.

The submission result, participant board and runtime API return the same stable Lab identity and authoritative logical closure. HTTP/proxy links, IP links and new lab sessions are refused after closure; stale caches and in-flight link requests cannot reopen them. Existing stage `Closed` and returnable `Practice` retain their meanings.

## Laboratory contract

Add plural `StopLabs` and `StartLabs` operations. Every item carries exact LabGroup/Lab reference, expected Lab UID, operation ID and monotonically increasing lifecycle revision. Stop additionally carries explicit snapshot policy and optional retention deadline. Acceptance means intent accepted, never actual stop completed. Duplicate identical intent is idempotent; stale UID/revision and conflicting equal revisions are refused.

Retain existing protobuf field numbers, readiness, scheduling, snapshots and traffic. New `LabStatus` fields use fresh tags 41/42 for lifecycle and resources. Missing lifecycle intent means Running for existing Lab objects. Existing `LabGroup.spec.suspended` semantics are preserved: they currently retain VPN/gateway and must not silently become a full stop.

Internal observed states distinguish Running, Snapshotting, Stopping, Stopped, StopFailed, Starting and Unknown. Observations echo UID, operation/revision and observed generation. A newer explicit start fences older stop and capture workers; ordinary polling, deployment reconciliation or group resume never restarts a individually stopped Lab.

Preserve Lab/Device/Connection definitions, stable codes, CIDRs, image/snapshot references and credentials while stopped. All controllers, scheduler dispatch, network/fabric reconciliation and deploy retries must honor stopped intent. Switch-only laboratories are included.

Detailed wire and CRD proposal: `/Users/volodymyrporokhniak/.codex/outputs/2026-10-08-lab-lifecycle/laboratory-contract-proposal.md`.

## Required snapshot barrier

When the configured policy requires a snapshot, every required device must explicitly acknowledge a successful capture of its exact live pod UID, epoch and incarnation for the current operation. Existing `ExitSnapshotPod` is unsuitable: it also acknowledges giving up after failure. Add a separate capture request/result owned by the operator and node-agent respectively.

Valid unchanged state is acknowledged explicitly. Quota refusal, missing runtime, capture/push error or timeout is failure. Preserve the latest valid image on failure, retain runtime allocations and expose StopFailed. No required snapshot failure permits deletion or claims released capacity.

Quiescence must cover the final snapshot and stop boundary. A node-agent restart/thaw invalidates old success acknowledgements. Only a complete current barrier permits stopping devices. Snapshot-backed restart must restore the saved writable-layer state and UID/GID mappings. Filesystem preservation must not be described as preservation of arbitrary process memory or unrecorded runtime network changes.

## Confirmed resource accounting

Report configured requests/limits, actually held runtime requests, measured usage when available, release state/time, retained snapshot quota and physically known storage separately. Missing or stale observations mean Unknown, not zero usage or free capacity. Delete-request acceptance is not runtime or storage release confirmation. A force-deleted Pod on an unreachable node does not prove its process stopped.

Count one allocation per shared Lab, one VPN/gateway overhead per team group, pending starts and retained storage. A stopped Lab frees device compute after acknowledgement; it does not free a fraction of a still-running group service. Reservation planning, per-node placement, measured usage and billable infrastructure remain separate.

If an explicit full-group stop is later requested, require all child Labs confirmed stopped and no pending starts; stop VPN/gateway as well while retaining the group identity/configuration. Group start alone never changes child stopped intent.

## Minimalist sizing

Use actual planned participant count, or the configured maximum team size when membership may grow. Five is a documented fallback when planning information is missing, not an immutable product limit. Include active Lab count, internet Lab count, allowed relations and an explicit supported traffic/retained-flow envelope. Resource sizes are immutable after group creation today; reserve for the chosen maximum or explicitly handle controlled recreation, never silently under-size an existing group.

Fresh native measurements for separate five-peer groups: 224 matched observations, 10–13 Labs, 7–10 internet Labs, 50–65 allowed relations. Maximum observed VPN lifetime memory peak 40.004 MiB; gateway 13.961 MiB. Existing test limits were 164 MiB and 44–56 MiB. The 26 MiB consolidation saving is not a pod-sizing equation.

Candidate to validate locally: VPN 80 MiB / 50m, gateway 32 MiB / 25m for the measured structural envelope and explicitly bounded planned flow profile. These are engineering test candidates, not supported defaults yet. Proposed versioned sizing includes U/L/I/P/F and traffic rates; preserve legacy sizing fields for old consumers. Until meaningful lower-limit tests pass, keep existing supported presets and expose the new recommendation as unvalidated. No finite small memory guarantee follows from U/L alone while conntrack and collector flow allocations remain unbounded.

Raw extraction and Linux proof plan: `/Users/volodymyrporokhniak/.codex/outputs/2026-10-08-lab-lifecycle/small-group-native-peaks.csv` and `linux-proof-plan.md` in the same directory. No repeated old performance run is required.

## Stage and manual behavior

`all_ready`: reserve and prepare the fixed eligible-team set before simultaneous opening. Teams cannot manually stop/restart unresolved environments. Resource release from solved copies must not privately open another task for a subset of teams.

`as_ready`: explicit participant stop does not solve a task, keeps score/progress, and can be explicitly restarted if stage access, active-lab limit, event budget and cluster placement permit it. No automatic resurrection. Do not confuse publication mode with the existing rolling-roster join policy.

Stage closure withdraws runtime access; retention determines stopped preservation versus deletion. Keep event results/history independent of infrastructure deletion. Existing returnable-stage practice must not be silently removed. Moderator forced per-team stop powers remain outside this work until separately decided.

## Consumer ownership and compatibility

Laboratory owns physical lifecycle/snapshot success/resource observations. Backend owns participation, solve completeness, authorization, durable desired intent and event admission. Frontends render authoritative states and never infer closure from one visible question or optimistically credit capacity.

Event frontend handles immediate shared closure and resource/manager views. Admin handles detailed actual state and failure observations. Exercise-author test environments are not event participant environments and remain unchanged unless a separate policy is requested.

Local Go workspace currently overrides Laboratory with the local checkout while published builds pin v1.0.0. Verify both build modes; never claim published-pin compatibility from a local workspace pass. Release/publication is a separate owner decision and is not bypassed by this feature.

## Acceptance checks

- First/second dependent solve stays open; final solve closes only its owning team's exact Lab once, preserving scores/history.
- Concurrent final answers, duplicate attempts, moderator answer acceptance and job retries obey the same aggregate lock and generation fence; annulment never implicitly resurrects an environment.
- Lost wake-up, process restart, agent outage, stale operation/UID/revision and accepted-but-not-stopped replies do not lose stop intent or free resources early.
- Required unchanged/successful snapshots permit stop; capture/push/quota/runtime errors retain all workloads; node-agent restart invalidates stale quiescence success.
- Confirm actual runtime/network teardown and snapshot restore on Linux; stopped deployment/controller/group recovery cannot recreate devices or links.
- Participant closure is settled immediately; stale cached IP/proxy links and late link responses stay closed; other shared environments remain available.
- Admin and event resource totals include group overhead and retained storage, retain pending/unknown allocations and credit only matching actual release.
- Existing CRD/protobuf clients, stage/history, native isolation/counter tests and generated artifacts remain compatible.
- Smaller resource candidates undergo actual native kernel/CPU/cgroup testing; observe retained-flow growth, peaks, OOM and throttling, and record profile boundaries.

## Implementation ownership

Separate agents own Laboratory, daemon, and event/admin consumers. They may prepare domain work in parallel after contract review; consumer integration follows the frozen producer fields. Canonical tests and targeted native/database/UI checks precede local commits. No unrelated cleanup, release, remote writes or speculative moderator/academy features.
