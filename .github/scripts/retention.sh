#!/usr/bin/env bash
# The retention policy (CONTRIBUTING.md, "Retention"), run by Promote right after a release and by hand (Retention workflow).
#   retention.sh                    environment: DRY_RUN (true|false, default true), IMAGES, CHART_REPO, PRUNE_FROM, PRUNE_KEEP,
#                                   MAIN_REF (default origin/main); GH_TOKEN and the Docker Hub credentials when DRY_RUN is false;
#                                   STRICT (default true): when false, a deletion that failed or was refused (Docker Hub may refuse an
#                                   immutable tag) is reported but the exit status stays 0, so a promote that has already published
#                                   the release does not turn red; it is always listed (warnings and the step summary)
# A final tag counts only when its commit is part of main (Promote merges every released line into main): a tag made by hand on
# any other commit changes neither the newest line nor the window, so it can never make a real release look old.
# The git tag and the release page of a version are deleted only when all its images and chart versions are gone, so a refused
# deletion is found again by the next run.
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

failures=()
VERSION_SHAPE='^v?[0-9]+\.[0-9]+(\.[0-9]+)?(-rc\.[0-9]+)?$'

# only_ours: the tags on stdin that retention may act on: a final tag must be on main; a tag of any other shape is not ours
only_ours() {
  local t
  while read -r t; do
    [[ -n "$t" ]] || continue
    [[ "$t" =~ $VERSION_SHAPE ]] || continue
    if [[ "$t" != *-rc.* ]] && ! git merge-base --is-ancestor "refs/tags/$t^{commit}" "${MAIN_REF:-origin/main}" 2>/dev/null; then
      echo "ignored: $t is a final tag whose commit is not part of main" >&2
      continue
    fi
    echo "$t"
  done
}

# drop_release TAG with_git_tag: delete the GitHub release page, and the git tag when asked
drop_release() {
  local tag=$1 with_tag=$2
  [[ "$tag" =~ $VERSION_SHAPE ]] || { echo "refusing to delete the release page of odd tag $tag"; return 0; }
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
  local tag=$1 rc=0
  [[ "$tag" =~ $VERSION_SHAPE ]] || { echo "refusing to delete the artifacts of odd tag $tag"; return 1; }
  hub_failed=()
  # shellcheck disable=SC2086
  delete_matching "$DRY_RUN" "$(esc "$tag")" $IMAGES || rc=1
  delete_matching "$DRY_RUN" "$(esc "${tag#v}")" "$CHART_REPO" || rc=1
  [[ "$rc" -eq 0 ]] || failures+=("${hub_failed[@]:-$tag (listing failed)}")
  return "$rc"
}

# report: the failures, in the log, as warnings and in the step summary
report() {
  local f
  [[ ${#failures[@]} -gt 0 ]] || return 0
  echo "== not deleted (${#failures[@]}): the next run tries again"
  for f in "${failures[@]}"; do
    echo "::warning::retention could not delete $f"
    echo "  $f"
  done
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    {
      echo "### Retention: not deleted (${#failures[@]})"
      printf -- '- %s\n' "${failures[@]}"
    } >>"$GITHUB_STEP_SUMMARY"
  fi
}

retention() {
  local tags tag
  DRY_RUN=${DRY_RUN:-true}
  git fetch --tags --force --quiet 2>/dev/null || true
  failures=()
  tags=$(git tag --list | only_ours)

  echo "== candidates of released versions"
  while read -r tag; do
    [[ -n "$tag" ]] || continue
    if drop_artifacts "$tag"; then drop_release "$tag" true; else echo "$tag stays (an artifact was not deleted)"; fi
  done < <("$here/release_version.py" stale-rc <<<"$tags")

  echo "== releases out of the window (from ${PRUNE_FROM:-0.0}, keep ${PRUNE_KEEP:-10} previous lines)"
  while read -r tag; do
    [[ -n "$tag" ]] || continue
    if drop_artifacts "$tag"; then drop_release "$tag" false; else echo "$tag stays (an artifact was not deleted)"; fi
  done < <("$here/release_version.py" prune --from "${PRUNE_FROM:-0.0}" --keep "${PRUNE_KEEP:-10}" <<<"$tags")

  echo "== dev images and caches of what main has"
  hub_failed=()
  # shellcheck disable=SC2086
  cleanup "$(git rev-parse "${MAIN_REF:-origin/main}^{commit}")" "$DRY_RUN" $IMAGES || failures+=("${hub_failed[@]:-dev images or caches (listing failed)}")

  report
  [[ ${#failures[@]} -eq 0 || "${STRICT:-true}" != true ]]
}

[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0 2>/dev/null
retention
