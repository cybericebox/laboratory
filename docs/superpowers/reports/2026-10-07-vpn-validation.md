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
| 10 users / 10 labs | 49.5–50.0 µs | 46,672 | 158 |
| 10 users / 20 labs | 96.6–97.0 µs | 91,824 | 260 |
| 20 users / 10 labs | 104.3–104.9 µs | 96,880 | 301 |
| 20 users / 20 labs | 202.8–203.7 µs | 186,128–186,129 | 503 |
| Unchanged empty-plan fingerprint | 20.2–20.4 ns | 0 | 0 |

The unchanged fingerprint microbenchmark uses an empty plan and is not a timing estimate for a populated group. The 20×20 control test exercises the populated matrix and confirms the absence of kernel application on 100 telemetry updates.

## Final review and additional regressions

One fresh read-only whole-change review identified eight Important cases. All were corrected in a single fix pass with failed-then-passing regressions: serialized counter read/fold/retirement and snapshot/publication; closed reissued/cold-unknown bindings until successful conntrack retirement; DHCP migration to a valid offer while retaining the old address reservation; shutdown quiescence before final sample; stopped DHCP handler rejection/draining; client-original activity timestamps; source-leg closure during gateway replacement; and the missing kernel validation matrix.

The startup access gate is also rebuilt with FORWARD in one closed atomic batch. Verified retained identities preserve permitted flows; unknown persisted identities are retired before activation. Kernel tests now continue established TCP and UDP sockets in **both original orientations**, delete real conntrack entries, send ten repeated SYNs (one initiative), remove a UDP flow before reading its retained native counters, change another allowed pair while the first remains active, preserve unrelated connmark bits, and verify shutdown cannot reopen the gate. No second review was dispatched; the fix pass is verified by regressions and full gates.

## Final gate results

- Default `make test`: **PASS** after the fix pass (includes native project tests, envtest, generation, formatting and vet; the Makefile deliberately separates installed-cluster e2e).
- `make test ENVTEST_K8S_VERSION=1.33.0`: **PASS**.
- Full `make test-netns`: **PASS**, no selected kernel tests skipped, shared VPN/gateway/DHCP/accessroute/cnigate cases included.
- Relevant Linux package race checks and vet: **PASS**.
- Linux command binaries: **PASS**.
- Generated CRD/protobuf consistency and diff whitespace: **PASS**.

The separate Kind e2e fixture remains unrun because no Kind cluster was supplied; no AWS/x86 capacity or NAT throughput claim is made. The initial envtest start failure and all successful reruns remain in the evidence archive. Metadata enrichment additionally checks the trusted current virtual client endpoint; this value is ephemeral and absent from public reports and private persisted checkpoints. A migrated DHCP owner can release its previous reservation, while other owners cannot.

Final named native-test inventory: **872 PASS / 2 expected runtime or helper SKIP / 0 FAIL** across 45 tested packages. The two skips are the CRI integration requiring an image service and the proxy memory subprocess helper; selected Linux kernel tests have no skips. Full individual results and raw logs are included in the evidence archive.

Owned local test infrastructure removed and independently verified at **2026-10-07T01:26:39Z**: both named VPN/gateway containers, all disposable netns runners, and the image created by the e2e fixture. Shared tool/cache volumes were preserved. AWS resources created during implementation: **zero**.
