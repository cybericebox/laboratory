# Backend to Laboratory operator handoff

This document records the currently verified boundary between AP Backend and
the Laboratory operator. It supersedes older notes that described per-Lab
access control or monitoring as missing.

## Implemented contract

| Backend need | Operator and client support | Verification |
| --- | --- | --- |
| One resource per team challenge | `LabGroup` per team and `Lab` per challenge; create, get, update and delete RPCs | agent unit tests |
| Participant VPN credentials | one-time `CreateLabGroupClient`; the private key is returned once and is not retained by the operator | agent client tests |
| Default-deny access to a particular challenge Lab | full replacement `ReconcileLabGroupAccess` policy, enforced by the VPN reconciler | agent ACL tests |
| Revoke access before start, after finish, or when a Lab is not ready | empty policy is default-deny; AP Backend reconciles lifecycle boundaries | backend ACL tests |
| Operational visibility | secret-free `Monitoring` stream with ordered snapshots/deltas and `GetCapacity` | agent monitoring tests; backend ingestion tests |
| Final withdrawal cleanup | deleting the `LabGroup` cascades its Labs and VPN clients | backend cleanup worker and agent delete RPC |

## Remaining operator work

### Suspend and resume a LabGroup

**Reason.** At event finish AP Backend already revokes VPN access. It cannot,
without changing the operator, stop the VPN, gateway and challenge workloads
while retaining their CR configuration for review.

**Required shape.**

1. Add an explicit desired `suspended` state to `LabGroup` and surface the
   observed state/condition in its status.
2. Add an idempotent agent RPC and client method that changes this state. It
   must be a replace/set operation, not a toggle.
3. When suspended, scale the per-group VPN and gateway deployments to zero and
   prevent Lab reconciliation from recreating or scaling challenge device
   workloads. Keep CRs, allocated networks, secrets and access-policy state.
4. When resumed, reconcile those same objects back to their desired running
   state and report readiness through the existing monitoring stream.
5. AP Backend may then request suspend on finish and resume only through an
   explicit manager action. Event withdrawal remains the sole destructive
   cleanup boundary.

**Acceptance check.** Create a group with a ready Lab and VPN client; suspend
it; prove all workloads have zero replicas and VPN traffic is unavailable;
resume it; prove the original Lab, client identity, ACL and readiness recover
without recreating the group.

## Required live integration gate

The repository's `test/e2e` suite requires a Kind cluster named `kind`. It is
the remaining verification gate for this contract: mTLS agent connection,
operator image deployment, LabGroup namespace readiness, per-Lab ACL,
one-time VPN configuration, monitoring/capacity ingestion, and withdrawal
cleanup. It must run against a real cluster before frontend work is accepted.

The local unit suite is expected to stay green without that cluster; a missing
Kind cluster is an environment prerequisite, not an implementation failure.
