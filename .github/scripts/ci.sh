#!/usr/bin/env bash
# CI helpers, the same file in every repository that publishes images. Used by the workflows in .github/workflows.
#   ci.sh sha-tag SHA                         sha-<7 hex>
#   ci.sh exists IMAGE TAG                    exit 0 when IMAGE:TAG exists in the registry
#   ci.sh next-version                        BUMP=auto|major; writes skip, version, tag to GITHUB_OUTPUT
#   ci.sh hub-tags IMAGE                      every tag of a Docker Hub repository, one per line
#   ci.sh hub-delete IMAGE TAG                delete one tag (Docker Hub API)
#   ci.sh delete-rc VERSION IMAGE...          delete every VERSION-rc.* tag of the images
#   ci.sh cleanup RELEASE_SHA DRY_RUN IMAGE...  delete sha-<7> dev tags whose commit is an ancestor of RELEASE_SHA
# Docker Hub calls use DOCKERHUB_USERNAME and DOCKERHUB_TOKEN (org secrets, Read/Write/Delete); GitHub calls use GH_TOKEN.
set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }

hub_jwt=""
hub_login() {
  [[ -n "$hub_jwt" ]] && return 0
  [[ -n "${DOCKERHUB_USERNAME:-}" && -n "${DOCKERHUB_TOKEN:-}" ]] || die "DOCKERHUB_USERNAME and DOCKERHUB_TOKEN are required"
  hub_jwt=$(jq -n --arg u "$DOCKERHUB_USERNAME" --arg p "$DOCKERHUB_TOKEN" '{username: $u, password: $p}' |
    curl -fsS -X POST -H 'Content-Type: application/json' -d @- https://hub.docker.com/v2/users/login | jq -r .token)
  [[ -n "$hub_jwt" && "$hub_jwt" != null ]] || die "Docker Hub login failed"
}

hub_tags() {
  hub_login
  local url="https://hub.docker.com/v2/repositories/$1/tags?page_size=100"
  while [[ -n "$url" && "$url" != null ]]; do
    local page
    page=$(curl -fsS -H "Authorization: JWT $hub_jwt" "$url")
    jq -r '.results[].name' <<<"$page"
    url=$(jq -r '.next // empty' <<<"$page")
  done
}

hub_delete() {
  hub_login
  curl -fsS -o /dev/null -X DELETE -H "Authorization: JWT $hub_jwt" "https://hub.docker.com/v2/repositories/$1/tags/$2/"
  echo "deleted $1:$2"
}

# true when $1 is a higher version than $2 (sort -V)
ver_gt() { [[ "$1" != "$2" && "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n 1)" == "$1" ]]; }

next_version() {
  local last rcs latest_rc="" n bump=patch since count
  last=$(git tag --list 'v[0-9]*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1 || true)
  last=${last:-v0.0.0}

  # rc tags of versions above the last release (older ones are leftovers)
  rcs=""
  while read -r t; do
    [[ -n "$t" ]] || continue
    ver_gt "${t%-rc.*}" "$last" && rcs+="$t"$'\n'
  done < <(git tag --list 'v*-rc.*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+-rc\.[0-9]+$' || true)
  [[ -n "$rcs" ]] && latest_rc=$(printf '%s' "$rcs" | sort -V | tail -n 1)

  # skip when the tree is the one of the last rc (or of the last release)
  local tree
  tree=$(git rev-parse 'HEAD^{tree}')
  if [[ -n "$latest_rc" && "$(git rev-parse "$latest_rc^{tree}")" == "$tree" ]]; then
    echo "the tree is unchanged since $latest_rc: no new rc"
    echo "skip=true" >>"${GITHUB_OUTPUT:-/dev/stdout}"
    return 0
  fi
  if git rev-parse -q --verify "$last^{commit}" >/dev/null && [[ "$(git rev-parse "$last^{tree}")" == "$tree" ]]; then
    echo "the tree is unchanged since the release $last: no new rc"
    echo "skip=true" >>"${GITHUB_OUTPUT:-/dev/stdout}"
    return 0
  fi

  if [[ "${BUMP:-auto}" == major ]]; then
    bump=major
  else
    since=1970-01-01T00:00:00Z
    if git rev-parse -q --verify "$last^{commit}" >/dev/null; then
      since=$(TZ=UTC git log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ "$last^{commit}")
    fi
    count=$(gh api -X GET search/issues -f q="repo:${GITHUB_REPOSITORY} is:pr is:merged label:minor merged:>$since" --jq .total_count)
    [[ "$count" -gt 0 ]] && bump=minor
    echo "last release $last ($since), merged PRs labelled minor since: $count"
  fi

  local x y z
  IFS=. read -r x y z <<<"${last#v}"
  case "$bump" in
    major) x=$((x + 1)); y=0; z=0 ;;
    minor) y=$((y + 1)); z=0 ;;
    *) z=$((z + 1)) ;;
  esac
  local version="v$x.$y.$z"
  # an rc of a higher version (an earlier major run) keeps its version
  if [[ -n "$latest_rc" ]] && ver_gt "${latest_rc%-rc.*}" "$version"; then
    version=${latest_rc%-rc.*}
  fi
  n=0
  if [[ -n "$rcs" ]]; then
    while read -r t; do
      [[ "${t%-rc.*}" == "$version" ]] && n=$((${t##*-rc.} > n ? ${t##*-rc.} : n))
    done <<<"$rcs"
  fi
  {
    echo "skip=false"
    echo "version=$version"
    echo "tag=$version-rc.$((n + 1))"
  } >>"${GITHUB_OUTPUT:-/dev/stdout}"
  echo "next: $version-rc.$((n + 1)) ($bump)"
}

cleanup() {
  local release_sha=$1 dry=$2
  shift 2
  local image tag full
  for image in "$@"; do
    echo "== $image"
    local doomed=()
    while read -r tag; do
      [[ "$tag" =~ ^sha-[0-9a-f]{7}$ ]] || continue
      full=$(git rev-parse -q --verify "${tag#sha-}^{commit}" 2>/dev/null || true)
      if [[ -z "$full" ]]; then
        echo "keep $tag (commit unknown here)"
        continue
      fi
      if git merge-base --is-ancestor "$full" "$release_sha"; then
        doomed+=("$tag")
      else
        echo "keep $tag (not an ancestor of the release commit)"
      fi
    done < <(hub_tags "$image")
    echo "to delete from $image: ${#doomed[@]}"
    printf '  %s\n' "${doomed[@]}"
    [[ "$dry" == true ]] && continue
    for tag in "${doomed[@]}"; do
      hub_delete "$image" "$tag"
    done
  done
  [[ "$dry" == true ]] && echo "dry run: nothing deleted"
  return 0
}

cmd=${1:-}
shift || true
case "$cmd" in
  sha-tag) echo "sha-$(printf '%s' "$1" | cut -c1-7)" ;;
  exists) docker buildx imagetools inspect "$1:$2" >/dev/null 2>&1 ;;
  next-version) next_version ;;
  hub-tags) hub_tags "$1" ;;
  hub-delete) hub_delete "$1" "$2" ;;
  delete-rc)
    version=$1
    shift
    for image in "$@"; do
      while read -r tag; do
        [[ "$tag" =~ ^${version//./\\.}-rc\.[0-9]+$ ]] && hub_delete "$image" "$tag"
      done < <(hub_tags "$image")
    done
    ;;
  cleanup) cleanup "$@" ;;
  *) die "usage: ci.sh sha-tag|exists|next-version|hub-tags|hub-delete|delete-rc|cleanup" ;;
esac
