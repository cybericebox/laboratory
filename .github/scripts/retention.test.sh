#!/usr/bin/env bash
# Tests of retention.sh: .github/scripts/retention.test.sh (git only; the registry and gh are faked).
set -uo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
fail=0
check() { # name expected actual
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: want '$2', got '$3'"; fail=1; fi
}
# shellcheck source=/dev/null
source "$here/retention.sh"
set +e
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp" || exit 1
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
git init -q -b main .
git commit -q --allow-empty -m a
a=$(git rev-parse HEAD)
git commit -q --allow-empty -m b
for t in v1.9.0 v1.10.0-rc.1 v1.10.0-rc.2 v1.10.0 v1.10.1-rc.1 v1.10.1 v1.10.2-rc.1 v1.11.0 v1.11.1 v1.12.0-rc.1; do git tag "$t"; done
git update-ref refs/remotes/origin/main HEAD
export IMAGES="img1 img2" CHART_REPO=chart PRUNE_FROM=1.10 PRUNE_KEEP=10 MAIN_REF=origin/main

# shellcheck disable=SC2329
hub_tags() {
  case "$1" in
    chart) printf '%s\n' 1.10.0-rc.1 1.10.0-rc.2 1.10.0 1.10.1-rc.1 1.10.1 1.10.2-rc.1 1.11.0 1.11.1 1.12.0-rc.1 ;;
    *) printf '%s\n' v1.10.0-rc.1 v1.10.0-rc.2 v1.10.0 v1.10.1-rc.1 v1.10.1 v1.10.2-rc.1 v1.11.0 v1.11.1 v1.12.0-rc.1 v1.9.0 "sha-${a:0:7}" latest ;;
  esac
}
# shellcheck disable=SC2329
hub_delete() { echo "del $1 $2"; }
# shellcheck disable=SC2329
gh() { echo "gh $*"; }
# shellcheck disable=SC2329
git() { if [[ "$1 $2" == "fetch --tags" ]]; then return 0; fi; command git "$@"; }

DRY_RUN=false
out=$(retention 2>&1)
dels=$(grep -E '^(del|gh)' <<<"$out" | tr '\n' '|')
for want in "del img1 v1.10.0-rc.1" "del img2 v1.10.0-rc.2" "del chart 1.10.0-rc.1" "del chart 1.10.0-rc.2" \
  "gh release delete v1.10.0-rc.1 --cleanup-tag --yes" "gh release delete v1.10.0-rc.2 --cleanup-tag --yes" "gh release delete v1.10.1-rc.1 --cleanup-tag --yes" \
  "del img1 v1.10.0|" "del chart 1.10.0|" "gh release delete v1.10.0 --yes" "del img1 sha-${a:0:7}"; do
  case "$dels" in *"$want"*) check "deletes: $want" ok ok ;; *) check "deletes: $want" present "missing in: $dels" ;; esac
done
for keep in "v1.10.2-rc.1" "v1.12.0-rc.1" "v1.10.1|" "v1.11.0 " "v1.11.1" "v1.9.0" "latest" "1.12.0-rc.1"; do
  case "$dels" in *"$keep"*) check "keeps: $keep" kept "deleted in: $dels" ;; *) check "keeps: $keep" ok ok ;; esac
done
case "$dels" in *"gh release delete v1.10.0 --cleanup-tag"*) check "a pruned final release keeps its git tag" kept deleted ;; *) check "a pruned final release keeps its git tag" ok ok ;; esac
check "the git tag of a pruned release is still there" v1.10.0 "$(command git tag --list v1.10.0)"

# shellcheck disable=SC2034 # read by retention()
DRY_RUN=true
out=$(retention 2>&1)
check "dry run deletes nothing" 0 "$(grep -cE '^(del |gh )' <<<"$out")"
check "dry run lists" 1 "$(grep -c 'would delete img1:v1.10.0-rc.1' <<<"$out")"

# --- a final tag that is not on main (made by hand) changes nothing
command git checkout -q --detach HEAD
command git commit -q --allow-empty -m side
command git tag v1.99.0
command git checkout -q main
DRY_RUN=false
out=$(retention 2>&1)
check "a final tag outside main is ignored (v1.11.0 is not pruned)" 0 "$(grep -c '^del img1 v1.11.0$' <<<"$out")"
check "the ignored tag is said" 1 "$(grep -c 'ignored: v1.99.0' <<<"$out")"
command git tag -d v1.99.0 >/dev/null

# --- a refused deletion: the rest goes on, the tag of that candidate stays, it is reported, the exit status follows STRICT
# shellcheck disable=SC2329
hub_delete() { if [[ "$1 $2" == "img1 v1.10.0-rc.1" ]]; then hub_failed+=("img1:v1.10.0-rc.1 (HTTP 403)"); return 1; fi; echo "del $1 $2"; }
# shellcheck disable=SC2034 # read by retention()
DRY_RUN=false STRICT=false
out=$(retention 2>&1); rc=$?
check "refused: the other images are still deleted" 1 "$(grep -c '^del img2 v1.10.0-rc.1$' <<<"$out")"
check "refused: later candidates are still handled" 1 "$(grep -c '^del img1 v1.10.0-rc.2$' <<<"$out")"
check "refused: the git tag and page of that candidate stay" 0 "$(grep -c 'gh release delete v1.10.0-rc.1 ' <<<"$out")"
check "refused: it is reported" 1 "$(grep -c '::warning::retention could not delete img1:v1.10.0-rc.1 (HTTP 403)' <<<"$out")"
check "refused: not strict, the exit status is 0 (the release is already public)" 0 "$rc"
STRICT=true retention >/dev/null 2>&1; check "refused: strict, the exit status is 1" 1 "$?"
summary=$tmp/summary
GITHUB_STEP_SUMMARY=$summary STRICT=false retention >/dev/null 2>&1
check "refused: the step summary lists it" 1 "$(grep -c 'img1:v1.10.0-rc.1' "$summary")"

# --- a final tag is never deleted or moved, even if a caller asks for it
: >"$tmp/gh.calls"
# shellcheck disable=SC2329
gh() { echo "gh $*" >>"$tmp/gh.calls"; }
# shellcheck disable=SC2329
git() { if [[ "$1" == push ]]; then echo "git $*" >>"$tmp/gh.calls"; return 0; fi; command git "$@"; }
# shellcheck disable=SC2034
DRY_RUN=false
out=$(drop_release v1.11.0 true)
check "drop_release refuses the git tag of a final release" 1 "$(grep -c 'refusing to delete the git tag' <<<"$out")"
check "drop_release of a final tag runs no gh and no git push" 0 "$(wc -l <"$tmp/gh.calls" | tr -d ' ')"
drop_release v1.12.0-rc.1 true >/dev/null
check "a candidate tag may go with its page" 1 "$(grep -c 'release delete v1.12.0-rc.1 --cleanup-tag' "$tmp/gh.calls")"
# the whole run: no command ever deletes or moves a final tag
DRY_RUN=false
: >"$tmp/gh.calls"
retention >/dev/null 2>&1
check "retention never deletes the tag of a final release" 0 "$(grep -cE 'release delete v[0-9.]+ --cleanup-tag|push origin --delete refs/tags/v[0-9.]+$|tag -f|push.*--force' "$tmp/gh.calls")"
exit $fail
