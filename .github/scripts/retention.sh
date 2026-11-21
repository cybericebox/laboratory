#!/usr/bin/env bash
# The retention policy (CONTRIBUTING.md, "Retention"), run by Promote right after a release and by hand (Retention workflow).
#   retention.sh                    environment: DRY_RUN (true|false, default true), IMAGES, CHART_REPO, PRUNE_FROM, PRUNE_KEEP,
#                                   MAIN_REF (default origin/main); GH_TOKEN and the Docker Hub credentials when DRY_RUN is false
# 1. candidates (vX.Y.Z-rc.N) of every version that has been released: images, chart, pre-release page and git tag
# 2. final releases that left the window (the newest line keeps every patch; each of the PRUNE_KEEP previous lines keeps its newest
#    patch; lines below PRUNE_FROM are outside the policy): images, chart and release page; the git tag stays forever
# 3. the dev images (sha-<7>) of commits that are part of main, and the registry layer caches
# The list is printed first. With DRY_RUN=true nothing is deleted.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=ci.sh
source "$here/ci.sh"

esc() { echo "${1//./\\.}"; }

# drop_release TAG with_git_tag: delete the GitHub release page, and the git tag when asked
drop_release() {
  local tag=$1 with_tag=$2
  if [[ "$DRY_RUN" == true ]]; then
    echo "would delete the release page of $tag$([[ "$with_tag" == true ]] && echo ' and its git tag')"
    return 0
  fi
  if [[ "$with_tag" == true ]]; then
    gh release delete "$tag" --cleanup-tag --yes 2>/dev/null || git push origin --delete "refs/tags/$tag" || echo "no release page or tag $tag"
  else
    gh release delete "$tag" --yes 2>/dev/null || echo "no release page for $tag"
  fi
}

# drop_artifacts TAG: the images and the chart of a version
drop_artifacts() {
  local tag=$1
  # shellcheck disable=SC2086
  delete_matching "$DRY_RUN" "$(esc "$tag")" $IMAGES
  delete_matching "$DRY_RUN" "$(esc "${tag#v}")" "$CHART_REPO"
}

retention() {
  local tags tag
  DRY_RUN=${DRY_RUN:-true}
  git fetch --tags --force --quiet 2>/dev/null || true
  tags=$(git tag --list)

  echo "== candidates of released versions"
  while read -r tag; do
    [[ -n "$tag" ]] || continue
    drop_artifacts "$tag"
    drop_release "$tag" true
  done < <("$here/release_version.py" stale-rc <<<"$tags")

  echo "== releases out of the window (from ${PRUNE_FROM:-0.0}, keep ${PRUNE_KEEP:-10} previous lines)"
  while read -r tag; do
    [[ -n "$tag" ]] || continue
    drop_artifacts "$tag"
    drop_release "$tag" false
  done < <("$here/release_version.py" prune --from "${PRUNE_FROM:-0.0}" --keep "${PRUNE_KEEP:-10}" <<<"$tags")

  echo "== dev images and caches of what main has"
  # shellcheck disable=SC2086
  cleanup "$(git rev-parse "${MAIN_REF:-origin/main}^{commit}")" "$DRY_RUN" $IMAGES
}

[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0 2>/dev/null
retention
