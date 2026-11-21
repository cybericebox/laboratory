#!/usr/bin/env bash
# Publishes the GitHub release of a promoted version so that it survives "immutable releases" (a published release cannot get new assets).
#   release.sh VERSION SHA LATEST ASSET     environment: GH_TOKEN; run in a clone that has the tags (git tag lookup of a published release)
# The release is made as a DRAFT, gets its asset (the chart), and is published last, so a published release is complete. Every state is
# safe to run again:
#   no release       create the draft at SHA, upload the asset, publish
#   a draft          it must target SHA; upload the asset when it is missing or differs (a draft may change), publish
#   published        nothing is changed or uploaded: the tag must be at SHA and the asset must be there with the same sha256 as ASSET,
#                    otherwise exit 1
# LATEST (true|false) is applied when the draft is published; a published release is never edited, so the page and its notes stay as they were.
set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }

sha256_of() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

# the sha256 of the asset NAME of release TAG ("" when it has none)
remote_asset_sha() {
  local tag=$1 name=$2 dir
  gh release view "$tag" --json assets --jq ".assets[].name" | grep -qx "$name" || return 0
  dir=$(mktemp -d)
  gh release download "$tag" --pattern "$name" --dir "$dir"
  sha256_of "$dir/$name"
  rm -rf "$dir"
}

publish_release() {
  local version=$1 sha=$2 latest=$3 asset=$4 name view draft target at have want
  name=$(basename "$asset")
  [[ -f "$asset" ]] || die "$asset does not exist"
  want=$(sha256_of "$asset")
  err=$(mktemp)
  if view=$(gh release view "$version" --json isDraft,targetCommitish 2>"$err"); then
    draft=$(jq -r .isDraft <<<"$view")
    target=$(jq -r .targetCommitish <<<"$view")
  elif grep -qi 'release not found' "$err"; then
    # Only "not found" means there is none: any other failure (network, 5xx, permission) must not make a second draft of the same tag.
    gh release create "$version" --draft --target "$sha" --title "$version" --generate-notes
    echo "draft release $version created"
    draft=true
    target=$sha
  else
    cat "$err" >&2
    rm -f "$err"
    die "cannot tell whether release $version exists"
  fi
  rm -f "$err"
  if [[ "$draft" == true ]]; then
    [[ "$target" == "$sha" ]] || die "the draft $version targets $target, not $sha"
    have=$(remote_asset_sha "$version" "$name")
    if [[ "$have" == "$want" ]]; then
      echo "the draft already has $name"
    else
      gh release upload "$version" "$asset" --clobber
      echo "uploaded $name"
    fi
    gh release edit "$version" --draft=false --latest="$latest"
    echo "release $version is published"
    return 0
  fi
  # published: verify only
  at=$(git rev-parse "refs/tags/$version^{commit}") || die "release $version is published but its tag is not here"
  [[ "$at" == "$sha" ]] || die "release $version exists at ${at:0:7}, not at ${sha:0:7}"
  have=$(remote_asset_sha "$version" "$name")
  [[ -n "$have" ]] || die "release $version is published without $name: a published release cannot get assets; it must be re-created by hand"
  [[ "$have" == "$want" ]] || die "release $version has a different $name (sha256 ${have:0:12}, expected ${want:0:12})"
  echo "release $version is published and complete"
}

[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0 2>/dev/null
[[ $# -eq 4 ]] || die "usage: release.sh VERSION SHA LATEST ASSET"
publish_release "$@"
