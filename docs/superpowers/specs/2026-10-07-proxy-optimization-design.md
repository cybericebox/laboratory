# Proxy optimization — proposed design, 2026-10-07

Status: owner approved the complete implementation on 2026-10-07 ("Да, давай все"). Branch: `fix/envtest-crd-warmup`, native execution continues after approval. Owner order: finish proxy and agent entirely in laboratory, verify/freeze its contract, then backend and finally the consuming frontend. "Контент" was clarified by the owner to mean the data contract.

## Purpose and boundary

Reduce repeat management work and per-request allocations while making existing permitted-access counters truthful and durable. Keep HTTPS handoff/session authentication, tenant and lab authorization, cookie protections, WireGuard end-to-end cryptography, quotas and current Go data path. There is no new DNS/client application, payload logging, denied-rule analytics or AWS deployment in this stage. Packet forwarding/HTTP processing is not made dependent on successful analytics delivery.

## Evidence from current code and runtime

| Area | Current behavior | Verified consequence |
|---|---|---|
| Active HTTP stream | Meter.Record follows ReverseProxy.ServeHTTP | Answered but still-open stream has an empty ledger |
| Proxy process restart in the same pod | New Meter; report key stays proxy-pod | Existing8 plus new1 is replaced by1 |
| Failed upstream | countingWriter also sees proxy-generated error body | Probe recorded12 proxy error bytes as lab bytes |
| UDP stop | ReadFromUDP blocks; only checks stop before/after read | Closing stop alone does not release an idle reader |
| Expired receiver index reuse | Old mirror remains; cleanup deletes its PeerIndex unconditionally | Old entry deletes the new session's reverse index |
| Upgrades | Hijacked byte stream is outside countingWriter/body | WebSocket data is absent from byte totals (code verified) |
| Report publication | Each namespace scans the whole global Meter map | G groups × R rows work, instead of per-group rows |
| Live quotas | Every admission scans all active requests | Cost grows with all active sessions |
| Demux configuration | All LabGroup updates rehash/resolve/update | Unrelated status/metadata can trigger repeat DNS work |

Baseline: 102 short/race tests passed, three expected memory/helper skips; separate memory checks passed. macOS ARM64 test process: 100,000 demux entries use18.3MiB retained heap (192B/entry); isolated L7 harness idle heap+stack4.2MiB and approximately103–107KiB per held TLS+HTTP+upstream request, reaching207.9MiB at2,000. These exclude Kubernetes informer caches and do not establish cluster throughput or production RSS. Probe tests ran through a Go overlay; no product source was altered by the investigation. Raw evidence is in `/Users/volodymyrporokhniak/.codex/outputs/2026-10-07-proxy-review`.

## Approaches and selected scope

1. Correct lifecycle/accounting first, then measure and optimize the identified loops within the current data path. This separates correctness regressions from performance comparisons and keeps rollback small.
2. Combine lifecycle changes with a larger replacement of session/routing structures in one change. This is harder to attribute and validate and has no supporting throughput measurement yet.

Recommend1. The following design describes its final behavior; the implementation plan will split it into testable local commits.

## HTTP/L7 routing and access

Resolve the group namespace, route protocol/target and lab identity once per request from one cache view. Build compact read-only access inputs that omit status/counter fields; relevant Service labels/ports, group namespace/tenant/deletion, client existence/deletion and policy Spec changes refresh them. Authorization remains default-deny, deny wins, and namespace/tenant scope remains mandatory. Do not introduce an authorization TTL that delays revocation.

Prewarm the required informer kinds and load counter state before advertising readiness. Watch relevant changes and retain periodic checks as a safety net. A client or group marked for deletion is unavailable. The same access decision closes ongoing requests; optimize checks by distinct active access keys rather than repeated full object copies for every connection.

## Permitted traffic accounting

Current web protocols are HTTP/1.1 and HTTP/2 over TCP, plus upgraded WebSocket connections. HTTP/3/QUIC is not implemented or added in this stage. There is no UDP user-traffic analytics at the proxy. Keep one Meter authority per process. Begin an attempt after route/access/quota checks and before forwarding; it becomes visible while the request is open. Record response timing when the upstream responds, not at request start. Progress updates count body bytes and do not retain paths, headers, addresses or payloads. Completion is exactly once and runs for cancellation, disconnect and ReverseProxy abort as well as normal return.

For HTTP, define byte directions relative to the participant and retain application-body scope. Own401/404/429/502 responses are not laboratory response bytes. For WebSocket, count useful application bytes after a successful101 upgrade, excluding the proxy's handshake, TLS/TCP overhead and WebSocket framing/control traffic. Forward bytes unchanged; read only the framing metadata necessary to measure lengths and never retain or interpret application message contents. Measure application payload as relayed (compressed when an extension negotiated compression), without decompression or content inspection; continuation fragments contribute payload, control frames do not. Test the negotiated extension/framing cases before claiming a useful-byte count. If a byte scope cannot be established reliably, report it as unavailable/incomplete instead of labelling transport bytes as useful traffic. This scope is included in the later public-contract documentation.

