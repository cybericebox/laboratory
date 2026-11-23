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
- A PR into a line also runs `line-gate.yml`, the required check `Line gate`. A change may enter a line when it touches none of the harness (`.github/**`, `mise.toml`, `mise-tasks/**`, `hack/**`, `scripts/**`, `.golangci.yml`) and is at most 400 changed lines (added plus deleted; lock files such as `go.sum` and the generated files, with the chart CRD copies, are not counted: the explicit lists are in `line_gate.py`). The label `large-fix`, which the owner sets in GitHub for a justified larger fix, waives the size limit only. There is no title convention. The gate runs from the default branch (`develop`; `pull_request_target` always takes the workflow and the checkout from there, not from the line), reads the changed files through the API and runs nothing of the PR; it also protects every other place where mise reads configuration (`.mise/`, `.config/mise/`, `mise.<env>.toml`, `.tool-versions`) and counts a file named like a generated one unless it sits where the generator writes; it refuses changes to the harness, so a PR cannot edit its own gate or checks. A change to CI goes into `develop` and reaches a line with the next cut; when the CI of a line itself is broken the owner merges the fix with the ruleset bypass.
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

## Dependency updates

Dependabot opens version updates every Monday morning (Kyiv time), into the default branch (`develop`). Minor and patch updates are grouped: one pull request per ecosystem and kind per week. Major updates come as separate pull requests, so they stay visible. Config: `.github/dependabot.yml`; it names no `target-branch`, so security updates go to `develop` too.

- A pull request with only patch updates merges itself into `develop` once the required check is green (`.github/workflows/dependabot-auto-merge.yml`). That workflow acts only on pull requests whose base is `develop`: a pull request into `release/*` or `main` never gets auto-merge from it (a line is merged by hand, `main` is updated only by Promote).
- Anything with a minor or a major update waits for the owner: review it, fix what breaks, merge it.
- Security fixes come as one grouped pull request per ecosystem (the `*-security` group), not one per package. Critical and high ones are handled the same day. A security pull request with at most minor updates merges itself like a patch one; a major waits for the owner.
- Dependabot runs get no repository secrets and a read-only token. The checks of a pull request use no secret at all (not even a Docker Hub login: they pull public images anonymously), so a Dependabot pull request runs every required job like any other. A step that needs a secret must never be added to the checks.
- **Support window for security fixes.** A security fix is released as a patch on the **three newest minor lines** that are in production or supported (for example 1.12, 1.11 and 1.10). Only the patch of the newest line goes through staging before it is promoted; the patches of the older two are promoted after their candidates build and the checks and the line gate pass. A fix reaches the lines from `develop`: a pull request into `develop` with the label `security` is carried to the three newest lines automatically once merged ("Security fixes" under "Releases"). Dependabot's security pull requests do not get the label by themselves: the owner adds it (after the merge works too) when the lines need the update. A fix that cannot be carried gets an issue, and a person makes the pull request into that line (`mise run dev-start <name> --line X.Y`, see "A fix for a release line"). Such a pull request passes the line gate: a lock file (`go.sum`, `go.work.sum`, `Chart.lock`, other lock files at the places listed in `line_gate.py`) and generated files do not count towards the 400 lines, so a dependency patch fits; `go.mod` and the code do count.

## Releases

A release is cut from `develop` into a line and travels line, candidates, final release. The commands are in `../infrastructure` (`mise run release ...`); the policy is `docs/internal-docs/infrastructure/specs/delivery.md`. Versions of the laboratory are its own, continuing 1.x: the line after 1.9 is 1.10.

| Step | Who | Workflow |
|---|---|---|
| 1. Cut `release/X.Y` from `develop` | the operator, `release start` | none: a new branch is a push, so step 2 starts |
| 2. Candidate `vX.Y.Z-rc.N` on every push to the line | automatic | `candidate.yml` |
| 3. The same change into `develop` | automatic | `backport.yml` |
| 4. Fixes into the line | a PR, merged by the owner by hand | `check.yml`, `line-gate.yml` |
| 4a. Security fixes into the three newest lines | automatic PRs from a PR into `develop` with the label `security`, merged by the owner by hand | `security.yml` |
| 5. Promote the candidate that passed staging | the operator, `release promote` | `promote.yml` |
| 6. Retention | automatic after 5, or by hand | `cleanup.yml` |

