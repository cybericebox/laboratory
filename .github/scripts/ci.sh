#!/usr/bin/env bash
# CI helpers of the release workflows (.github/workflows). Docker Hub calls use DOCKERHUB_USERNAME and DOCKERHUB_TOKEN (Read/Write/Delete).
#   ci.sh sha-tag SHA                          sha-<7 hex>
#   ci.sh exists IMAGE TAG                     exit 0 when IMAGE:TAG exists in the registry
#   ci.sh retry CMD...                         run CMD again after a Docker Hub rate limit (429)
#   ci.sh image-state IMAGE TAG SHA            missing | same | different: whether the immutable tag exists, and whether it was built from SHA
#                                              (the org.opencontainers.image.revision label); exit 1 for "different"
#   ci.sh retag IMAGE SRC DST                  DST becomes another name of SRC (no rebuild). A version tag is immutable: when it exists
#                                              it must be the same digest (nothing happens), otherwise exit 1. Any other tag (latest) moves.
#   ci.sh chart CHART_DIR VERSION APP_VERSION OUT   package the Helm chart and push it to oci://registry-1.docker.io/cybericebox (the tgz goes
#                                              to OUT, as pulled back from the registry). A version that exists must hold the same chart (nothing happens), otherwise exit 1.
#   ci.sh hub-tags IMAGE                       every tag of a Docker Hub repository, one per line
#   ci.sh hub-delete IMAGE TAG                 delete one tag
#   ci.sh delete-matching DRY_RUN REGEX IMAGE...  delete the tags that match the whole REGEX (ERE) in every repository
#   ci.sh cleanup RELEASE_SHA DRY_RUN IMAGE...  delete sha-<7> tags whose commit is an ancestor of RELEASE_SHA, and the buildcache tags
#   hub-delete, delete-matching and cleanup go on after a refused deletion (an immutable tag may be refused), list the refusals in hub_failed and exit 1 at the end
set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }

VERSION_TAG='^v?[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$' # what the registry keeps immutable
REVISION_LABEL=org.opencontainers.image.revision

hub_jwt=""
hub_login() {
  [[ -n "$hub_jwt" ]] && return 0
  [[ -n "${DOCKERHUB_USERNAME:-}" && -n "${DOCKERHUB_TOKEN:-}" ]] || die "DOCKERHUB_USERNAME and DOCKERHUB_TOKEN are required"
  hub_jwt=$(jq -n --arg u "$DOCKERHUB_USERNAME" --arg p "$DOCKERHUB_TOKEN" '{username: $u, password: $p}' |
    curl -fsS -X POST -H 'Content-Type: application/json' -d @- https://hub.docker.com/v2/users/login | jq -r .token) || die "Docker Hub login failed"
  [[ -n "$hub_jwt" && "$hub_jwt" != null ]] || die "Docker Hub login failed"
}

hub_tags() {
  hub_login
  local url="https://hub.docker.com/v2/repositories/$1/tags?page_size=100"
  while [[ -n "$url" && "$url" != null ]]; do
    local page
    # Explicit checks: this runs inside $(...) and < <(...), where set -e does not apply, so a failed page would be read as "no more tags".
    page=$(curl -fsS -H "Authorization: JWT $hub_jwt" "$url") || die "Docker Hub: the list of tags of $1 failed ($url)"
    jq -r '.results[].name' <<<"$page" || die "Docker Hub: the list of tags of $1 is not JSON"
    url=$(jq -r '.next // empty' <<<"$page") || die "Docker Hub: no next page in the list of tags of $1"
  done
}

# Deletions that the registry refused or that failed: "IMAGE:TAG (HTTP code)", for the caller to report. An immutable tag may be refused.
hub_failed=()
hub_delete() {
  local code
  hub_login
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: JWT $hub_jwt" "https://hub.docker.com/v2/repositories/$1/tags/$2/") || code=000
  case "$code" in
    2??) echo "deleted $1:$2" ;;
    404) echo "$1:$2 is already gone" ;;
    *)
      hub_failed+=("$1:$2 (HTTP $code)")
      echo "::warning::could not delete $1:$2 (HTTP $code)" >&2
      return 1
      ;;
  esac
}

# retry CMD...: runs the command; when it fails with a Docker Hub rate limit (429 Too Many Requests) it is run again after
# 60, 120 and 240 seconds (RETRY_DELAYS overrides the list). Any other failure is final. The output is printed once, at the end.
retry() {
  local delay out rc
  for delay in ${RETRY_DELAYS:-60 120 240} last; do
    rc=0
    out=$("$@" 2>&1) || rc=$?
    if [[ "$rc" -eq 0 ]] || ! grep -qiE '429|too many requests|toomanyrequests' <<<"$out" || [[ "$delay" == last ]]; then
      [[ -z "$out" ]] || printf '%s\n' "$out"
      return "$rc"
    fi
    echo "::warning::rate limited (429), retry in ${delay}s: $*" >&2
    sleep "$delay"
  done
}

# true when the image $1:$2 exists on Docker Hub, false when the registry says it does not (a rate limit is retried). Any other failure
# (network, authentication, 5xx) stops the script: it is never read as "missing", which would let a push or a retag go on blindly.
image_exists() {
  local out
  if out=$(retry docker buildx imagetools inspect "$1:$2" 2>&1); then return 0; fi
  if grep -qiE 'not found|manifest unknown|name unknown|\b404\b' <<<"$out"; then return 1; fi
  die "cannot tell whether $1:$2 exists: $(head -c 300 <<<"$out")"
}

# the digest of the manifest (list) behind $1:$2
image_digest() { retry docker buildx imagetools inspect "$1:$2" --format '{{json .Manifest}}' | jq -r .digest; }

