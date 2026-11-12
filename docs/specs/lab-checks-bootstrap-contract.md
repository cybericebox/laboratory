# Device bootstrap, lab checks and device access: the laboratory contract

**Agent API contract version:** 1.4 -> 1.5. Everything here is additive: new field numbers, no renames, no removals. An old daemon keeps working; an old agent simply lacks the fields.

**Design record:** `docs/specs/2026-11-11-lab-checkers-bootstrap-devices.md` in the platform repo (sections 3, 4.1, 4.2, 5, 7). **Scope of this step:** the data model, the Secrets, the validation, the report and the manual-run request. The checker is described in `lab-checker.md`.

The launcher and the pod-spec changes it needs (stager init container, memory volume, startup and readiness probes, recreation of a pod whose service ended, `status.bootstrap`) are built: see `DEPLOY.md`, "Device bootstrap and the service command", and package `internal/launcher`.

Not implemented (deliberately):

- ~~the checker component~~ (done in the step after this one: `docs/specs/lab-checker.md`; the DaemonSet is rendered while `checks.enabled` is true, and its keepalive is a Lease per node);
- the terminal component and the proxy route for it;
- full VMs, the guest agent, network checks, a report of the checker keepalive to the daemon (the Lease exists; `agent.proto` has no field for it yet);
- the generated `clientset/applyconfiguration` was not regenerated (the generator does not run on the Go 1.27 toolchain); the typed clientset used by the agent and the operator is unaffected.

## 1. What travels where

| Data | Secret? | Where it lives |
|---|---|---|
| Bootstrap list of a device (script, files, time limit) | yes | Secret `<device object>-bootstrap`, key `bootstrap.json` |
| Checks of a lab (parts, expected values, schedule, time limit) | yes | Secret `<lab>-checks`, key `checks.json` |
| Device environment (unchanged) | yes | Secret `<device object>-env` |
| Service command, access list | no | `Lab.spec.devices[]` (`spec_json`), copied to `Device.spec` |
| "This device has a bootstrap", "this lab has checks" | no | `spec.devices[].bootstrap.enabled`, `spec.checks.items[] {id, device, manualRun}`, set by the agent only |
| Bootstrap state, check results | no | status, see section 3 |

`<device object>` is the name of the Device CR (`internal/devices.Name(lab, device)`, the same name the `-env` Secret uses). `<lab>` is the Lab CR name. All three Secrets are owner-referenced by the Lab (garbage-collected with it), labelled `laboratory.cybericebox.com/lab` (and `/device` for a device Secret). The agent only creates and deletes them, never reads them; no read call (`ListLabs`, `Monitoring`, the CR) returns a value.

Bootstrap and checks travel in the same write as the environment: under the same variable-write claim (`AnnotationLabVariableAdmission`), with the same rules. A lifecycle-managed lab that already exists keeps its Secrets on a resend; for any other existing lab a resend rewrites them (delete then create), exactly as the `-env` Secrets.

## 2. Protobuf (additive, `agent.proto`)

Call and request changes:

| Where | Field | Meaning |
|---|---|---|
| `LabVariant` | `repeated DeviceBootstrap bootstrap = 5` | bootstrap lists common to the variant |
| `LabVariant` | `LabChecks checks = 6` | checks common to the variant |
| `LabItem` | `repeated DeviceBootstrap bootstrap = 10` | the lab's own lists; replace the variant's list of the same device |
| `LabItem` | `LabChecks checks = 11` | the lab's own checks; when present they replace the variant's as a whole |
| `LabChanges` | `repeated DeviceBootstrap bootstrap = 4` | rewrite the Secret of every listed device (empty list removes it) |
| `LabChanges` | `LabChecks checks = 5` | rewrite the checks Secret |
| service | `rpc RunChecks(RunChecksRequest) returns (BatchResult)` | manual run, section 5 |

