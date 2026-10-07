# VPN validation — 2026-10-07

Environment: Docker Linux aarch64, Go 1.27; disposable privileged network namespaces. No AWS resources created. Existing 57-stage c7i x86 measurements are unchanged.

| Check | Observed result |
|---|---|
| Symmetric pair gate and spoofed lab source | PASS, physical interface defines lab identity |
| Revoke pair and routed-source flow | PASS, both original directions covered |
| 300 concurrent UDP writes in one flow | 300 packets / 9900 IP bytes / 1 client initiative / 0 lab initiatives |
| TCP client flow and lab replies | PASS, reply does not add lab initiative |
| Unchanged reconcile native counters | PASS, 7 packets / 700 bytes retained |
| Kernel binding retirement | PASS, gate closes before final counter fold; historical totals survive |
| Retained epoch resume / changed epoch / short flow | PASS; no doubled totals, reset flagged Partial |
| Overflow / ownership / bounded ledger | PASS; saturation and Partial, MaxRows=512/Truncated |
| 20 clients × 20 labs, 100 stats updates | Zero additional applications; real permission update applies once |
| Linux VPN race tests | PASS |
| Agent + traffic race tests | 175 passed, including envtest and protobuf compatibility |
| Whole netns suite | PASS (VPN, reconciler, gateway, accessroute, cnigate), no kernel skips |
| Go vet / generated CRD and protobuf consistency | PASS |

One changed permission snapshot uses one gate Restore; retirement adds one cleanup Restore so accepted packets between Save and gate closure are retained. No Restore/Save/conntrack revocation from unchanged telemetry inputs. Sampling is 5 seconds and report publication one minute; policy allow counters come from the same aggregate.

## Repeat resource comparison

Use two x86 c7i.2xlarge nodes, the original pinned cluster/network versions, original 1-core VPN cap and identical sampling intervals. Pin the new image digest and source commit; preserve the original dataset. Repeat empty VPN/gateway, 10/20 clients × 10/20 lab legs, handshakes, availability probes, fixed pps/packet sizes, concurrent flows/new-flow rate, DHCP bursts and same/cross-node placements. Separate Geneve and VXLAN stages; collect container CPU/RAM, node CPU/softirq, conntrack occupancy and actual counter correctness. Produce full stage table and raw archive and independently verify deletion of experiment resources.

Local compiler/fingerprint benchmarks are management-path measurements only; they cannot establish a new resource minimum, clients per group or NAT throughput. Equivalent external x86 comparison remains a separate authorized experiment.

## Local management benchmarks (3 runs)

| Compile permissions | Time per operation | Bytes allocated | Allocations |
|---|---:|---:|---:|
| 10 users / 10 labs | 49.5–50.2 µs | 46,672 | 158 |
| 10 users / 20 labs | 96.1–96.9 µs | 91,824 | 260 |
| 20 users / 10 labs | 104.2–104.7 µs | 96,880 | 301 |
| 20 users / 20 labs | 200.6–203.0 µs | 186,128–186,129 | 503 |
| Unchanged empty-plan fingerprint | 14.6–14.8 ns | 0 | 0 |

The unchanged fingerprint microbenchmark uses an empty plan and is not a timing estimate for a populated group. The 20×20 control test exercises the populated matrix and confirms the absence of kernel application on 100 telemetry updates.
