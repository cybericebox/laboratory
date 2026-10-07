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
| 1 | off | 7 | 0 | 0.07 | 40,860 |
| 1 | on | 8 | 0 | 0.10 | 43,360 |
| 10 | off | 70 | 0 | 0.10 | 48,156 |
| 10 | on | 80 | 0 | 0.11 | 43,352 |
| 20 | off | 140 | 0 | 0.16 | 50,648 |
| 20 | on | 160 | 0 | 0.17 | 47,960 |

These single-run CPU/RSS figures include the Go test runtime, fake Kubernetes store, interface setup and teardown. They are **not** steady-state VPN/gateway reservations, architectural capacity limits or NAT-throughput measurements. Use the equivalent x86 two-node protocol in the VPN validation report for comparison with the preserved AWS dataset.

Configuration failures stop DHCP and close its INPUT exception; NAT readiness is tracked independently. Loss/replacement of a lab interface invalidates its applied cache. DHCP readiness is queried from live server health and health transitions requeue affected lab legs (coalesced full resync on a notification burst); a 30-second requeue backs up event delivery. Lease state survives disable/enable of the same lab/network until expiry and is dropped on lab deletion.