# the revision label of $1:$2, read from its linux image (an index has no labels of its own); empty when it has none
image_revision() {
  local raw digest
  raw=$(retry docker buildx imagetools inspect "$1:$2" --raw)
  digest=$(jq -r '(if .manifests then [.manifests[] | select(.platform.os == "linux")][0].digest else empty end) // empty' <<<"$raw")
  if [[ -n "$digest" ]]; then
    retry docker buildx imagetools inspect "$1@$digest" --format '{{json .Image}}'
  else
    retry docker buildx imagetools inspect "$1:$2" --format '{{json .Image}}'
  fi | jq -r --arg l "$REVISION_LABEL" '(.config.Labels // .Config.Labels // {})[$l] // empty'
}

# image_state IMAGE TAG SHA: missing | same | different (exit 1)
image_state() {
  local rev
  if ! image_exists "$1" "$2"; then
    echo missing
    return 0
  fi
  rev=$(image_revision "$1" "$2")
  if [[ "$rev" == "$3" ]]; then
    echo same
  else
    echo different
    echo "::error::$1:$2 exists but was built from '${rev:-unknown}', not from $3: a version tag is never pushed twice" >&2
    return 1
  fi
}

# retag IMAGE SRC DST
retag() {
  local image=$1 src=$2 dst=$3 a b
  if [[ "$dst" =~ $VERSION_TAG ]] && image_exists "$image" "$dst"; then
    a=$(image_digest "$image" "$src")
    b=$(image_digest "$image" "$dst")
    [[ -n "$a" && "$a" == "$b" ]] || die "$image:$dst exists and is not $image:$src ($b, not $a): a version tag is never pushed twice"
    echo "$image:$dst is already $image:$src"
    return 0
  fi
  retry docker buildx imagetools create -t "$image:$dst" "$image:$src"
  echo "$image:$dst is now $image:$src"
}

# chart CHART_DIR VERSION APP_VERSION OUT
chart() {
  local dir=$1 version=$2 app=$3 out=$4 oci=oci://registry-1.docker.io/cybericebox tmp
  tmp=$(mktemp -d)
  mkdir -p "$out"
  helm package "$dir" --version "$version" --app-version "$app" --destination "$out" >/dev/null
  if helm pull "$oci/laboratory" --version "$version" --untar --untardir "$tmp/remote" >/dev/null 2>&1; then
    mkdir -p "$tmp/local"
    tar -xzf "$out/laboratory-$version.tgz" -C "$tmp/local"
    if diff -r "$tmp/local/laboratory" "$tmp/remote/laboratory" >/dev/null; then
      echo "chart $version is already in the registry and identical"
    else
      rm -rf "$tmp"
      die "chart $version exists in the registry and differs from this one: a version tag is never pushed twice"
    fi
  else
    retry helm push "$out/laboratory-$version.tgz" "$oci"
  fi
  # The file of OUT is the registry's own copy, byte for byte: packaging twice does not give the same bytes, and the release asset must
  # be the very chart that the registry holds (publish_release compares them by digest on a re-run).
  retry helm pull "$oci/laboratory" --version "$version" --destination "$tmp/pulled" >/dev/null
  cp "$tmp/pulled/laboratory-$version.tgz" "$out/laboratory-$version.tgz"
  rm -rf "$tmp"
}

# delete_matching DRY_RUN REGEX IMAGE...
delete_matching() {
  local dry=$1 re=$2 image tag tags failed=0
  shift 2
  for image in "$@"; do
    tags=$(hub_tags "$image") || return 1 # not inside < <(...): a failed listing must stop, not look like an empty repository
    while read -r tag; do
      [[ -n "$tag" ]] || continue
      if [[ "$tag" =~ ^($re)$ ]]; then
        if [[ "$dry" == true ]]; then echo "would delete $image:$tag"; else hub_delete "$image" "$tag" || failed=1; fi
      fi
    done <<<"$tags"
  done
  return "$failed"
}

# cleanup RELEASE_SHA DRY_RUN IMAGE...: the dev images of builds that are part of a release, and the registry layer caches
cleanup() {
  local release_sha=$1 dry=$2
  shift 2
  local image tag full tags failed=0
  for image in "$@"; do
    echo "== $image"
    local doomed=()
    tags=$(hub_tags "$image") || return 1
    while read -r tag; do
      if [[ "$tag" == buildcache-develop || "$tag" == buildcache-release || "$tag" == buildcache ]]; then
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
    done <<<"$tags"
    echo "to delete from $image: ${#doomed[@]}"
    printf '  %s\n' "${doomed[@]}"
    [[ "$dry" == true ]] && continue
    for tag in "${doomed[@]}"; do
      hub_delete "$image" "$tag" || failed=1
    done
  done
  [[ "$dry" == true ]] && echo "dry run: nothing deleted"
  return "$failed"
}

[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0 2>/dev/null
cmd=${1:-}
shift || true
case "$cmd" in
  sha-tag) echo "sha-$(printf '%s' "$1" | cut -c1-7)" ;;
  exists) image_exists "$1" "$2" ;;
  retry) retry "$@" ;;
  image-state) image_state "$1" "$2" "$3" ;;
  retag) retag "$1" "$2" "$3" ;;
  chart) chart "$1" "$2" "$3" "$4" ;;
  hub-tags) hub_tags "$1" ;;
  hub-delete) hub_delete "$1" "$2" ;;
  delete-matching) delete_matching "$@" ;;
  cleanup) cleanup "$@" ;;
  *) die "usage: ci.sh sha-tag|exists|retry|image-state|retag|chart|hub-tags|hub-delete|delete-matching|cleanup" ;;
esac
