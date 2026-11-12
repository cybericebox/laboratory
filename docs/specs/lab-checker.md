# The lab checker

**Design record:** `docs/specs/2026-11-11-lab-checkers-bootstrap-devices.md` in the platform repository (sections 3.1, 3.2, 3.4, 3.5, 3.6, 3.6a, 5). **Data model:** `lab-checks-bootstrap-contract.md` in this directory. **Operation and rights:** DEPLOY.md, "Lab checks". This note is the component itself.

Code: `internal/checker` (logic, informers, writers), `internal/cmds/checkerd` (the process), `internal/probe/check_*.go` (the in-device side), chart `charts/laboratory/templates/checker`.

## 1. Shape

A DaemonSet `laboratory-checker` of its own, on the lab nodes only, rendered only while `checks.enabled` is true. Its process is `/node checker` of the node image. It takes no inbound connection: no listener, no metrics, no health port (the kubelet's probes run `/node checker-health live|ready`, which look at two files in an emptyDir). The pod template is a frame of named templates (`laboratory.checker.container`, `...extraContainers`, `...volumes`, `...extraVolumes`) so that a second container can be added without rewriting the first.

Inputs, all scoped to the node: pods by `spec.nodeName` (server-side field selector, label selector `lab` and `device`), Labs (cluster-wide, trimmed to a stub when a lab has no checks), the Secret `<lab>-checks` by one GET when the lab has a pod on the node and again after the agent rewrote it (see 4). No loop polls the API server; the only periodic work is the in-memory schedule (once per second) and local runtime calls.

## 2. One run

```
Tick (in memory)  ->  due, device ready, room under the node cap  ->  Job
Execute           ->  Backend.Open(pod): find the container by pod UID and name (CRI ListContainers + ContainerStatus)
                      RunCheck: every part in order under the check's time limit -> Run{outcome, detail}
                  ->  machine.apply: a change of state/value/detail?  ->  dirty
Flush             ->  one server-side apply per dirty check; then the acknowledgement of a manual run whose results are all written
```

- **Barrier.** No run at all unless the pod is Ready and, for a device with `bootstrap.enabled`, `status.devices[].bootstrap.phase == done`. A run that finishes after the device left the barrier (the pod restarted) is discarded. The previous result stays.
- **Schedule.** `scheduleSeconds == 0` is manual only. The first run comes within `min(10 s, schedule)` after the device became ready, at a point that is a hash of the lab UID and the check id (no random source, so tests are exact); after that the next run is `schedule` +-10 % later, the offset again a hash of the run count, so the phases of many checks drift apart. At most `checks.maxConcurrent` runs at once on a node, the earliest due first, a person's manual run before any scheduled one.
- **Modes.** `once` stops after the first pass (its `firstPassedMs` is set and never cleared); `continuous` keeps running.
- **All parts in the same run.** Parts run one after another inside the check's one time limit; the verdict is the conjunction. A part that is false decides (`not_passed`) even when another could not be made; otherwise a part that could not be made makes the run `error`.

## 3. The observations (file and custom; the built-in ones are in the contract note, section 10)

**file** (exists / absent, content contains, content matches a regexp (RE2, searched not anchored), mode (permission bits with setuid/setgid/sticky), owner, group; owner and group by number or by name from the device's own `/etc/passwd` and `/etc/group`).

- *Container device.* Read from the node. `RootFS` (`internal/checker/rootfs.go`) resolves the path inside the container's root directory: every step is `fstatat`/`openat`/`readlinkat` relative to a descriptor the checker holds, with no-follow flags; a symbolic link is read and its target resolved by the checker (absolute = the container's root; `..` never above the root, applied to the checker's own descriptor stack); at most 40 links; the last step is `openat(O_NOFOLLOW|O_NONBLOCK)` and the opened descriptor is checked to be the regular file that was looked at (same device and inode), so a link or a FIFO swapped in at the last moment is refused, not followed or waited on. Content is bounded to 1 MiB. Host user and group numbers are translated to the container's through `linux.uidMappings`/`gidMappings` of the runtime spec (a user namespace); a file whose owner is not in the mapping fails owner/group expectations. Paths below `/proc`, `/sys`, `/dev` are refused (`error`): the node sees only the container's root file system, not what the container mounts for itself, and the node's own `/proc` would answer for it. On Linux the opened file is also refused when it is on procfs, sysfs, cgroupfs and the like.
- *Micro-VM device.* The root file system is inside the guest, so `probe check-file` is run through the runtime's exec (Kata executes it in the guest) as the container's user. The tool follows links (the guest is the participant's own world).

**custom.** `probe check-exec <json>` through the runtime's exec on the node's socket (never the Kubernetes API). The runtime's exec cannot switch user, so the tool does it (it needs to be root in the container to). The script travels in the request and is run from a memory file. The tool enforces the time limit (process group kill) and caps the output at 64 KiB, and answers with one JSON document, so its own exit code never mixes with the command's. Exit 0 = passed, then the optional output expectations (output = stdout and stderr together).

## 4. Results and who writes them

`Lab.status.checks` is a map list keyed by `id`. The checker writes by **server-side apply of one entry at a time, under a field manager of its own for that check** (`lab-checker.<hash of the id>`), and `status.checksRunAck` under `lab-checker-ack`. Why: the lab controller updates the whole status with `Status().Update`; an apply of one entry owns only that entry and so never takes over, drops or reverts what the controller wrote, and the controller's updates never drop an entry (proved against a real API server in `status_envtest_test.go`, including parallel writers). The object applied is unstructured, never the typed Lab (a typed object would carry every non-omitempty field and claim it).

Rule for `error`, exactly:

| Situation | Written |
|---|---|
| no result published yet | `error` and its detail at once |
| `passed`/`not_passed` published, scheduled run fails | nothing, until `ErrorsBeforePublish` (3) scheduled runs in a row failed |
| 3rd failure in a row, or a failed manual run | `state=error`, `detail`, `changedMs=now`, `value` cleared, `firstPassedMs` kept |
| the next run that gives passed/not_passed | replaces the error |
| device not ready | no run, nothing written, failures in a row reset |

`detail` is the part number, its kind, and the expectation kind that did not hold (`part 2 (file): content_contains: not found`), plus observed facts that are no expected value (an exit code, a mode). It never holds an expected value, output or file content. A failure in the infrastructure gets a fixed text (`the container is not running`, `the in-device tool is missing or did not answer`, ...), never a path of the node.

**Manual runs.** The annotation `run-checks` `{"token":N,"ids":[...]}`: the checker keeps a pending request per lab; a newer token is merged into the pending one (ids united, "all" wins), tokens up to the acknowledgement seen when the lab was first noticed are done. It runs the listed checks (or all `manualRun` checks) of its own node, writes their results, then writes N into `status.checksRunAck`. The acknowledgement never overtakes a result: it is written only when every entry of the lab is written.

## 5. Keepalive

One Lease per node, `laboratory-checker-<node>` in the release namespace, label `laboratory.cybericebox.com/checker=true`, `renewTime` every 20 s, `leaseDurationSeconds` 60, annotations `laboratory.cybericebox.com/checker-state` and `.../checker-version`. Not one signal per check. To make it visible to the daemon through the existing stream, the agent would need: `list/watch` of Leases in `laboratory-system` with that label (a Role), and a new message in `agent.proto`, for example `message CheckerHealth { int32 nodes = 1; int32 stale = 2; }` as a field of `MonitoringUpdate` (and `unavailable_reason` in `ChecksFeature`). Not done here: `agent.proto` is untouched.

## 6. Features

`FeaturesResponse.checks.enabled` is true only while the option is on **and** the DaemonSet `laboratory-checker` exists in the release namespace (the agent does one GET by name, cached 30 s, keeps the last answer on an API error). A lab with checks is refused otherwise with the reason. A cluster with no lab node yet still has the DaemonSet, so it is not locked out.

## 7. What a Linux node alone can verify

See the report of the change; in short: the mount propagation of the containerd state directory, the layout `<state>/io.containerd.runtime.v2.task/<ns>/<id>/rootfs` for k0s, the host numbers of files of a container in a user namespace, `DAC_READ_SEARCH` being enough with the default AppArmor and seccomp profiles, the CRI exec of the in-device tool in runc and Kata containers, the switch of user in the tool.
