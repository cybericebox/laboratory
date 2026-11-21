# Contributing

`main` holds what is in production, `develop` is the integration branch, `release/X.Y` is a release line. Nobody pushes to any of them directly (branch protection applies to admins too): every change is a branch and a pull request. `main` only receives promoted lines, through the pull request that Promote opens.

## The cycle

1. `mise run dev-start <x>`: fetches `develop` and creates `feature/<x>` from it.
2. Work and commit in that branch.
3. `mise run dev-push`: pushes the branch (it refuses on `develop`, `main` and `release/*` and warns about uncommitted changes), opens a PR into `develop` or updates the open one, and turns on auto-merge: the PR merges by itself once every check is green.
4. After the merge GitHub deletes the remote branch. `mise run dev-done` switches to `develop`, pulls it and deletes the local branch.

The tasks use `scripts/dev.sh` and need the `gh` CLI, logged in.

### A fix for a release line

A line takes fixes only. Start from the line, not from `develop`:

```bash
mise run dev-start <x> --line 1.10   # fix/1.10/<x> from the fresh release/1.10
# work, commit
mise run dev-push                    # opens the PR into release/1.10; NO auto-merge
```

The PR needs green `Check passed` and `Line gate` (below). **The owner merges it by hand** in GitHub when they are green; `dev-push` never turns on auto-merge for a PR whose base is `release/*`. After the merge `mise run dev-done` deletes the branch. The merge builds the next candidate and the Backport workflow opens the same change as a PR into `develop` (that one merges by itself like any other).

## Checks

- A PR into `develop` or into a line runs `check.yml`: `test.yml` (the release helpers, generated files are current, unit tests, lint, the iptables tests in network namespaces, the eBPF tests and the reproducibility of the eBPF objects, the e2e test) and the Docker build, which only reads the registry layer cache `cybericebox/laboratory-<name>:buildcache-release` (written by candidate builds) and writes and publishes nothing: a check runs the code of the PR and holds no credential that can write. A new push cancels the previous run; a PR that is moved to another base is checked again (`edited`). The required check is `Check passed`.
- A PR into a line also runs `line-gate.yml`, the required check `Line gate`. A change may enter a line when it touches none of the harness (`.github/**`, `mise.toml`, `mise-tasks/**`, `hack/**`, `scripts/**`, `.golangci.yml`) and is at most 400 changed lines (added plus deleted; generated files, the chart CRD copies and `go.sum` are not counted). The label `large-fix`, which the owner sets in GitHub for a justified larger fix, waives the size limit only. There is no title convention. The gate runs from the default branch (`develop`; `pull_request_target` always takes the workflow and the checkout from there, not from the line), reads the changed files through the API and runs nothing of the PR; it also protects every other place where mise reads configuration (`.mise/`, `.config/mise/`, `mise.<env>.toml`, `.tool-versions`) and counts a file named like a generated one unless it sits where the generator writes; it refuses changes to the harness, so a PR cannot edit its own gate or checks. A change to CI goes into `develop` and reaches a line with the next cut; when the CI of a line itself is broken the owner merges the fix with the ruleset bypass.
- A merge into `develop` publishes nothing: no image, no tag. `Build image` (`build.yml`, manual) builds any commit, by default the `develop` head, and publishes its `sha-<7>` for an ad-hoc check. When that image exists it is not rebuilt.

## Tools

Every command runs through mise (`mise install` once, then `mise run <task>`; `mise tasks` lists them with the environment variables each reads). A short command is a task in `mise.toml`, a longer one an executable script under `mise-tasks/` (a subdirectory is a group: `mise-tasks/image/build` is `image:build`); keep them shellcheck-clean and give each a `#MISE description`. Where a tool is pinned:

- **A Go tool of the module goes into `go.mod` as a `tool` directive**: `controller-gen`, `setup-envtest` and `bpf2go`. They run as `go tool <name>`, built by the toolchain of `go.mod`, at the version `go.mod` records. Add one with `go get -tool <package>@<version>`, bump it with `go get -tool <package>@<version>`, then `go mod tidy` and `mise run generate-check`. A bump of `controller-tools` changes the version line of every CRD, so commit the regenerated files with it.
- **Everything else goes into `mise.toml`**: Go itself (the same version as the `toolchain` line of `go.mod`; the `golang` images in `Dockerfile` and `hack/*.sh` are pinned to it by tag and digest, and `hack/generate-bpf.sh` keeps the Debian snapshot date of its image), kind, kubectl, helm, kustomize, protoc and its Go plugins, zot, gh. `golangci-lint` is also in `mise.toml` (built with the pinned Go through the `go:` backend), not in `go.mod`: its authors advise against making it a module dependency, and it would pull its whole dependency tree into the module.
- The code generators of `hack/update-codegen.sh` (`client-gen`, `lister-gen`, `informer-gen`, `applyconfiguration-gen`) are not tool directives: `kube_codegen.sh` of `k8s.io/code-generator` builds them itself with `go install`, at the version of `k8s.io/client-go` in `go.mod`, into `bin/codegen` (ignored).
- Nothing is installed by hand and nothing a task writes goes outside `bin/`, `dist/` (the chart package of the release) and the generated directories (`.gitignore`).

## How to change things

