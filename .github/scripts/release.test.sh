#!/usr/bin/env bash
# Tests of release.sh against a faked, stateful gh: .github/scripts/release.test.sh
set -uo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
fail=0
check() { # name expected actual
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: want '$2', got '$3'"; fail=1; fi
}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
mkdir "$tmp/bin" "$tmp/st"
# The fake gh keeps one release in files: exists, draft (true|false), target, asset (content; absent = no asset), calls.
# On a published release an upload or an asset change is refused, as with immutable releases.
cat >"$tmp/bin/gh" <<'GH'
#!/usr/bin/env bash
echo "gh $*" >>"$ST/calls"
[[ "$1" == release ]] || exit 1
sub=$2; shift 2
case "$sub" in
  view)
    [[ ! -f "$ST/flaky" ]] || { echo "HTTP 502: Bad Gateway" >&2; exit 1; }
    [[ -f "$ST/exists" ]] || { echo "release not found" >&2; exit 1; }
    if [[ "$*" == *assets* ]]; then
      if [[ -f "$ST/asset" ]]; then echo "chart.tgz"; fi
    else
      printf '{"isDraft":%s,"targetCommitish":"%s"}\n' "$(cat "$ST/draft")" "$(cat "$ST/target")"
    fi ;;
  create)
    [[ ! -f "$ST/exists" ]] || exit 1
    touch "$ST/exists"; echo true >"$ST/draft"
    while [[ $# -gt 0 ]]; do [[ "$1" == --target ]] && echo "$2" >"$ST/target"; shift; done ;;
  upload)
    [[ "$(cat "$ST/draft")" == true ]] || { echo "HTTP 422: immutable release" >&2; exit 1; }
    cp "$2" "$ST/asset" ;;
  download)
    [[ "$*" == *--dir* ]] || exit 1
    dir=""; while [[ $# -gt 0 ]]; do [[ "$1" == --dir ]] && dir=$2; shift; done
    cp "$ST/asset" "$dir/chart.tgz" ;;
  edit)
    [[ "$(cat "$ST/draft")" == true ]] || { echo "HTTP 422: immutable release" >&2; exit 1; }
    echo false >"$ST/draft"; echo "$*" >"$ST/edit" ;;
esac
exit 0
GH
chmod +x "$tmp/bin/gh"
export PATH=$tmp/bin:$PATH ST=$tmp/st
cd "$tmp" || exit 1
git init -q -b main repo && cd repo || exit 1
git commit -q --allow-empty -m a
sha=$(git rev-parse HEAD)
git commit -q --allow-empty -m b
other=$(git rev-parse HEAD)
git tag v1.10.0 "$sha"
echo "chart bytes" >"$tmp/chart.tgz"
mv "$tmp/chart.tgz" "$tmp/chart.tgz.src"; mkdir "$tmp/a"; cp "$tmp/chart.tgz.src" "$tmp/a/chart.tgz"
asset=$tmp/a/chart.tgz
reset() { rm -f "$ST"/*; : >"$ST/calls"; }
run() { "$here/release.sh" v1.10.0 "$sha" "${1:-true}" "$asset" 2>&1; }
calls() { grep -c "$1" "$ST/calls" || true; }

# 1. nothing yet: draft, upload, publish, in this order
reset
out=$(run); rc=$?
check "new: succeeds" 0 "$rc"
check "new: created as a draft" 1 "$(calls 'release create v1.10.0 --draft --target')"
check "new: order is create, upload, edit" "create upload edit" "$(grep -oE 'release (create|upload|edit)' "$ST/calls" | cut -d' ' -f2 | tr '\n' ' ' | sed 's/ $//')"
check "new: published at the end with latest" 1 "$(grep -c -- '--draft=false --latest=true' "$ST/edit")"
check "new: the asset is there" "chart bytes" "$(cat "$ST/asset")"
: "$out"

# 2. latest=false is passed through
reset; run false >/dev/null
check "latest=false is applied at publication" 1 "$(grep -c -- '--latest=false' "$ST/edit")"

# 3. a re-run after the draft was created but nothing uploaded
reset; touch "$ST/exists"; echo true >"$ST/draft"; echo "$sha" >"$ST/target"
run >/dev/null; rc=$?
check "draft without asset: continues" "0 false" "$rc $(cat "$ST/draft")"
check "draft without asset: no second create" 0 "$(calls 'release create')"
check "draft without asset: uploaded" 1 "$(calls 'release upload')"

# 4. a draft that already has the same asset: no upload, still published
reset; touch "$ST/exists"; echo true >"$ST/draft"; echo "$sha" >"$ST/target"; cp "$asset" "$ST/asset"
run >/dev/null
check "draft with the asset: not uploaded again" 0 "$(calls 'release upload')"
check "draft with the asset: published" false "$(cat "$ST/draft")"

# 5. a draft with a different (partial) asset: replaced
reset; touch "$ST/exists"; echo true >"$ST/draft"; echo "$sha" >"$ST/target"; echo partial >"$ST/asset"
run >/dev/null
check "draft with another asset: replaced" "chart bytes" "$(cat "$ST/asset")"

# 6. a draft that targets another commit: refused, nothing published
reset; touch "$ST/exists"; echo true >"$ST/draft"; echo "$other" >"$ST/target"
run >/dev/null; rc=$?
check "draft at another commit: refused" "1 true" "$rc $(cat "$ST/draft")"

# 7. published and complete: verified, nothing changes
reset; touch "$ST/exists"; echo false >"$ST/draft"; echo "$sha" >"$ST/target"; cp "$asset" "$ST/asset"
run >/dev/null; rc=$?
check "published and equal: ok" 0 "$rc"
check "published: never uploaded or edited" 0 "$(( $(calls 'release upload') + $(calls 'release edit') + $(calls 'release create') ))"

# 8. published without the asset: refused, never uploaded
reset; touch "$ST/exists"; echo false >"$ST/draft"; echo "$sha" >"$ST/target"
run >/dev/null; rc=$?
check "published without the asset: refused" 1 "$rc"
check "published without the asset: no upload attempted" 0 "$(calls 'release upload')"

# 9. published with a different asset: refused
reset; touch "$ST/exists"; echo false >"$ST/draft"; echo "$sha" >"$ST/target"; echo other >"$ST/asset"
run >/dev/null; rc=$?
check "published with another asset: refused" 1 "$rc"

# 10. published but the tag is at another commit: refused
reset; touch "$ST/exists"; echo false >"$ST/draft"; echo "$sha" >"$ST/target"; cp "$asset" "$ST/asset"
git tag -f v1.10.0 "$other" >/dev/null
run >/dev/null; rc=$?
check "published, tag at another commit: refused" 1 "$rc"
# a failed lookup is not "no release": no second draft of the same tag is made
reset
touch "$ST/exists" "$ST/flaky"; echo true >"$ST/draft"; echo "$sha" >"$ST/target"
out=$(run); rc=$?
check "flaky lookup: fails" 1 "$rc"
check "flaky lookup: nothing was created" 0 "$(calls 'release create')"
check "flaky lookup: nothing was uploaded or published" 0 "$(calls 'release upload')"

exit $fail