- **Candidates.** Every push to `release/X.Y` builds the five images tagged `vX.Y.Z-rc.N`, the Helm chart with version `X.Y.Z-rc.N` and appVersion `vX.Y.Z-rc.N` (pushed to `oci://registry-1.docker.io/cybericebox/laboratory`, so staging installs it from the registry), then creates the git tag and the GitHub pre-release. `Z` is 0 while the line has no final release, otherwise the newest final patch plus 1: the first candidate of a new line is `vX.Y.0-rc.1`, and after `vX.Y.0` is promoted the next merge gives `vX.Y.1-rc.1`. One run per line at a time, so two merges cannot take the same number; a candidate is built only for the head of the line.
- **Immutable tags.** Docker Hub keeps the tags that match `^v?[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$` unchangeable (`latest`, `sha-*` and `buildcache-*` are not). A version tag is never pushed twice: a re-run of a failed job finds the tag, checks that it is the same thing (the `org.opencontainers.image.revision` label of an image, the digest on a retag, the content of a chart) and carries on, and fails when it is different. Candidates are deleted only by retention.
- **Backport.** After each push to a line, `backport.yml` cherry-picks the pushed change onto `develop` and opens a PR (`backport/X.Y/<sha>`; auto-merge on) with the job's own `GITHUB_TOKEN` and no other credential. The branches require signed commits, so the commit is not made with git on the runner: `git` computes the cherry-pick in a scratch clone (nothing of it is pushed) and the result is committed through the GraphQL mutation `createCommitOnBranch`, which GitHub signs (verified) as `github-actions[bot]`; the job checks that the commit has the tree of the cherry-pick and is verified, otherwise it deletes the branch, opens an issue and fails. That API sends whole files and deletions only: a file mode (an executable), a symlink, a submodule or a change above 10 MiB / 300 files cannot be sent, and gets an issue instead; a rename is a deletion plus an addition. When a pick does not apply cleanly it also opens an issue with the commands to do it by hand instead of failing silently. See "Pull requests that automation opens" for how its checks start.
- **Security fixes.** A security fix is made like any change: a pull request into `develop`, with the label `security`. Once it is merged (or when the label is added after the merge), `security.yml` carries it to the three newest release lines that exist (`release_version.py newest-lines`; older lines get nothing). For each line that does not already have the change (its commits are in the line, a commit of the line carries the same `Carried-from` trailer, or the pick is empty), `security.sh` cherry-picks the merge (`-m 1`; for a rebase merge, the newest commits of `develop`, as many as the pull request has of its own, each proven by its patch-id against `refs/pull/<n>/head`, otherwise nothing is carried, an issue says so and the job fails) onto the head of the line in a scratch clone, commits the result through `createCommitOnBranch` on `security/X.Y/<sha>` (verified, checked like the backport) and opens a pull request into `release/X.Y` with the label `security` (and `large-fix` when the source pull request had it). No auto-merge: the owner merges each one by hand, and the Line gate judges it like any fix (no harness files, 400 changed lines without `large-fix`). A pick that does not apply, or a change the API cannot express, gets an issue with the label `security` (once: a closed issue is not opened again) and the commands to do it by hand (a plain commit: no trailer, after the merge Backport finds the change already in `develop`, an empty pick, and opens nothing). A failed line does not stop the others; the job fails at the end and a re-run continues. The commit message ends with `Carried-from: <merge commit in develop>`: after the merge into the line, `backport.sh` sees that every commit the push brings has that trailer and a commit of `develop` behind it, and brings nothing back (no loop; a line-only commit merged together with it is still brought back). The trailer is honoured only on a commit that this workflow made in this repository: the API must show it verified (author `github-actions[bot]`, committer `web-flow`; a workflow of a fork can make that too, so it is not enough) and a pull request of the commit must come from a branch `security/X.Y/*` of this repository (a fork cannot create branches here). Anyone can write the text in a commit of a pull request, so a trailer anywhere else, or one that cannot be checked (the API fails), is ignored and the change is brought back. The pull requests start their checks like Backport's (below); the Line gate, a `pull_request_target` run, may not start for a pull request the job token opened: close and reopen it. Each merge builds the line's next candidate as usual; only the newest line goes through staging before its promote (staging is skipped for now), the older lines are promoted from their candidate directly.
- **Promote.** `promote.yml` (manual, input `rc`, `dry_run`) runs from `main` only. It refuses unless the candidate is the newest of its version, its commit is the result of a merged PR into the line (the first candidate of a line: a merged PR into `develop`), `Check passed` is green on that PR's head, and the line gate still passes; any failed lookup refuses. There is no review requirement. Then the same images get `vX.Y.Z` by digest (no rebuild; `latest` moves when it is the newest release), the chart is published as `X.Y.Z`, the GitHub release is created as a draft, gets the chart and is published last (releases are immutable, so a published one takes no asset; a re-run continues from whatever state it finds, `release.sh`), the git tag appears at the candidate's commit. Retention runs next. Last, `main_pr.sh` pushes the branch `promote/vX.Y.Z` at the candidate's commit and opens a pull request into `main` with `GITHUB_TOKEN`, with auto-merge (merge commit); its check is `Main gate` (`main-gate.yml`, `pull_request_target`, so the pull request cannot change it): the release tag exists and is part of `release/X.Y`, and the head IS the tagged commit or a clean merge of `main` into it with nothing else in it (`main_gate.py`), so the tests are not run again. Every other pull request into `main` fails it. Retention does not wait for that merge: the release is already public and candidates and old releases do not depend on `main`; only its last step (dev images of commits that `main` has) catches up on a later run. A re-run of Promote finds the branch and the open pull request and continues. If the pull request conflicts, `main` was changed by hand: the job fails (the release stays published) and its summary gives the commands (merge `origin/main` into `promote/vX.Y.Z`, push).
- **The line stays.** `release/X.Y` is not deleted at promote: it takes the patches. The next merge gives the next patch candidate. The three newest lines take security fixes (above).
- **Clusters and the chart** use exact tags only (`vX.Y.Z`, a candidate on staging, or a `sha-*` build for a test); `latest` is for outside users. The platform backend picks up a laboratory release with `mise run dev-push LAB=vX.Y.Z` in `../platform/backend` (the repository `cybericebox/backend`, named `daemon` until 2026-11-22).

