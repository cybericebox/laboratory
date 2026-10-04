#!/usr/bin/env bash
# CI helpers, the same file in every repository that publishes images. Used by the workflows in .github/workflows.
#   ci.sh sha-tag SHA                         sha-<7 hex>
#   ci.sh exists IMAGE TAG                    exit 0 when IMAGE:TAG exists in the registry
#   ci.sh next-version                        BUMP=auto|major, VERSION=X.Y.Z (optional override, above the last release); writes skip, version, tag to GITHUB_OUTPUT
#   ci.sh hub-tags IMAGE                      every tag of a Docker Hub repository, one per line
#   ci.sh hub-delete IMAGE TAG                delete one tag (Docker Hub API)
#   ci.sh delete-rc VERSION IMAGE...          delete every VERSION-rc.* tag of the images
#   ci.sh rc-source IMAGE...                  "retag sha-<7>" or "build <reason>": how the rc image of HEAD (a main merge) is made
#   ci.sh cleanup RELEASE_SHA DRY_RUN IMAGE...  delete sha-<7> dev tags whose commit is an ancestor of RELEASE_SHA,
#                                             plus the buildcache-develop and buildcache cache tags
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

# true when the image $1:$2 exists on Docker Hub
image_exists() { docker buildx imagetools inspect "$1:$2" >/dev/null 2>&1; }

# true when $1 is a higher version than $2 (sort -V)
ver_gt() { [[ "$1" != "$2" && "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n 1)" == "$1" ]]; }

# vX.Y or vX.Y.Z -> vX.Y.Z
norm_ver() {
  [[ "$1" =~ ^v[0-9]+\.[0-9]+$ ]] && echo "$1.0" || echo "$1"
}

# bump_ver vX.Y.Z major|minor|patch
bump_ver() {
  local x y z
  IFS=. read -r x y z <<<"${1#v}"
  case "$2" in
    major) x=$((x + 1)); y=0; z=0 ;;
    minor) y=$((y + 1)); z=0 ;;
    *) z=$((z + 1)) ;;
  esac
  echo "v$x.$y.$z"
}

# "<tag> <normalised version>" of the last release (tags are vX.Y.Z or vX.Y); "none v0.0.0" when there is none
last_release() {
  local t best="" best_n=v0.0.0
  while read -r t; do
    [[ -n "$t" ]] || continue
    local n
    n=$(norm_ver "$t")
    if ver_gt "$n" "$best_n"; then best=$t best_n=$n; fi
  done < <(git tag --list 'v[0-9]*' | grep -E '^v[0-9]+\.[0-9]+(\.[0-9]+)?$' || true)
  echo "${best:-none} $best_n"
}