Index cumulative rows by namespace and (client,lab), avoiding global scans for every group report. Maintain live quota counters by client/group with balanced registration/removal. Cap memory and public rows as today, report per-group truncation/overflow explicitly, and never turn saturation into negative totals. Retired namespaces can reclaim in-memory rows after their lifecycle ends; reclamation must not discard state that a live or draining group still needs.

## Restart, reporting and shutdown

Continue existing cumulative values for the same writer/report key; do not overwrite them with a new boot's zero totals. Serialize restore/new observations/publication. A restore failure cannot silently replace a larger report; retain delivery error/completeness state and retry while serving traffic. New replica report keys continue to add once in the existing replica aggregation; do not add a parallel analytics journal.

Coordinate HTTPS shutdown and reporting: stop admissions, drain/cancel tracked requests within the existing grace period, explicitly close hijacked connections, wait for their counters to settle, then publish the final snapshot. `http.Server.Shutdown` alone does not close WebSockets. A failed final delivery is visible; no claim of zero-loss persistence across an abrupt process kill between minute reports is made. Coverage/completeness must distinguish a healthy idle collector from missing/truncated data.

## WireGuard UDP demux

Owner correction: **do not collect or export UDP traffic statistics here**. The demux relays encrypted WireGuard datagrams; their ciphertext/transport overhead is not useful laboratory traffic. Per-user/per-lab permitted packets and bytes are accounted on the VPN server after decapsulation. Keep the existing WireGuard peer statistics as their own VPN metrics; do not add demux byte totals or sum them with decrypted counters. Existing routing/session limits are operational protections, not a new traffic ledger. UDP test packet/rate observations are validation inputs only.

Bind the two receiver indices to one explicit session identity/lifetime so stale cleanup cannot delete a newer session. Reusing an expired slot retires only its actual old pair; reciprocal identity is checked on removal. TTL and limits use the shared session activity, and invalid/expired packets do not resurrect an expired binding.

Track the owning LabGroup/backend generation, retire bindings on removal or relevant key/backend change, and reject irrelevant reconfiguration events. Table updates skip unchanged keys/endpoints; DNS resolution remains outside the packet loop and uses cancellation/bounded lifetime.

Closing the stop signal closes the socket and joins readers/cleanup/resolver work. A closed socket exits instead of spinning on errors. Validate WireGuard packet type/reserved header/length before touching state; do not forward a silently truncated datagram. Consider immutable address values and reduced per-packet allocations after measurement. Shared state sharding/other accelerated forwarding is not justified in this first slice without a demonstrated bottleneck. No second per-lab UDP traffic ledger is added; VPN counters remain the traffic authority.

## Validation and measurements

Regression tests first for the five reproduced failures; then add active/cancelled HTTP and real WebSocket direction counters, same-pod restart and fresh-replica aggregation, failed publication retry, bounded shutdown with open streams, group retirement and cap reclamation, per-group truncation/overflow, namespace/tenant/deletion isolation, access revocation and ready-after-restore.

Demux tests cover cold stop, close-without-stop, actual UDP forwarding/rekey/roaming, shared lifetime, expired index reuse and index collisions, group retirement/backend changes, malformed/oversized datagrams and concurrent forwarding/cleanup/update under race detection. Do not infer valid WireGuard cryptographic sessions from synthetic type4 packets; use a real WireGuard integration where an actual session claim is needed.

Profile 1/10/50/200 groups, 20/200 clients, idle and held connections, 300/1,000/2,000 HTTP streams, fixed UDP packet-size/rate and connection/rekey rates solely for demux correctness and CPU/RAM characterization, not for user traffic analytics. Separate TLS/HTTP, WebSocket, handshake scan and transport-forwarding costs. Report local CPU/allocations/RAM/operation counts with architecture, full tables/raw logs and owned-resource cleanup. x86 two-node comparisons require an explicitly described separate experiment.

## Agent/AP Backend contract — next stage

Confirmed gaps to address after proxy behavior is finalized:

- Agent `monitoring_filter.go:cutTraffic` drops empty ledgers, including the idle heartbeat needed for coverage.
- Replica merge uses unchecked signed sums and one envelope for coverage; define reset/deduplication/coverage completeness precisely.
- AP Backend `daemon/internal/monitoring/lab/traffic.go` discards attempts=0 or firstSeen=0, so lab-only initiated VPN traffic is lost; its model/schema has no labInitiatedAttempts field.
- Truncated reports are not treated as incomplete coverage by ingest. Missing/truncated data must not become proof of "untouched".
- Participant-action timestamps remain distinct from lab-driven traffic. Preserve unknown times as absent, not invented1970 dates or participant activity.
- Do not add WireGuard demux UDP/ciphertext counters to the contract; HTTP/3 is a separate future feature.
- Update the protobuf dependency, ingest/model/SQL migration/generated queries and analytics interpretation together; keep public directions/counter units/zero semantics documented and old-client compatibility explicit.

Daemon implementation: owner selected a new branch in the current directory; `feat/proxy-traffic-contract` starts from develop. The pre-existing untracked AGENTS.md is preserved and excluded from commits.

## Primary references

- WireGuard protocol: https://www.wireguard.com/protocol/
- Go HTTP shutdown semantics: https://pkg.go.dev/net/http#Server.Shutdown
- ReverseProxy behavior and BufferPool: https://pkg.go.dev/net/http/httputil#ReverseProxy
