# Gateway / DHCP validation — 2026-10-07

Environment: Linux ARM64 Docker namespace runner, Go 1.27. No AWS/Cloudflare creation or deployment.

| Check | Result |
|---|---|
| Offers / leases / renewal / expiry / release ownership | PASS, 30 seconds / 24 hours; injected clock |
| Abandoned offer / range shrink / reserved gateway / 100 concurrent leases | PASS |
| Selected/other-server REQUEST, INIT-REBOOT, RENEW, RELEASE, DECLINE, INFORM | PASS; [RFC 2131](https://www.rfc-editor.org/rfc/rfc2131.html) state distinctions |
| Generated packet options | PASS, router/server ID/lease duration/optional DNS decoded |
| Two real lab sockets, broadcast discovery/request, unicast renew/release | PASS; independent pools and DNS |
| Identical/hot config / failure / Stop / Drop / concurrent lease operations | PASS; no replacement on DNS/range updates |
| Lab/Pool input filtering, names and incarnation ownership | PASS |
| Forbidden Lab read and lost interface readiness | PASS; errors visible and DHCPReady false |
| Helm role and live RBAC authorization | PASS; gateway service account can get/list/watch Labs in its namespace; other namespace and writes are Forbidden |
| Linux relevant packages under race detector / go vet | PASS |
| Controller/chart/proxy/agent race checks | 476 passed; proxy implementation unchanged |
| Kernel NAT/private-destination/anti-spoof/INPUT/IPv6 boundaries | PASS with shared VPN/DHCP suite |

## Management workload

Real lab interfaces and DHCP sockets, fake Kubernetes client and recording filter boundary; ten unchanged reconciles per lab. Filter counts include relevant API calls on the boundary, rather than iptables process invocations. Every unchanged reconcile performs **zero** filter operations; it still verifies interface identity/address and reads current Lab/Pool state. No cache is restored from previous process state.

| Labs | DHCP | Initial filter operations | Repeat filter operations | Harness CPU user+system s | Harness max RSS KiB |
|---:|---|---:|---:|---:|---:|
| 1 | off | 11 | 0 | 0.08 | 45,852 |
| 1 | on | 12 | 0 | 0.07 | 43,244 |
| 10 | off | 110 | 0 | 0.11 | 44,900 |
| 10 | on | 120 | 0 | 0.12 | 49,700 |
| 20 | off | 220 | 0 | 0.16 | 50,276 |
| 20 | on | 240 | 0 | 0.17 | 48,424 |

These single-run CPU/RSS figures include the Go test runtime, fake Kubernetes store, interface setup and teardown. They are **not** steady-state VPN/gateway reservations, architectural capacity limits or NAT-throughput measurements. Use the equivalent x86 two-node protocol in the VPN validation report for comparison with the preserved AWS dataset.

Configuration failures stop DHCP and close its INPUT exception; NAT readiness is tracked independently. Loss/replacement of a lab interface invalidates its applied cache. DHCP readiness is queried from live server health and health transitions requeue affected lab legs (coalesced full resync on a notification burst); a 30-second requeue backs up event delivery. Lease state survives disable/enable of the same lab/network until expiry and is dropped on lab deletion.

## Final review corrections and verification scope

Range shrink now migrates an owner to a new valid offer while retaining the prior address reservation until its original expiry. Stop rejects handlers dispatched late by server4 and joins handlers already running before pool reuse. Replacement/deletion closes a per-interface source DROP before removing anti-spoof/NAT configuration; failure leaves it closed. Real kernel traffic cannot escape through another lab's existing MASQUERADE binding during replacement.

The first broad `go test ./...` run passed 864 tests and had two expected runtime/helper skips; the separate e2e scaffold failed **before any spec** because its required Kind cluster was absent. The canonical `make test` target excludes `/e2e` by design. A first default run encountered envtest API-start failures; `make test ENVTEST_K8S_VERSION=1.33.0` and a targeted envtest1.37 rerun passed. Both failure and successful logs are preserved; a fresh default final gate is recorded below. No production-version incompatibility is inferred from a fixture start failure.

## Final gate results

- Default `make test`: **PASS** after the fix pass (includes native project tests, envtest, generation, formatting and vet; the Makefile deliberately separates installed-cluster e2e).
- `make test ENVTEST_K8S_VERSION=1.33.0`: **PASS**.
- Full `make test-netns`: **PASS**, no selected kernel tests skipped, shared VPN/gateway/DHCP/accessroute/cnigate cases included.
- Relevant Linux package race checks and vet: **PASS**.
- Linux command binaries: **PASS**.
- Generated CRD/protobuf consistency and diff whitespace: **PASS**.

The separate Kind e2e fixture remains unrun because no Kind cluster was supplied; no AWS/x86 capacity or NAT throughput claim is made. The initial envtest start failure and all successful reruns remain in the evidence archive. Metadata enrichment additionally checks the trusted current virtual client endpoint; this value is ephemeral and absent from public reports and private persisted checkpoints. A migrated DHCP owner can release its previous reservation, while other owners cannot.