New messages: `DeviceBootstrap {device=1, script=2, interpreter=3, repeated BootstrapFile files=4, time_limit_seconds=5}`, `BootstrapFile {path=1, content=2, owner=3, group=4, mode=5}`, `LabChecks {repeated Check items=1}`, `Check {id=1, device=2, CheckMode mode=3, schedule_seconds=4, manual_run=5, time_limit_seconds=6, repeated CheckPart parts=7}`, `CheckPart {oneof observation {FileObservation file=1; CustomObservation custom=2}}`, `FileObservation {path=1, optional exists=2, optional content_contains=3, optional content_matches=4, optional mode=5, optional owner=6, optional group=7}`, `CustomObservation {repeated command=1, script=2, interpreter=3, user=4, optional output_contains=5, optional output_matches=6}`, `RunChecksItem {lab_group=1, lab=2, repeated check_ids=3}`, `RunChecksRequest {repeated items=1}`.

Status messages and fields: `BootstrapStatus {BootstrapPhase phase=1, attempts=2, reason=3}` as `LabDeviceStatus.bootstrap = 32`; `CheckResult {id=1, CheckState state=2, first_passed_unix_ms=3, changed_unix_ms=4, value=5, detail=6}` as `LabStatus.checks = 44`; `LabStatus.checks_run_ack = 45`. Enums: `CheckMode`, `CheckState` (`UNKNOWN` = 0, `PASSED`, `NOT_PASSED`, `ERROR`), `BootstrapPhase` (`NONE` = 0, `PENDING`, `DONE`, `FAILED`).

Features: `FeaturesResponse.checks = 17` (`ChecksFeature {enabled, max_checks_per_lab, max_parts_per_check, max_time_limit_seconds, max_schedule_seconds, max_detail_bytes}`), `terminal = 18` (`TerminalFeature {enabled}`), `bootstrap = 19` (`BootstrapFeature {max_script_bytes, max_files, max_file_bytes, max_total_bytes, max_time_limit_seconds, max_attempts}`). The same message is carried in `MonitoringUpdate.features`.

**Device access (2026-11-12, owner decision; docs/specs/2026-11-12-device-access-and-checks.md in the platform repo).** The device's `exposure` (a web port) and `terminal` are removed, with no compatibility. One list `access` replaces them, in `spec_json`, in `DeviceTemplate`/`DeviceSpec`: `access[] {kind: http | terminal | vnc | rdp, port, protocol, command[], user}`, at most one entry of each kind (a map list keyed by `kind`). `http`: `port` 1..65535, `protocol` http (the default) or https. `terminal`: `command` (empty = `/bin/sh`), `user` (empty = the image's user), no port. `vnc`, `rdp`: `port` only; accepted and stored, served by nothing, `features.access.vnc/rdp` is false. Everything that read `exposure.web` (L7 proxy routes and Services, readiness by port, `LabStatus.access` URLs, the access port of the pod) reads the `http` entry. Only a container device can have a terminal.

