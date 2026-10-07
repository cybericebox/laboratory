# Contributing

`main` holds releases, `develop` is the integration branch. Nobody pushes to either directly (branch protection applies to admins too): every change is a feature branch and a pull request.

## The cycle

1. `make dev-start NAME=<x>`: fetches `develop` and creates `feature/<x>` from it.
2. Work and commit in that branch.
3. `make dev-push [MINOR=1]`: pushes the branch (it refuses on `develop` and `main` and warns about uncommitted changes), opens a PR into `develop` or updates the open one, labels it `minor` with `MINOR=1`, and turns on auto-merge: the PR merges by itself once every check is green.
4. After the merge GitHub deletes the remote branch. `make dev-done` switches to `develop`, pulls it and deletes the local branch.

Both targets use `scripts/dev.sh` and need the `gh` CLI, logged in.

## Checks and images

- A PR into `develop` or `main` runs `check.yml`: unit tests, lint and the e2e test, and the Docker build, which writes its layers to the registry cache `cybericebox/laboratory-<name>:buildcache-develop` and publishes nothing. A new push cancels the previous run. A PR from a fork builds without the cache. The required check is `Check passed`.
- A merge into `develop` (`develop.yml`) builds from that cache and publishes `sha-<7 hex of the merge commit>`. There is no moving `develop` tag.
- `Build image` (`build.yml`, manual) builds any commit, by default the `develop` head, and publishes its `sha-<7>`. When that image exists it is not rebuilt.

## Releases

- A merge of `develop` into `main` (`prerelease.yml`) publishes `vX.Y.Z-rc.N` and creates the GitHub pre-release. The version is a patch bump of the last release; it is a minor bump when any PR merged since then has the `minor` label; a major bump only through a manual run with `major`. Nothing is built when the tree is unchanged since the last rc. The rc is a retag of the develop image (no build) when the tree of `main` equals the tree of the develop head (the second parent of the merge) and that head has its `sha-<7>` image; otherwise it is built, reading the `buildcache-develop` cache.
- `Promote` (`promote.yml`, manual, input: the rc tag) checks the rc image and its checks, adds `vX.Y.Z` and `latest` to the same image without a rebuild, creates the GitHub release, deletes the `vX.Y.Z-rc.*` tags and pre-releases, and deletes the dev images: only tags that match `^sha-[0-9a-f]{7}$` whose commit is an ancestor of the release commit, plus the `buildcache-develop` and old `buildcache` cache tags (the next PR starts with a cold cache). The list is printed first; the `dry_run` input changes nothing.
- `Cleanup` (`cleanup.yml`, manual, inputs: the release `vX.Y.Z` and `dry_run`) runs the same dev-image cleanup against an existing release commit, for example when it was skipped.
- Clusters and the chart use exact tags only (`sha-*` or `vX.Y.Z`); `latest` is for outside users.

## The chart

Promoting an rc also packages the Helm chart (`version` `X.Y.Z`, `appVersion` `vX.Y.Z`), pushes it to `oci://registry-1.docker.io/cybericebox/laboratory` and attaches it to the GitHub release. The daemon picks up a laboratory release with `make dev-push LAB=vX.Y.Z` in daemon.
