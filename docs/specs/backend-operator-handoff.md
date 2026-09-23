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
| Retain configuration outside the event runtime | `LabGroup.spec.suspended` scales VPN, gateway and Device Deployments to zero; resume restores them without replacement resources | agent and controller tests; backend lifecycle tests |
| Operational visibility | secret-free `Monitoring` stream with ordered snapshots/deltas and `GetCapacity` | agent monitoring tests; backend ingestion tests |
| Final withdrawal cleanup | deleting the `LabGroup` cascades its Labs and VPN clients | backend cleanup worker and agent delete RPC |

## Required live integration gate

The repository's `test/e2e` suite requires a Kind cluster named `kind`. It is
the remaining verification gate for this contract: mTLS agent connection,
operator image deployment, LabGroup namespace readiness, per-Lab ACL,
one-time VPN configuration, monitoring/capacity ingestion, suspension and
resume of retained workloads, and withdrawal cleanup. It must run against a
real cluster before frontend work is accepted.

The local unit suite is expected to stay green without that cluster; a missing
Kind cluster is an environment prerequisite, not an implementation failure.
