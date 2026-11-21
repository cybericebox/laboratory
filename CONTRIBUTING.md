# Contributing

`main` holds releases, `develop` is the integration branch. Nobody pushes to either directly (branch protection applies to admins too): every change is a feature branch and a pull request.

## The cycle

1. `mise run dev-start <x>`: fetches `develop` and creates `feature/<x>` from it.
2. Work and commit in that branch.
3. `[MINOR=1] mise run dev-push`: pushes the branch (it refuses on `develop` and `main` and warns about uncommitted changes), opens a PR into `develop` or updates the open one, labels it `minor` with `MINOR=1`, and turns on auto-merge: the PR merges by itself once every check is green.
4. After the merge GitHub deletes the remote branch. `mise run dev-done` switches to `develop`, pulls it and deletes the local branch.

The tasks use `scripts/dev.sh` and need the `gh` CLI, logged in.

## Checks and images

- A PR into `develop` or `main` runs `check.yml`: `test.yml` (generated files are current, unit tests, lint, the iptables tests in network namespaces, the eBPF tests and the reproducibility of the eBPF objects, the e2e test) and the Docker build, which writes its layers to the registry cache `cybericebox/laboratory-<name>:buildcache-develop` and publishes nothing. A new push cancels the previous run. A PR from a fork builds without the cache. The required check is `Check passed`.
- A merge into `develop` (`develop.yml`) builds from that cache and publishes `sha-<7 hex of the merge commit>`. There is no moving `develop` tag.
- `Build image` (`build.yml`, manual) builds any commit, by default the `develop` head, and publishes its `sha-<7>`. When that image exists it is not rebuilt.

## Tools

Every command runs through mise (`mise install` once, then `mise run <task>`; any Makefile target is `mise run make <target>`). Where a tool is pinned:

- **A Go tool of the module goes into `go.mod` as a `tool` directive**: `controller-gen`, `setup-envtest` and `bpf2go`. They run as `go tool <name>`, built by the toolchain of `go.mod`, at the version `go.mod` records. Add one with `go get -tool <package>@<version>`, bump it with `go get -tool <package>@<version>`, then `go mod tidy` and `mise run generate-check`. A bump of `controller-tools` changes the version line of every CRD, so commit the regenerated files with it.
- **Everything else goes into `mise.toml`**: Go itself (the same version as the `toolchain` line of `go.mod`; the `golang` images in `Dockerfile` and `hack/*.sh` are pinned to it by tag and digest, and `hack/generate-bpf.sh` keeps the Debian snapshot date of its image), kind, kubectl, helm, kustomize, protoc and its Go plugins, zot, gh. `golangci-lint` is also in `mise.toml` (built with the pinned Go through the `go:` backend), not in `go.mod`: its authors advise against making it a module dependency, and it would pull its whole dependency tree into the module.
- The code generators of `hack/update-codegen.sh` (`client-gen`, `lister-gen`, `informer-gen`, `applyconfiguration-gen`) are not tool directives: `kube_codegen.sh` of `k8s.io/code-generator` builds them itself with `go install`, at the version of `k8s.io/client-go` in `go.mod`, into `bin/codegen` (ignored).
- Nothing is installed by hand and nothing a task writes goes outside `bin/`, `dist/` and the generated directories (`.gitignore`).

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
3. Add `bases/<group>_<plural>.yaml` to `config/crd/kustomization.yaml`. For a controller, add `+kubebuilder:rbac` markers to it (`mise run generate` writes `config/rbac/role.yaml`) and the same rules to the chart's ClusterRoles in `charts/laboratory/templates`; `test/chart/rbac*_test.go` check them.
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
mise run test-bpf                                # Docker; after a change of wg_fastpath.bpf.c also: mise run make generate-bpf and commit the objects
mise run cluster-up && mise run e2e              # Docker; then: mise run cluster-down
```

After Docker work remove what it left: `docker image prune -af`, `docker builder prune -af`, `docker volume prune -af`.

## Releases

- A merge of `develop` into `main` (`prerelease.yml`) publishes `vX.Y.Z-rc.N` and creates the GitHub pre-release. The version is a patch bump of the last release; it is a minor bump when any PR merged since then has the `minor` label; a major bump only through a manual run with `major`. Nothing is built when the tree is unchanged since the last rc. The rc is a retag of the develop image (no build) when the tree of `main` equals the tree of the develop head (the second parent of the merge) and that head has its `sha-<7>` image; otherwise it is built, reading the `buildcache-develop` cache.
- `Promote` (`promote.yml`, manual, input: the rc tag) checks the rc image and its checks, adds `vX.Y.Z` and `latest` to the same image without a rebuild, creates the GitHub release, deletes the `vX.Y.Z-rc.*` tags and pre-releases, and deletes the dev images: only tags that match `^sha-[0-9a-f]{7}$` whose commit is an ancestor of the release commit, plus the `buildcache-develop` and old `buildcache` cache tags (the next PR starts with a cold cache). The list is printed first; the `dry_run` input changes nothing.
- `Cleanup` (`cleanup.yml`, manual, inputs: the release `vX.Y.Z` and `dry_run`) runs the same dev-image cleanup against an existing release commit, for example when it was skipped.
- Clusters and the chart use exact tags only (`sha-*` or `vX.Y.Z`); `latest` is for outside users.

### Retention

The same rule applies to GitHub release pages and published images, in every repo.

- Git tags of final releases are kept forever, so any version can be rebuilt from source.
- Release candidates (`-rc.N`): tags, release pages and images are deleted right after the final release is promoted.
- The newest minor keeps all its patch releases and images.
- Older minors keep only their latest patch. The other patches of a minor are deleted when the next minor is released.
- Besides the newest minor, the latest patches of the 10 previous minors are kept (release page and images). An older minor drops out when it is no longer among the last 10: its release page and images are deleted, the tag stays. There are no exceptions: production always runs a current version.
- Pruning runs automatically in the release workflow. Manual cleanup is done only once, when the policy is introduced.

## The chart

Promoting an rc also packages the Helm chart (`version` `X.Y.Z`, `appVersion` `vX.Y.Z`), pushes it to `oci://registry-1.docker.io/cybericebox/laboratory` and attaches it to the GitHub release. The daemon picks up a laboratory release with `mise run dev-push LAB=vX.Y.Z` in daemon.

## API contract version

The agent API has a contract version (`pkg/agent/client/contract.go`, `ContractVersion`, "major.minor"). Callers (the daemon, the
infrastructure check) compare it with `client.Compatible`, not the image tag: a laboratory release that changes no call must not
stop a daemon. Any change of `agent.proto` fails `TestContractLock` until the version is bumped (minor for an additive change,
major for a break), the Dockerfile `agent` label is updated and `pkg/agent/protobuf/contract.lock` gets the new line.