next_version() {
  local last rcs latest_rc="" n bump=patch since count
  local last_ref
  last=$(last_release)
  last_ref=${last%% *}
  last=${last##* }
  [[ "$last_ref" == none ]] && last_ref=""

  # rc tags of versions above the last release (older ones are leftovers)
  rcs=""
  while read -r t; do
    [[ -n "$t" ]] || continue
    if ver_gt "${t%-rc.*}" "$last"; then rcs+="$t"$'\n'; fi
  done < <(git tag --list 'v*-rc.*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+-rc\.[0-9]+$' || true)
  [[ -n "$rcs" ]] && latest_rc=$(printf '%s' "$rcs" | sort -V | tail -n 1)

  # skip when the tree is the one of the last rc (or of the last release)
  local tree
  tree=$(git rev-parse 'HEAD^{tree}')
  if [[ -n "$latest_rc" && "$(git rev-parse "$latest_rc^{tree}")" == "$tree" && -z "${VERSION:-}" ]]; then
    echo "the tree is unchanged since $latest_rc: no new rc"
    echo "skip=true" >>"${GITHUB_OUTPUT:-/dev/stdout}"
    return 0
  fi
  if [[ -n "$last_ref" && "$(git rev-parse "$last_ref^{tree}")" == "$tree" && -z "${VERSION:-}" ]]; then
    echo "the tree is unchanged since the release $last: no new rc"
    echo "skip=true" >>"${GITHUB_OUTPUT:-/dev/stdout}"
    return 0
  fi

  local version=""
  if [[ -n "${VERSION:-}" ]]; then
    # an explicit version (manual run): X.Y.Z, above the last release
    [[ "$VERSION" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "version $VERSION is not X.Y.Z"
    version="v${VERSION#v}"
    ver_gt "$version" "$last" || die "version $version is not above the last release $last"
    bump=explicit
  elif [[ "${BUMP:-auto}" == major ]]; then
    bump=major
  else
    since=1970-01-01T00:00:00Z
    if [[ -n "$last_ref" ]]; then
      since=$(TZ=UTC git log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ "$last_ref^{commit}")
    fi
    count=$(gh api -X GET search/issues -f q="repo:${GITHUB_REPOSITORY} is:pr is:merged label:minor merged:>$since" --jq .total_count)
    [[ "$count" -gt 0 ]] && bump=minor
    echo "last release $last ($since), merged PRs labelled minor since: $count"
  fi

  if [[ -z "$version" ]]; then
    version=$(bump_ver "$last" "$bump")
    # an rc of a higher version (an earlier major run) keeps its version
    if [[ -n "$latest_rc" ]] && ver_gt "${latest_rc%-rc.*}" "$version"; then
      version=${latest_rc%-rc.*}
    fi
  fi
  n=0
  if [[ -n "$rcs" ]]; then
    while read -r t; do
      if [[ "${t%-rc.*}" == "$version" ]]; then n=$((${t##*-rc.} > n ? ${t##*-rc.} : n)); fi
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
      # the registry layer caches (rebuilt cold by the next PR); the old name "buildcache" goes too
      if [[ "$tag" == buildcache-develop || "$tag" == buildcache ]]; then
        doomed+=("$tag")
        continue
      fi
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

# rc_source image... : how the pre-release image of HEAD (the merge of develop into main) is made. Prints one line:
#   retag sha-<7>   the main tree equals the tree of the develop head (the second parent), and that head has an image
#                   sha-<7> for every image: the rc is a retag of it, no build
#   build <reason>  anything else: a real build
rc_source() {
  local head parent tag image
  head=$(git rev-parse HEAD)
  parent=$(git rev-parse -q --verify "$head^2" 2>/dev/null || true)
  if [[ -z "$parent" ]]; then
    echo "build: HEAD is not a merge commit"
    return 0
  fi
  if [[ "$(git rev-parse "$head^{tree}")" != "$(git rev-parse "$parent^{tree}")" ]]; then
    echo "build: the tree of main differs from the develop head ${parent:0:7}"
    return 0
  fi
  tag="sha-${parent:0:7}"
  for image in "$@"; do
    if ! image_exists "$image" "$tag"; then
      echo "build: $image:$tag does not exist"
      return 0
    fi
  done
  echo "retag $tag"
}

# delete_rc vX.Y.Z image... : delete the rc tags of that version
delete_rc() {
  local version=$1 image tag
  shift
  for image in "$@"; do
    while read -r tag; do
      if [[ "$tag" =~ ^${version//./\\.}-rc\.[0-9]+$ ]]; then hub_delete "$image" "$tag"; fi
    done < <(hub_tags "$image")
  done
}

[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0 2>/dev/null
cmd=${1:-}
shift || true
case "$cmd" in
  sha-tag) echo "sha-$(printf '%s' "$1" | cut -c1-7)" ;;
  exists) image_exists "$1" "$2" ;;
  rc-source) rc_source "$@" ;;
  next-version) next_version ;;
  hub-tags) hub_tags "$1" ;;
  hub-delete) hub_delete "$1" "$2" ;;
  delete-rc) delete_rc "$@" ;;
  cleanup) cleanup "$@" ;;
  *) die "usage: ci.sh sha-tag|exists|next-version|hub-tags|hub-delete|delete-rc|rc-source|cleanup" ;;
esac
