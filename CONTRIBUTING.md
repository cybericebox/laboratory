# Contributing

`main` holds releases, `develop` is the integration branch. Nobody pushes to either directly (branch protection applies to admins too): every change is a feature branch and a pull request.

## The cycle

1. `make dev-start NAME=<x>`: fetches `develop` and creates `feature/<x>` from it.
2. Work and commit in that branch.
3. `make dev-push [MINOR=1]`: pushes the branch (it refuses on `develop` and `main` and warns about uncommitted changes), opens a PR into `develop` or updates the open one, labels it `minor` with `MINOR=1`, and turns on auto-merge: the PR merges by itself once every check is green.
4. After the merge GitHub deletes the remote branch. `make dev-done` switches to `develop`, pulls it and deletes the local branch.

Both targets use `scripts/dev.sh` and need the `gh` CLI, logged in.

## Checks and images

- A PR into `develop` or `main` runs `check.yml`: unit tests, lint and the e2e test, and the Docker build, which publishes no runnable image and stores its BuildKit cache in Docker Hub. A new push cancels the previous run. A PR from a fork never writes caches or receives Docker Hub credentials. The required check is `Check passed`.
- A merge into `develop` (`develop.yml`) builds from that cache and publishes `sha-<7 hex of the merge commit>`. There is no moving `develop` tag.
- `Build image` (`build.yml`, manual) builds any commit, by default the `develop` head, and publishes its `sha-<7>`. When that image exists it is not rebuilt.

## Releases

- A merge of `develop` into `main` (`prerelease.yml`) publishes `vX.Y.Z-rc.N` and creates the GitHub pre-release. The version is a patch bump of the last release; it is a minor bump when any PR merged since then has the `minor` label; a major bump only through a manual run with `major`. Nothing is built when the tree is unchanged since the last rc. The rc is a retag of the develop image (no build) when the tree of `main` equals the tree of the develop head (the second parent of the merge) and that head has its `sha-<7>` image; otherwise it is built, reading the `buildcache-develop` cache.
- `Promote` (`promote.yml`, manual, input: the rc tag) checks the rc image and its checks, adds `vX.Y.Z` and `latest` to the same image without a rebuild, creates the GitHub release, deletes the `vX.Y.Z-rc.*` tags and pre-releases, and deletes the dev images: only tags that match `^sha-[0-9a-f]{7}$` whose commit is an ancestor of the release commit, then retires verified BuildKit cache aliases (the next PR starts with a cold cache); obsolete cache manifests are deleted by quarterly maintenance. The list is printed first; the `dry_run` input changes nothing.
- `Cleanup` (`cleanup.yml`, manual, inputs: the release `vX.Y.Z` and `dry_run`) runs the same dev-image cleanup against an existing release commit, for example when it was skipped.
- Clusters and the chart use exact tags only (`sha-*` or `vX.Y.Z`); `latest` is for outside users.

## The chart

Promoting an rc also packages the Helm chart (`version` `X.Y.Z`, `appVersion` `vX.Y.Z`), pushes it to `oci://registry-1.docker.io/cybericebox/laboratory` and attaches it to the GitHub release. The daemon picks up a laboratory release with `make dev-push LAB=vX.Y.Z` in daemon.

## Docker Hub build cache lifecycle

Build caches stay in Docker Hub, separately from runnable `sha-*`, release and
special images. The PR check exports `mode=max` to a unique
`buildcache-run-<timestamp>-<run-id>-<attempt>` tag, then publishes that exact
manifest as `buildcache-develop`. Before replacing a pre-existing cache it adds
`buildcache-history-<timestamp>-<digest>` when needed. These tags track digests
on Docker Hub, including interrupted or concurrent builds; no manual digest log
or GitHub Actions cache storage is required. Each laboratory target uses its
own image repository, so its cache cannot overwrite another target's cache.

The separate `Registry cache maintenance` workflow runs every three months and expires
superseded tracked caches that are at least 72 hours old. PR builds only publish
and track caches; they do not scan retained images or prune caches. The current
cache remains available throughout the dev
and pre-release cycle, even when it is a week old. A full `Promote` or manual
release `Cleanup` removes verified `buildcache-develop`, legacy `buildcache` and
`buildcache-run-*` aliases immediately. It keeps a `buildcache-history-*` reference
for each cache digest; quarterly maintenance removes these references and the
obsolete cache manifests. Retirement itself never deletes a manifest. Dev tags are deleted only when
their commits are ancestors of the selected release; newer/unknown commits,
release tags and special tags are preserved. Existing deployments using a
removed dev tag must switch to the release tag before cleanup.

The helper reads retained tags and their multi-architecture descendants before
deleting anything. Only verified BuildKit cache manifests are eligible; layer
blobs and runnable image manifests are never deleted. If Docker Hub reports a
remaining reference, the tracking tag is restored. A cache cleanup failure
keeps the published cache usable and is reported by the workflow. Registry
storage accounting and background deletion can take time to update.

The quarterly schedule for this repository is day 7 at 03:17 UTC in January, April, July and October. Schedules are
staggered across repositories, and laboratory processes its five images sequentially.
GitHub activates scheduled workflows only after they reach the repository's default
branch. In public repositories, GitHub may disable schedules after 60 days without
repository activity; re-enable the workflow when needed. A manual workflow run is
also available, with `dry_run` enabled by default. Scheduled runs apply deletion.
Maintenance shares the image-build/release cache lock. Authentication, manifest-read
or deletion errors fail the maintenance workflow instead of being hidden in a PR
check. This workflow does not discover legacy untracked caches.

To preview tracked old cache cleanup from this repository (requires the usual
`DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` Read/Write/Delete environment secrets):

```sh
python3 .github/scripts/cache.py prune cybericebox/REPOSITORY --dry-run
python3 .github/scripts/cache.py retire cybericebox/REPOSITORY --dry-run
```

Replace `REPOSITORY` with this image's repository name; for laboratory run once
per controller/agent/proxy/node/lab image. Remove `--dry-run` to apply.

Old untagged caches created before tracking was introduced must be inventoried
once using Docker Hub Image Management. Never bulk-delete every untagged row:
architecture images and attestations may have no tags. Copy full verified cache
digests and preview them with the same reference checks:

```sh
python3 .github/scripts/cache.py orphans cybericebox/REPOSITORY sha256:FULL_DIGEST --dry-run
```

This command cannot discover unknown untagged objects itself; Docker Hub's
standard Registry API does not expose an enumeration of all such manifests.

Cache-reading/writing image builds and release/manual cache retirement share a queued
job concurrency group (`docker-hub-cache-maintenance`, `queue: max`). They
cannot update and delete the mutable canonical cache tag simultaneously. The
laboratory image jobs wait in that same queue; image builds in other repositories and ordinary tests are unaffected. Manual cache commands must be run
while these cache writers are idle.