Run each step through `mise run`. `mise run generate` regenerates every generated file (CRDs and RBAC from the markers, the chart's CRD copies, deepcopy, the clientset with listers, informers and apply configurations, the gRPC code); never edit those files by hand. `mise run generate-check` regenerates and fails, printing the files, when the tree is not clean afterwards: commit your change first, then run it.

### A field of a custom resource

1. Edit the type in `api/<group>/v1alpha1/*_types.go`, with its `json` tag, comment and `+kubebuilder:validation` markers.
2. `mise run generate`. For a field of `PoolSpec` this changes `api/allocation/v1alpha1/zz_generated.deepcopy.go` (pointers, maps and slices only), `config/crd/bases/<group>_<plural>.yaml`, its copy `charts/laboratory/crds/`, and `clientset/applyconfiguration/<group>/v1alpha1/<kind>spec.go`.
3. Use the field in the code, then run `mise run test` (the envtest suites of `api/` apply the CRDs to a real API server, `test/chart` renders the chart). Run `mise run test-netns` if the field reaches the VPN or the gateway.
4. If the agent returns or accepts the field, change `agent.proto` too (see below, the contract version rule). Chart RBAC changes with the code that needs it, in `charts/laboratory/templates` (`test/chart/rbac*_test.go`).

### A new custom resource kind

1. Create `api/<group>/v1alpha1/<kind>_types.go` with the spec, the status, the kind and its list. Above the kind: `// +genclient` (and `// +genclient:nonNamespaced` with `// +kubebuilder:resource:scope=Cluster` for a cluster-wide kind) and `// +kubebuilder:object:root=true`; above the list `// +kubebuilder:object:root=true`; in `init()` `SchemeBuilder.Register(&Kind{}, &KindList{})`. Copy `maintenancewindow_types.go`.
2. `mise run generate`. New files: `config/crd/bases/<group>_<plural>.yaml`, the same in `charts/laboratory/crds/`, and `clientset/applyconfiguration/.../<kind>.go`, `<kind>spec.go`, `clientset/client/.../<kind>.go` and its fake, `clientset/informers/.../<kind>.go`, `clientset/listers/.../<kind>.go`; the deepcopy and the `*_client.go`, `generic.go`, `expansion` files change.
3. Add `bases/<group>_<plural>.yaml` to `config/crd/kustomization.yaml` (the manager e2e suite installs the CRDs from it). For a controller, add `+kubebuilder:rbac` markers to it (`mise run generate` writes `config/rbac/role.yaml`) and the same rules to the chart's ClusterRoles in `charts/laboratory/templates`; `test/chart/rbac*_test.go` check them.
4. Tests: `mise run test`.

### agent.proto and node_agent.proto

1. Edit `pkg/agent/protobuf/agent.proto` or `pkg/rpc/node/v1/node_agent.proto`, then `mise run proto` (`mise run generate` runs it as well). The `*.pb.go` and `*_grpc.pb.go` files change.
2. `agent.proto` has a contract version. `go test ./pkg/agent/client -run TestContractLock` fails with the new line for `pkg/agent/protobuf/contract.lock`. Bump `ContractMinor` (additive change) or `ContractMajor` (a break) in `pkg/agent/client/contract.go`, set the same version in the `com.cybericebox.laboratory.api-contract` label of the `agent` target in `Dockerfile` (`TestDockerfileCarriesTheContract`), and write `<version> <fingerprint>` into `contract.lock` (the fingerprint is the one the failed test printed; it does not depend on the version). Run `go test ./pkg/agent/client` again.
3. `node_agent.proto` is internal between the node-agent and cni-gate, which ship in one image: no version, no lock. Build and run `mise run test`.

### A chart value

1. Add the key with a comment to `charts/laboratory/values.yaml` and use it in `charts/laboratory/templates`. Every option has a default there; do not add a value an installation must give unless `templates/validate.yaml` checks it.
2. Add or extend a test in `test/chart` (they render the chart with the pinned `helm`): `mise run test`, or one test with `mise exec -- go test ./test/chart -run <Name>`.
3. Describe the value in `DEPLOY.md`.
4. `../infrastructure` reads the values: its `mise run check-config-drift` compares `values.yaml` with `docs/optional/platform/laboratory.env` and its templates. After the change run it there (`mise run check-config-drift -- --chart <this repository>/charts/laboratory/values.yaml`) and, if it reports a missing key, `mise run gen-laboratory-env` with the same `--chart` argument; that change is made in the infrastructure repository.

### Before `mise run dev-push`

Run the checks CI runs, in this order, and push when all are green:

```bash
mise run check                                   # generate-check, lint, test (commit your change first: generate-check needs a clean tree)
mise run test-netns                              # Docker
mise run test-bpf                                # Docker; after a change of wg_fastpath.bpf.c also: mise run generate-bpf and commit the objects
mise run cluster-up && mise run e2e              # Docker; then: mise run cluster-down
```

After Docker work remove what it left: `docker image prune -af`, `docker builder prune -af`, `docker volume prune -af`.

## Releases

A release is cut from `develop` into a line and travels line, candidates, final release. The commands are in `../infrastructure` (`mise run release ...`); the policy is `docs/internal-docs/infrastructure/specs/delivery.md`. Versions of the laboratory are its own, continuing 1.x: the line after 1.9 is 1.10.

| Step | Who | Workflow |
|---|---|---|
| 1. Cut `release/X.Y` from `develop` | the operator, `release start` | none: a new branch is a push, so step 2 starts |
| 2. Candidate `vX.Y.Z-rc.N` on every push to the line | automatic | `candidate.yml` |
| 3. The same change into `develop` | automatic | `backport.yml` |
| 4. Fixes into the line | a PR, merged by the owner by hand | `check.yml`, `line-gate.yml` |
| 5. Promote the candidate that passed staging | the operator, `release promote` | `promote.yml` |
| 6. Retention | automatic after 5, or by hand | `cleanup.yml` |

- **Candidates.** Every push to `release/X.Y` builds the five images tagged `vX.Y.Z-rc.N`, the Helm chart with version `X.Y.Z-rc.N` and appVersion `vX.Y.Z-rc.N` (pushed to `oci://registry-1.docker.io/cybericebox/laboratory`, so staging installs it from the registry), then creates the git tag and the GitHub pre-release. `Z` is 0 while the line has no final release, otherwise the newest final patch plus 1: the first candidate of a new line is `vX.Y.0-rc.1`, and after `vX.Y.0` is promoted the next merge gives `vX.Y.1-rc.1`. One run per line at a time, so two merges cannot take the same number; a candidate is built only for the head of the line.
- **Immutable tags.** Docker Hub keeps the tags that match `^v?[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$` unchangeable (`latest`, `sha-*` and `buildcache-*` are not). A version tag is never pushed twice: a re-run of a failed job finds the tag, checks that it is the same thing (the `org.opencontainers.image.revision` label of an image, the digest on a retag, the content of a chart) and carries on, and fails when it is different. Candidates are deleted only by retention.
- **Backport.** After each push to a line, `backport.yml` cherry-picks the pushed change onto `develop` and opens a PR (`backport/X.Y/<sha>`; auto-merge on) with the job's own `GITHUB_TOKEN` and no other credential. When it does not apply cleanly it opens an issue with the commands to do it by hand instead of failing silently. See "Pull requests that automation opens" for how its checks start.
- **Promote.** `promote.yml` (manual, input `rc`, `dry_run`) runs from `main` only. It refuses unless the candidate is the newest of its version, its commit is the result of a merged PR into the line (the first candidate of a line: a merged PR into `develop`), `Check passed` is green on that PR's head, and the line gate still passes; any failed lookup refuses. There is no review requirement. Then the same images get `vX.Y.Z` by digest (no rebuild; `latest` moves when it is the newest release), the chart is published as `X.Y.Z`, the GitHub release is created as a draft, gets the chart and is published last (releases are immutable, so a published one takes no asset; a re-run continues from whatever state it finds, `release.sh`), the git tag appears at the candidate's commit. Retention runs next. Last, `main_pr.sh` pushes the branch `promote/vX.Y.Z` at the candidate's commit and opens a pull request into `main` with `GITHUB_TOKEN`, with auto-merge (merge commit); its check is the light `Main gate` job of `check.yml` (the release exists and the head contains it), so the tests are not run again. Retention does not wait for that merge: the release is already public and candidates and old releases do not depend on `main`; only its last step (dev images of commits that `main` has) catches up on a later run. A re-run of Promote finds the branch and the open pull request and continues. If the pull request conflicts, `main` was changed by hand: the job fails (the release stays published) and its summary gives the commands (merge `origin/main` into `promote/vX.Y.Z`, push).
- **The line stays.** `release/X.Y` is not deleted at promote: it takes the patches. The next merge gives the next patch candidate.
- **Clusters and the chart** use exact tags only (`vX.Y.Z`, a candidate on staging, or a `sha-*` build for a test); `latest` is for outside users. The daemon picks up a laboratory release with `mise run dev-push LAB=vX.Y.Z` in daemon.

### Retention

The same rule applies to GitHub release pages, published images and chart versions, in every repo. `cleanup.yml` applies it right after each promote (`.github/scripts/retention.sh`); run it by hand (manual, `dry_run` is on by default) after a failure.

- Git tags of final releases are kept forever, so any version can be rebuilt from source.
- Release candidates (`-rc.N`): tags, release pages, images and chart versions are deleted right after the final release is promoted (and any left over from an earlier release).
- The newest minor keeps all its patch releases and images.
- Older minors keep only their latest patch. The other patches of a minor are deleted when the next minor is released.
- Besides the newest minor, the latest patches of the 10 previous minors are kept (release page and images). An older minor drops out when it is no longer among the last 10: its release page and images are deleted, the tag stays. There are no exceptions: production always runs a current version.
- Laboratory only: the releases below 1.10 (1.x before the lines) are history from before the policy and are not pruned.
- The dev images (`sha-<7>` of commits that are part of `main`) and the registry layer caches are deleted by the same run.

### Pull requests that automation opens

Events made with `GITHUB_TOKEN` do not start workflows, except `workflow_dispatch` and `repository_dispatch`. A pull request opened by Backport or by Promote does start a `pull_request` run of `check.yml`, but, according to the GitHub documentation, in an *approval-required* state: the pull request shows a banner and a user with write access presses **Approve workflows to run** (this behaviour was documented in May 2026 and is still rolling out; if the button is not there, close the pull request and reopen it, which a person's event always starts). The required check must come from a `pull_request` run: GitHub does not evaluate the checks of a `workflow_dispatch` run for a pull request, so dispatching `check.yml` on the branch would not satisfy `Check passed`, and `check.yml` has no dispatch trigger. After the check is green, auto-merge completes the pull request.

### Where each workflow definition comes from

The default branch is `develop`; `main` only receives promoted lines. `pull_request_target` takes the workflow and the checkout from the default branch (GitHub changed this on 2025-12-08), not from the base branch of the pull request. The Docker Hub write credentials exist only as secrets of three environments, each bound to branches: `candidate` (`release/*`), `release` (`main`), `build` (`develop` and `release/*`). No other credential exists: no GitHub App, no personal token.

| Workflow | Event | Definition comes from | Secrets | Can write access to `develop` alone change a release? |
|---|---|---|---|---|
| `line-gate.yml` | `pull_request_target` into `release/*` | `develop` | none | It changes the gate for every line, but only through a reviewed pull request into `develop`; Promote runs the gate again from its own checkout of `main`, so a weakened copy on `develop` cannot get a release past Promote |
| `check.yml`, `test.yml` | `pull_request` into `develop`, `release/*`, `main` | the merge commit of the pull request | none | No secret at all; the gate keeps `.github/**` out of a line |
| `candidate.yml` | push to `release/X.Y`, or manual | the line, or the chosen ref | environment `candidate` (`release/*` only) | No: a branch whose name matches `release/*` is needed, and only the owner may create one; the environment refuses any other ref |
| `backport.yml` | push to `release/X.Y` | the line | none (job token only) | No |
| `promote.yml`, `cleanup.yml` | manual | the chosen ref, `main` required (a first step stops any other ref) | environment `release` (`main` only) | No: a copy on `develop` gets no secret; its scripts and gate are the ones of `main` |
| `build.yml` | manual | the chosen ref (`develop` by default) | environment `build` (`develop` and `release/*`) | A run on `develop` can publish `sha-*` tags, which are not part of the release path and are never promoted |
| scheduled workflows | none exist | | | |

### Settings in GitHub that the workflows rely on

- Default branch: `develop`. `pull_request_target` (`line-gate.yml`) and the Run workflow button read their workflow from it, so every workflow must be on `develop` (and `promote.yml`, `cleanup.yml` and `.github/scripts` also on `main`, see Bootstrap).
- Environments (the only place with secrets; each holds `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` and nothing else): `candidate`, deployment branches `release/*` only, push permission is enough; `release`, deployment branch `main` only, Read/Write/Delete (retention deletes); `build`, deployment branches `develop` and `release/*`, push permission.
- Rulesets (not classic branch protection, so admins are bound too). `release/*`: creation, update and deletion only through the owner (nobody else may create a branch of that name: the environments trust the name), pull request required, no direct push, no force push, required checks `Check passed` and `Line gate` (the job names, they are stable), no required approvals (the owner works alone), merges by hand, no merge queue (the workflows do not run for `merge_group`). `develop`: pull request required, required check `Check passed`. `main`: what exists already (pull request required, required check `Check passed`, no force push, no deletion, no bypass actor): Promote needs nothing more, because it only opens a pull request.
- Actions: default workflow permissions read-only (already set), "Require actions to be pinned to a full commit SHA" (already set), approval for every pull request from outside collaborators (fork pull requests), **"Allow GitHub Actions to create and approve pull requests" on** (Backport and Promote open pull requests with the job token), and "Allow auto-merge" on (already used by `dev-push`).
- Docker Hub: tag immutability for the pattern above on the five image repositories and `cybericebox/laboratory` (the chart). Check once on a scratch repository whether an immutable tag can be deleted through the API: if it cannot, restrict the immutable pattern to final versions (`^v?[0-9]+\.[0-9]+\.[0-9]+$`), or retention will keep reporting every candidate as not deleted.
- Label `large-fix` (anyone with triage rights can set it: keep that role to the owner).
- A promote is started with `gh workflow run promote.yml --ref main -f rc=...`.
- Whoever has write access can create a check run named `Check passed` on any commit from a workflow of their own branch: Promote trusts the checks, so write access to the repository is write access to the release.

### Bootstrap

The scripts that decide are read from `main`, and the environment `release` only runs on `main`, so `promote.yml`, `cleanup.yml` and everything under `.github/scripts` (with `check.yml`, which runs the `Main gate`, and `mise.toml`) must be on `main` before the first promote. They get there once through an ordinary pull request `develop` into `main`: it is not a promote branch, so it gets the full check, and the owner merges it. After that `main` is updated only by the pull requests of Promote.

## The chart

A candidate or a release packages the Helm chart (`version` `X.Y.Z[-rc.N]`, `appVersion` `vX.Y.Z[-rc.N]`, which is the tag of every image), pushes it to `oci://registry-1.docker.io/cybericebox/laboratory`, and a release also attaches it to the GitHub release.

## API contract version

The agent API has a contract version (`pkg/agent/client/contract.go`, `ContractVersion`, "major.minor"). Callers (the daemon, the
infrastructure check) compare it with `client.Compatible`, not the image tag: a laboratory release that changes no call must not
stop a daemon. Any change of `agent.proto` fails `TestContractLock` until the version is bumped (minor for an additive change,
major for a break), the Dockerfile `agent` label is updated and `pkg/agent/protobuf/contract.lock` gets the new line.