### Retention

The same rule applies to GitHub release pages, published images and chart versions, in every repo. `cleanup.yml` applies it right after each promote (`.github/scripts/retention.sh`); run it by hand (manual, `dry_run` is on by default) after a failure.

- Git tags of final releases are kept forever, so any version can be rebuilt from source.
- Release candidates (`-rc.N`): tags, release pages, images and chart versions are deleted right after the final release is promoted (and any left over from an earlier release).
- The newest minor keeps all its patch releases and images.
- Older minors keep only their latest patch. The other patches of a minor are deleted when the next minor is released.
- Besides the newest minor, the latest patches of the 10 previous minors are kept (release page and images). An older minor drops out when it is no longer among the last 10: its release page and images are deleted, the tag stays. There are no exceptions: production always runs a current version.
- Laboratory only: the releases below 1.10 (1.x before the lines) are history from before the policy and are not pruned.
- The dev images (`sha-<7>` of commits that are part of `main`) and the registry layer caches are deleted by the same run.

### Signed commits and immutable releases

The rulesets "Signed commits" (`main`, `develop`, `release/*`) and "Protect release tags" (`v*` except `v*-rc.*`: no update, no deletion), and "immutable releases", are on. What follows from them:

- No workflow makes a commit with git. The only commits a workflow creates are the backport and the security carry, through the API (above). `promote/vX.Y.Z` points at the already signed, already merged candidate commit (no new commit); the merge into `main` is made by GitHub (auto-merge, signed). Nothing bumps a version, edits the chart or writes release notes into git: the chart version is a `helm package --version` argument, and the release notes are generated by GitHub.
- A release is made as a draft, gets its asset, and is published last (the order GitHub recommends for immutable releases): a published release takes no new asset and its tag cannot move. Title, notes and the latest flag stay editable, so nothing in the flow needs more. A candidate pre-release has no asset and is published at once. Deleting an immutable release is allowed, and then its tag may be deleted too (the name can never be reused: the numbers of candidates only grow, so nothing reuses one); retention deletes a candidate's page with its tag (`gh release delete --cleanup-tag`), a final release's page without its tag, and never touches a final tag (`retention.sh` refuses; a test covers it).
- Docker Hub tag immutability and these GitHub rules are independent: a refusal from either is reported by retention and tried again by the next run.

