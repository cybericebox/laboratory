# Laboratory validation — 2026-10-07

All five self-written image targets (controller, agent, proxy, node, lab) were checked. The generic laboratory registry artifact is the Helm chart; the chart's separate kube-scheduler remains upstream. Earlier VPN/Gateway/DHCP baseline results remain unchanged. No backend optimization was included.

| Verification | Result | Evidence |
|---|---|---|
| Complete native make test | PASS in isolated module; parent go.work generator failure preserved | make-test-isolated.log; make-test.log |
| Complete race suite (-short) | 993 PASS /4 expected SKIP /0 FAIL,78package outcomes | final-race-tests.jsonl; test-table.md |
| Native proxy suite including sizing | 134 PASS /1 helper SKIP /0 FAIL | final-native-sizing.jsonl |
| Privileged Linux canonical matrix | 23top-level /45inclsubtest PASS /0 SKIP | node-implementation/canonical-netns.log |
| Authenticated WireGuard | small+fullMTU both directions, key/backend retirement+rehandshake and stop PASS | proxy-wireguard-proof/full-mtu-green.log |
| WebSocket | TCP upgrade, buffered frames, both directions and negotiated compressed fragmentation/control exclusion PASS | final-compressed-websocket.log |
| Public CRD→serialized gRPC | counters/IDs/times/disjoint spans preserved, checkpoints private, authorized idle coverage PASS | agent-performance/implementation evidence |
| All final image targets | 5targets×2architectures PASS, only own program; runtime copy hash equality | image-build-validation/matrix.json |
| Agent final-image runtime |250m/256Mi;20mTLSstreams/3tenants/480RPCs0errors,0OOM; RSSpeak61.6MiB; default30sdrain exit0 | agent-runtime/report.md |
| Controller real-cache/image runtime |200groups/2258devices/798connections; warmstatuswrites502→0;128Miimage startup/recovery/probes PASS | controller-implementation/implementation-report.md |
| Node ARM64+native AMD64 | kernel/namespace/CNI/OVS/supervision PASS; native predecessor and final executable/dependency hashes equal | node-native-amd64; final-node-native-equivalence.json |
| AWS deletion | ownedinstance terminated; network/disk/key absent; localSSHkey removed, independently verified | node-native-amd64/cleanup.json |
| go vet /generated artifacts/Helm | PASS; CRD copies match | final-vet.log; final-manifests-generate.log; component logs |

Final skips: CRI pull requires its external registry/runtime fixture; two memory load/helper cases are excluded from race-short and checked in native sizing; runtime helper itself has no normal-run task. Privileged Linux coverage runs separately withzero skipped kernel cases. Kind/full-cluster e2e is outside the local default suite; supplied realcache/image/kernel/CRD/RPC fixtures are named rather than claimed equivalent.

Earlier instrumented TLS connection-sizing attempts failed kernel acceptance at257/300 and986/1000. They are preserved as unresolved instrumented stress results; correctness race suite and native steady sizing passed. Native sizing: idle4.3MiB,2000heldHTTPrequests207.2MiB heap+stack,about103KiB/request; demux100kentries19.0MiB/200B each. These exclude full production caches/RSS and establish no cloudcapacity.

One independent whole-change review (gpt-6-astra,167279f versus64d2363) found3issues, all reproducedRED and fixedinonepass: valid MTU WireGuard drop, destructive saturated baseline restore, selected-idle coverage loss. The same authenticated WG fixture now passes; namespace/foreign-data and restore/retry regressions pass. No re-review claimed.

Measured agent retained-history growth493MB→6.17MB and partialfanout11.51GB→454MB allocations; fanout10.02s→2.77s. Quietpoll still~169MB transient/noCPUimprovement claim. Defaults/resources/cadences remain; no sizingrecommendation derives from syntheticbenchmarks.

Limitations: unpublishedtraffic onabruptkill and never-observed historicalreplicas cannot be reconstructed; completeKubernetes/CRI/productionhostbootstrap and arbitrarylargeproduction workloads remain unproven. OVSimplementation untouched; remainingflow-cache/CRIreuse/polling opportunities require invalidation/recoveryproof. IPv6/isolation/VPN/GWpriorstage evidence remains separatelyarchived.

All decisions and deferred scope are recorded in implementation-decisions.md. Raw priorbaselines, postchecks, failures and nativeverification retained. Only experiment-ownedresources are deleted; unrelatedcontainers, sharedcachevolumes/baseimages and pre-existingenvtestPIDs704/709 are preserved.