Features: `FeaturesResponse.terminal = 18` is removed (the number and the name are reserved); `FeaturesResponse.access = 21` is `AccessFeature {http, terminal, vnc, rdp}`: http is always true, `terminal` is true only when `terminal.enabled` is on AND the component that serves it exists (the agent's `Features.TerminalServed`), vnc and rdp are false. `AccessFeature.terminal_host = 5` is removed (the number and the name are reserved; it never left the branch, so the contract version stays 1.5). A lab with a terminal device is still refused by the option alone.

Terminal access entry: `LabAccessEntry` with `protocol = "terminal"`, `port = 0` and `url` = the host of the console of that device, `https://<device>-<code>--term.<endpoints.labs_domain>`. Every access through the proxy is addressed by a host name alone, never by a path or a query. The label is `names.AccessHostLabel(device, code, kind)`: the web host label `<device>-<code>` (the code is the device's own, `Device.spec.code`, unique in the group namespace) plus the fixed mark `--term`; the marks of the reserved kinds are `--vnc` and `--rdp`. The agent fills `url` from the same source as the web host of a http entry and never recomputes it in the caller; it is empty until the device has its code, and then the caller shows no link. The marked label cannot be a web host (a web host ends in a hyphen and 3-4 letters or digits) and no new device name may end in `--term`, `--vnc` or `--rdp`; a site tells a console host from a device web host by the suffix of the first label alone, and frames only the first. It is not added to `LabStatus.access_urls`. The link opens through the handoff of the platform for that host label like a device link.

There is no typed proto message for the device spec; it travels as `spec_json` (the `DeviceTemplate` shape), so the service command and the access list are spec fields.

## 3. CRD fields

`DeviceTemplate` and `DeviceSpec` (same names in both):

- `service {command[], args[], user, workingDir}`: from the image configuration, set by the daemon; empty = not provided (the image decides). Container devices only.
- `access[]`: see "Device access" above (the terminal is the `terminal` entry; command empty = `/bin/sh`, user empty = the image's user).
- `bootstrap {enabled}`: set by the agent when it received a non-empty list for the device (a client's value is dropped). The list and its time limit are only in the Secret.

`LabSpec.checks {items[] {id, device, manualRun}}`: set by the agent when it received checks. Mode, schedule, time limit and parts are only in the Secret.

`DeviceStatus.bootstrap` and `LabStatus.devices[].bootstrap` (`DeviceBootstrapStatus {phase none|pending|done|failed, attempts, reason<=256}`): written by the launcher side later; the lab controller copies the device value into the lab's `devices[]`.

`LabStatus.checks[] (CheckResult)`, map-list keyed by `id`: `state unknown|passed|not_passed|error`, `firstPassedMs`, `changedMs`, `value<=256`, `detail<=2048`. `LabStatus.checksRunAck` (int64).

`AccessEntry.url` is optional now (a terminal entry has none).

**Only changes are published.** The checker patches one `checks[id]` entry only when its state, value or detail changed. Every Lab status write already reaches the `Monitoring` stream as a lab delta; an unchanged result adds no write. The monitoring projection sorts results by id so equal statuses are byte-equal. A state `error` means "could not be made"; the consumer keeps the state of the previous change.

## 4. Secret documents (key layout)

`bootstrap.json` (`labchecks.Bootstrap`): `{"script","interpreter","files":[{"path","content"(base64),"owner","group","mode"}],"timeLimitSeconds"}`. Defaults are filled in by the agent: interpreter `/bin/sh` (only with a script), mode `0644`, time limit 300 s. The script text is not interpreted by the agent; values are already filled in by the daemon.

`checks.json` (`labchecks.ChecksDoc`): `{"checks":[{"id","device","mode":"once|continuous","scheduleSeconds","manualRun","timeLimitSeconds","parts":[{"file":{...}}|{"custom":{...}}]}]}`. Defaults: time limit 30 s, custom user `root`, script interpreter `/bin/sh`. Optional expectations are pointers: absent = not checked. Go types: `internal/labchecks`.

Order of expectations (for the checker, not enforced here): all parts of a check must hold in the same run; a file with `exists=false` has no other expectation; a custom part passes on exit code 0 and then on the optional output expectations.

## 5. RunChecks: how the request reaches the checker

`RunChecks` takes `(lab_group, lab, check_ids[])` items and answers acceptance per lab with `BatchResult` (`UPDATED` = recorded). Refused per lab (`FAILED`): the lab has no checks, an id is unknown or not `manualRun`, no check allows a manual run, the lab is retired or terminating. `NOT_FOUND`: no such lab. Refused as a whole (`FAILED_PRECONDITION`): the checks option is off.

Cooldown wait is a number, not only text. A manual run inside the cooldown after the previous one is refused with `RESOURCE_EXHAUSTED` (text for people: "wait N seconds before the next manual run of this lab") and the status carries the standard detail `google.rpc.RetryInfo{retry_delay}`: the least time to wait, rounded up to a whole second, never below the exact wait. Because a batch answer drops status details, the same value is in the per-lab result: `ItemResult.retry_after` (`google.protobuf.Duration`, field 8), with `retryable = true` and the machine reason `COOLDOWN` (`client.ReasonCooldown`). `retry_after` is set only for the cooldown; a caller must not parse the text. Other numbers: the ceiling on the time limit is already a number in `GetFeatures` (`checks.max_time_limit_seconds`) and its refusal is a fixed `INVALID_ARGUMENT`; the per-lab "busy" refusal has no number in it (the caller retries later).

Mechanism: the agent writes the annotation `laboratory.cybericebox.com/run-checks` on the Lab: `{"token":N,"ids":["a","b"]}`.

- `token` grows by one per accepted request: `max(previous token, status.checksRunAck) + 1`, written with an optimistic update (retry on conflict), so concurrent calls never share a token.
- `ids` empty = every check with `manualRun`. A request that arrives before the checker took the previous one (`previous token > status.checksRunAck`) is merged into it (union; "all" wins), so no request is lost.
- The checker watches the Lab, runs the listed checks, and writes the token it took into `status.checksRunAck`. Whether a run happened is read from the check results, not from the RunChecks answer.

## 6. Options and refusals

Helm values (both default `false`, independent): `checks.enabled`, `terminal.enabled`. They render as `CHECKS_ENABLED` and `TERMINAL_ENABLED` in the shared env map (the agent deployment and the operator ConfigMap read the same values); the agent reports them in `GetFeatures`/`Monitoring`. No DaemonSet is added by this step (the checker came with the next one: `docs/specs/lab-checker.md`; `features.checks.enabled` then also needs the DaemonSet to exist).

A lab that carries checks while `checks` is off, or a terminal device while `terminal` is off, is refused at `CreateLabs` with `INVALID_ARGUMENT`. `UpdateLabs` with checks and `RunChecks` are refused while `checks` is off. Bootstrap does not depend on either option. The refusal applies when the agent runs with its features set (always in production).

## 7. Validation and limits (`internal/labchecks`)

Refused with `INVALID_ARGUMENT` (the whole call, like a bad environment): a bootstrap or check that names an unknown device or a device that is not a container; duplicate check ids; invalid regular expressions (RE2); an empty observation; a file observation with no expectation, or `exists=false` with others; a path that is not absolute and clean; both or neither of command and script; limit violations.

| Limit | Value |
|---|---|
| script | 64 KiB |
| file content | 256 KiB |
| files per device | 32 |
| script + all file contents of a device | 512 KiB |
| bootstrap time limit | 1..3600 s (default 300) |
| bootstrap attempts (for the launcher) | 3 |
| checks per lab | 64 |
| parts per check | 8 |
| checks document | 512 KiB |
| check time limit | 1..600 s (default 30) |
| schedule | 0..86400 s, not below the time limit |
| check id | 1..64 characters |
| path / pattern / expected string | 4096 / 1024 / 4096 bytes |
| command items | 64 of 4096 bytes |
| result detail / value | 2048 / 256 bytes (cut on a rune boundary) |
| service command / args items | 256 of 4096 bytes |
| terminal command items | 32 |

`spec_json` still rejects `env`/`flags` on devices. `bootstrap` and `checks` in `spec_json` are accepted but dropped: only the agent sets them.

## 8. Updates

`UpdateLabs` can rewrite a bootstrap list only for a device that had one at creation (`bootstrap.enabled`), and checks only for a lab that had checks, with the same ids, devices and `manual_run` flags (the CR spec is immutable). An empty bootstrap list removes the Secret. Everything else (expected values, parts, schedule, time limit) may change.

## 9. Generated files a consumer takes

`pkg/agent/protobuf/agent.pb.go`, `agent_grpc.pb.go`, `agent.proto`, `pkg/agent/client` (`ContractMinor = 5`), `pkg/agent/protobuf/contract.lock`. CRDs: `config/crd/bases/` and `charts/laboratory/crds/` (`devices`, `labs`), and `api/laboratory/v1alpha1` with its deepcopy. The agent image label `com.cybericebox.laboratory.api-contract` is `1.5`.

## 9a. Connections from the lab to the participant (`vpn.reverseConnections`)

Added to 1.5 (unreleased, no version bump). Owner decision 2026-11-11: a lab must not be able to START a connection to a VPN client (a participant's machine) unless the author asked for it.

- **Field:** `Lab.spec.vpn.reverseConnections` (bool, default false), in `spec_json` of the variant like the rest of the lab spec. False: the VPN gateway passes from the lab to a client only replies to flows the client started (TCP, UDP, ICMP and the ICMP errors that belong to such a flow). True: as before this change. Connections from a client to the devices and between devices are never affected. The exercise key is `topology.vpn.reverse_connections`.
- **Validation:** `vpn.reverseConnections` needs `vpn.enabled` (CRD rule and `CreateLabs`: `INVALID_ARGUMENT`); on `internet` it is refused. Like the rest of the spec it is fixed at creation (`UpdateLabs` does not change it).
- **Capability:** `FeaturesResponse.vpn` (field 20, `VPNFeature{reverse_connections}`): true only when this agent and its VPN pods enforce the default (both gates) and accept the switch; derived from the rule generators, never a constant. Absent (an older agent): the field is refused by the agent (unknown JSON key), and the caller must not promise the participant that the lab cannot call back.
- **Status:** `LabStatus.vpn_lab_initiated_blocked` (field 46) is true only when the switch is off and the group's VPN gateway confirmed the block (`LabVPN.status.labInitiatedBlocked`). A lab whose group still has a VPN pod with an older program is reported false, never blocked on the spec alone.
- **Existing labs after an upgrade:** the absent field means false, so a lab created before the change is blocked as soon as its group's VPN pod runs the new gateway program. A lab that relied on a reverse shell stops working until it is recreated with the switch on.

## 10. To verify on a real node (micro-VM / Kata and cluster behaviour)

Built and unit-tested off-cluster; not verified without a Linux node with Kata:

- the memory emptyDir (`medium: Memory`) of a micro-VM pod lives in guest memory, is writable by the guest launcher and is not visible to the host after the pod ends;
- the Secret volume of the stager init container reaches the guest through the runtime's file sharing (virtiofs/9p), although it is not mounted in the main container: check that nothing of it is readable from the main container, and what the host side keeps;
- the termination message (`/dev/termination-log`) and the exit codes 78 and 79 reach the Kubernetes pod status from a Kata guest;
- exec probes (startup every 2 s during the bootstrap, readiness every 2 s) in a micro-VM: the load they add, and that they run as the (non-root) service user and can read `/.cib/boot/ok`;
- `syscall.Setgroups/Setgid/Setuid` and `exec` as the service user inside a user namespace (`hostUsers: false`) and in the micro-VM; that process 1 after exec is the service;
- the writable layer of a container restarted in place is fresh (containerd), and image `VOLUME` paths do not keep data across the restart;
- the kubelet's restart back-off (10 s, 20 s, ...) adds to the launcher's own pauses between attempts;
- `HOME` handling: the launcher replaces `/root` with the service user's home; a device whose image or env Secret sets `HOME=/root` explicitly for a non-root user would get the user's home instead.

## 10. Built-in observations (2026-11-12)

A check part is exactly one of eight observations. `file` and `custom` are as before (see 4) with additions; the six new ones read the state of the device **themselves**: the in-device program (`probe check-observe`) executes nothing of the device, no shell, no tool, no library of the image. The expected values stay in the checker; the program is asked where to look (a path, a port, a sysctl name, a user and a path, a destination) and answers with facts, bounded in size.

Proto (`CheckPart` oneof, numbers 3..8): `DirectoryObservation`, `ProcessObservation`, `PortObservation`, `KernelParamObservation`, `RequestObservation`, `PermissionObservation`; `FileObservation` gets `type = 8` and `checksum = 9`. Names follow the exercise schema 1.0, section 4.2.

| Observation | Keys (optional unless said; defaults are filled in by the agent) |
|---|---|
| `file` | `path`, `exists`, `type` (file/directory/symlink, link not followed), `content_contains`, `content_matches`, `checksum` (sha256 hex of a regular file of at most 256 KiB), `mode`, `owner`, `group` |
| `directory` | `path`, `exists` (true), `mode`, `owner`, `group`, `has_entries[]`, `lacks_entries[]` (at most 64 names each, a name has no `/`) |
| `process` | `name` (the command without its directory) and/or `matches` (regexp on the command line), at least one; `user`; `running` (true) |
| `port` | `port` 1..65535, `protocol` tcp (default) or udp, `listening` (true), `address` (an IP; a listener on the wildcard counts), `process` (the socket's holder) |
| `kernel_param` | `name` (sysctl with dots), `equals` and/or `matches`, at least one |
| `request` | `protocol` tcp / http / https, `host` (127.0.0.1), `port`; http(s): `method` GET (default) or HEAD, `path` (/), `status` (200), `body_contains`, `body_matches` (GET only). No redirect is followed, https is not verified, the body is read up to 64 KiB, proxy variables are ignored |
| `permission` | `user`, `path`, and at least one of `read`, `write`, `execute`; each given boolean must equal the real answer. Tested with `faccessat(AT_EACCESS)` in a child that runs as the user (groups from `/etc/group`); nothing is opened for writing, nothing is changed. A missing path is `not_passed` |
| `custom` | as before; `user` defaults to **nobody** (uid 65534 when the image has no such name); any other user, root included, is the author's explicit choice. The command gets a clean environment (`PATH` fixed, `HOME`, `LC_ALL=C`, `USER`/`LOGNAME`) and is looked up in that PATH; its working directory is `/` |

**What the program is asked, and what it returns.** One rule for all eight observations: the program receives only WHERE to look, and returns FACTS; every comparison is made in the checker. Nothing the author writes as an expectation is sent to the device (no `contains`, `matches`, `checksum`, `equals`, entry names, process name / pattern / user, address or owner process of a port, status, body rule, mode, owner, group, `running`, `listening`, `read`/`write`/`execute`; `output_*` of a command stays in the checker too). The request is visible in the device's `/proc` to a participant who is root, and an expected value may come from a `{{task.}}` substitution. A test inspects the exact bytes sent for each kind (`TestNothingThatIsExpectedIsSentToTheDevice`).

| Kind | Sent to the program | Returned (bounded) |
|---|---|---|
| `file` | path, whether the content is wanted | exists, type (lstat), mode, owner, group, regular, the content up to 256 KiB (the checker compares `contains`, `matches`, computes the sha256 for `checksum`) |
| `directory` | path, a random 32-byte key (new for every question) | exists, mode, owner, group, and the HMAC-SHA256 of every entry name under that key, cut to 16 bytes, up to 32768 entries; the checker hashes the names it expects with the same key. More entries = truncated |
| `process` | nothing | the process table: pid, comm, first word, command line (512 bytes, `cut` says it was longer), uid, user name; up to 2048 processes, `truncated`, and `incomplete` for entries that could not be read |
| `port` | port and protocol | the sockets of that port from `/proc/net`, read in full (a line that cannot be read is an error): address, holder processes; up to 256, `truncated`; `ownersIncomplete` when some process could not be looked into |
| `kernel_param` | the sysctl name | the value (up to 4096 bytes) |
| `request` | host, port, protocol, method, path, time limit | connected or the failure class; status; the body up to 64 KiB |
| `permission` | user and path (that is the question) | three booleans and whether the path exists |
| `custom` | the command or script, the user, the time limit (it IS what is executed) | exit code, signal, output up to 64 KiB |

**The rule for every observation: a cap hit, a read error, a timeout or an unreadable part is undecided (the result `error`, the previous state is kept), never `passed`, in the positive and the negative form alike** (`exists: false`, `listening: false`, `running: false`, `lacks_entries`, `read: false`). Content that was cut at its cap decides nothing, found or not (an anchor in a pattern would fit the cut): `contains`, `matches`, `checksum` of a file over 256 KiB, `output_*` of a command over 64 KiB, `body_*` of a response over 64 KiB are `error`; whatever else of the part can be judged still is, and a definite failure of another key is `not_passed`. A request that times out, or fails without a name, is `error`; a refusal, an unreachable network, a name that does not resolve and a failed TLS handshake are facts about the service (`not_passed`).

Bounds are in the table above. A directory of more than 32768 entries, a process table of more than 2048, a command line of more than 512 bytes (for `matches` only), a port of more than 256 sockets, a body of more than 64 KiB, a file of more than 256 KiB are undecided where the cut matters (what was seen stays seen: a process that matched is found, a forbidden entry that was listed is present, a listener that fits is a pass).

A file expectation of `type` or `checksum` is read by the in-device program on every device (the node-side reader follows links and cannot tell what the path itself is).

Features: `ChecksFeature.observations = 7` lists the kinds the checker runs: file, directory, process, port, kernel_param, request, permission, custom. A kind that is not listed is not run.

Today the user of a `custom` check can be switched only when the container's own user is root (the runtime's exec starts the tool as the image's user); with another user the result is `error` ("the in-device tool does not run as root"). The device access service will start the tool as root.