### Pull requests that automation opens

Events made with `GITHUB_TOKEN` do not start workflows, except `workflow_dispatch` and `repository_dispatch`. A pull request opened by Backport, Security fixes or Promote does start a `pull_request` run of `check.yml`, but, according to the GitHub documentation, in an *approval-required* state: the pull request shows a banner and a user with write access presses **Approve workflows to run** (this behaviour was documented in May 2026 and is still rolling out; if the button is not there, close the pull request and reopen it, which a person's event always starts). The required check must come from a `pull_request` run: GitHub does not evaluate the checks of a `workflow_dispatch` run for a pull request, so dispatching `check.yml` on the branch would not satisfy `Check passed`, and `check.yml` has no dispatch trigger. After the check is green, auto-merge completes the pull request.

### Where each workflow definition comes from

The default branch is `develop`; `main` only receives promoted lines. `pull_request_target` takes the workflow and the checkout from the default branch (GitHub changed this on 2025-12-08), not from the base branch of the pull request. The Docker Hub write credentials exist only as secrets of three environments, each bound to branches: `candidate` (`release/*`), `release` (`main`), `build` (`develop` and `release/*`). No other credential exists: no GitHub App, no personal token.

| Workflow | Event | Definition comes from | Secrets | Can write access to `develop` alone change a release? |
|---|---|---|---|---|
| `line-gate.yml` | `pull_request_target` into `release/*` | `develop` | none | It changes the gate for every line, but only through a reviewed pull request into `develop`; Promote runs the gate again from its own checkout of `main`, so a weakened copy on `develop` cannot get a release past Promote |
| `main-gate.yml` | `pull_request_target` into `main` | `develop` (default branch) | none | It changes the gate for `main`, but only through a reviewed pull request into `develop` |
| `check.yml`, `test.yml` | `pull_request` into `develop`, `release/*` | the merge commit of the pull request | none | No secret at all; the gate keeps `.github/**` out of a line |
| `candidate.yml` | push to `release/X.Y`, or manual | the line, or the chosen ref | environment `candidate` (`release/*` only) | No: a branch whose name matches `release/*` is needed, and only the owner may create one; the environment refuses any other ref |
| `backport.yml` | push to `release/X.Y` | the line | none (job token only) | No |
| `security.yml` | `pull_request_target` closed or labeled, into `develop` | `develop` | none (job token only) | It opens pull requests into the lines, which the owner merges by hand behind `Check passed` and `Line gate`; it runs only code already merged into `develop` |
| `promote.yml`, `cleanup.yml` | manual | the chosen ref, `main` required (a first step stops any other ref) | environment `release` (`main` only) | No: a copy on `develop` gets no secret; its scripts and gate are the ones of `main` |
| `build.yml` | manual | the chosen ref (`develop` by default) | environment `build` (`develop` and `release/*`) | A run on `develop` can publish `sha-*` tags, which are not part of the release path and are never promoted |
| scheduled workflows | none exist | | | |

### Settings in GitHub that the workflows rely on

- Default branch: `develop`. `pull_request_target` (`line-gate.yml`) and the Run workflow button read their workflow from it, so every workflow must be on `develop` (and `promote.yml`, `cleanup.yml` and `.github/scripts` also on `main`, see Bootstrap).
- Environments (the only place with secrets; each holds `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` and nothing else): `candidate`, deployment branches `release/*` only, push permission is enough; `release`, deployment branch `main` only, Read/Write/Delete (retention deletes); `build`, deployment branches `develop` and `release/*`, push permission.
- Rulesets (not classic branch protection, so admins are bound too). `release/*`: creation, update and deletion only through the owner (nobody else may create a branch of that name: the environments trust the name), pull request required, no direct push, no force push, required checks `Check passed` and `Line gate` (the job names, they are stable), no required approvals (the owner works alone), merges by hand, no merge queue (the workflows do not run for `merge_group`). `develop`: pull request required, required check `Check passed`. `main`: pull request required, no force push, no deletion, no bypass actor (exists), and add the required check `Main gate` (it is not there yet: today anything with write access can merge a pull request into `main`). `Check passed` is not produced for pull requests into `main`. `develop` and `release/*` have no ruleset yet apart from "Signed commits": the ones above must be created.
- Actions: default workflow permissions read-only (already set), "Require actions to be pinned to a full commit SHA" (already set), approval for every pull request from outside collaborators (fork pull requests), **"Allow GitHub Actions to create and approve pull requests" on** (Backport, Security fixes and Promote open pull requests with the job token), and "Allow auto-merge" on (already used by `dev-push`).
- Docker Hub: tag immutability for the pattern above on the five image repositories and `cybericebox/laboratory` (the chart). Check once on a scratch repository whether an immutable tag can be deleted through the API: if it cannot, restrict the immutable pattern to final versions (`^v?[0-9]+\.[0-9]+\.[0-9]+$`), or retention will keep reporting every candidate as not deleted.
- Rulesets "Signed commits" and "Protect release tags", and immutable releases: already on (see above).
- Labels `large-fix` and `security` (anyone with triage rights can set them: keep that role to the owner; `security` on a merged pull request into `develop` opens pull requests into the lines).
- A promote is started with `gh workflow run promote.yml --ref main -f rc=...`.
- Whoever has write access can create a check run named `Check passed` on any commit from a workflow of their own branch: Promote trusts the checks, so write access to the repository is write access to the release.

### Bootstrap

The scripts that decide are read from `main`, and the environment `release` only runs on `main`, so `promote.yml`, `cleanup.yml` and everything under `.github/scripts` (and `mise.toml`) must be on `main` before the first promote. They get there once through an ordinary pull request `develop` into `main`: it is not a promote branch, so `Main gate` fails it by design; the owner removes the `Main gate` requirement from the ruleset for that one merge and puts it back (the same for a conflict that has to be resolved by hand: a pull request from any other branch). After that `main` is updated only by the pull requests of Promote.

## The chart

A candidate or a release packages the Helm chart (`version` `X.Y.Z[-rc.N]`, `appVersion` `vX.Y.Z[-rc.N]`, which is the tag of every image), pushes it to `oci://registry-1.docker.io/cybericebox/laboratory`, and a release also attaches it to the GitHub release.

## API contract version

The agent API has a contract version (`pkg/agent/client/contract.go`, `ContractVersion`, "major.minor"). Callers (the daemon, the
infrastructure check) compare it with `client.Compatible`, not the image tag: a laboratory release that changes no call must not
stop a daemon. Any change of `agent.proto` fails `TestContractLock` until the version is bumped (minor for an additive change,
major for a break), the Dockerfile `agent` label is updated and `pkg/agent/protobuf/contract.lock` gets the new line.
