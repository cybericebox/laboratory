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
exit $fail
