#!/usr/bin/env bash
# Tests of ci.sh: .github/scripts/ci.test.sh (needs git only; the registry is faked).
set -uo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
fail=0
check() { # name expected actual
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: want '$2', got '$3'"; fail=1; fi
}
# shellcheck source=/dev/null
source "$here/ci.sh"
set +e

check "sha-tag" sha-abcdef0 "$("$here/ci.sh" sha-tag abcdef0123456789)"

# --- the registry is a fake: a file per tag holding "digest revision"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
reg=$tmp/reg
mkdir "$reg"
# shellcheck disable=SC2329
image_exists() { [[ -f "$reg/$(tr '/:' '__' <<<"$1:$2")" ]]; }
# shellcheck disable=SC2329
image_digest() { cut -d' ' -f1 "$reg/$(tr '/:' '__' <<<"$1:$2")"; }
# shellcheck disable=SC2329
image_revision() { cut -s -d' ' -f2 "$reg/$(tr '/:' '__' <<<"$1:$2")"; }
# shellcheck disable=SC2329
retry() { # retag's "docker buildx imagetools create -t IMG:DST IMG:SRC"
  [[ "$1 $2 $3" == "docker buildx imagetools" ]] || { "$@"; return; }
  local dst=$6 src=$7
  cp "$reg/$(tr '/:' '__' <<<"$src")" "$reg/$(tr '/:' '__' <<<"$dst")"
  echo created "$dst"
}
put() { echo "$3" >"$reg/$(tr '/:' '__' <<<"$1:$2")"; }

check "state: missing" missing "$(image_state img v1.0.0-rc.1 sha1 2>/dev/null)"
put img v1.0.0-rc.1 "sha256:aa sha1"
check "state: same revision" same "$(image_state img v1.0.0-rc.1 sha1 2>/dev/null)"
out=$(image_state img v1.0.0-rc.1 sha2 2>/dev/null); rc=$?
check "state: another revision is refused" "different 1" "$out $rc"
put img v1.0.0-rc.2 "sha256:bb"
out=$(image_state img v1.0.0-rc.2 sha1 2>/dev/null); rc=$?
check "state: no revision label is refused" "different 1" "$out $rc"

# retag
retag img v1.0.0-rc.1 v1.0.0 >/dev/null 2>&1
check "retag: the version is created" "sha256:aa" "$(image_digest img v1.0.0)"
out=$(retag img v1.0.0-rc.1 v1.0.0 2>&1); rc=$?
check "retag: the same version again is a no-op" "0 1" "$rc $(grep -c 'already' <<<"$out")"
put img v1.0.1 "sha256:zz"
out=$(retag img v1.0.0-rc.1 v1.0.1 2>&1); rc=$?
check "retag: a different digest under an existing version is refused" "1" "$rc"
put img latest "sha256:old"
retag img v1.0.0-rc.1 latest >/dev/null 2>&1
check "retag: latest moves" "sha256:aa" "$(image_digest img latest)"
retag img v1.0.0-rc.1 v1.0.0 >/dev/null 2>&1
check "retag: a rate-limit-free run leaves the version alone" "sha256:aa" "$(image_digest img v1.0.0)"

# --- delete_matching and cleanup in a repository with a develop branch merged into main
# shellcheck disable=SC2329
hub_tags() { printf '%s\n' v1.2.3-rc.1 v1.2.3-rc.2 v1.2.30-rc.1 v1.2.3 sha-abcdef0 latest; }
# shellcheck disable=SC2329
hub_delete() { echo "del $1 $2"; }
got=$(delete_matching false 'v1\.2\.3-rc\.[0-9]+' img | tr '\n' ' ' | sed 's/ $//')
check "delete-matching: only whole matches" "del img v1.2.3-rc.1 del img v1.2.3-rc.2" "$got"
check "delete-matching: dry run deletes nothing" "0" "$(delete_matching true 'v1\.2\.3-rc\.[0-9]+' img | grep -c '^del')"
check "delete-matching: status with a non-matching last tag" 0 "$(delete_matching false 'nomatch' img >/dev/null; echo $?)"

mkdir "$tmp/r2" && cd "$tmp/r2" || exit 1
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
git init -q -b main .
commit() { echo "$1" >"$1"; git add "$1"; git commit -qm "$1"; }
commit m0
git checkout -q -b develop
commit d1
d1=$(git rev-parse HEAD)
git checkout -q main
git merge -q --no-ff -m merge develop
release=$(git rev-parse HEAD)
git checkout -q develop
commit d2
newer=$(git rev-parse HEAD)
# shellcheck disable=SC2329
hub_tags() { printf '%s\n' "sha-${d1:0:7}" "sha-${newer:0:7}" sha-0000000 buildcache-develop buildcache latest v1.0.0 v1.0.0-rc.1 sha-ABCDEF0; }
# shellcheck disable=SC2329
hub_delete() { echo "del $2"; }
want="del sha-${d1:0:7} del buildcache-develop del buildcache"
check "cleanup" "$want" "$(cleanup "$release" false img | grep '^del' | tr '\n' ' ' | sed 's/ $//')"
check "cleanup dry run deletes nothing" "" "$(cleanup "$release" true img | grep '^del')"

# --- retry: a 429 is retried, any other failure is final
unset -f retry
# shellcheck source=/dev/null
source "$here/ci.sh"
export RETRY_DELAYS="0 0"
n="$tmp/n"
: >"$n"
# shellcheck disable=SC2329
flaky() { echo x >>"$n"; if [[ $(wc -l <"$n") -lt $1 ]]; then echo "429 Too Many Requests"; return 1; fi; echo finished; }
check "retry: succeeds after two 429" "finished 3" "$(retry flaky 3 | tr '\n' ' ' | sed 's/ $//') $(wc -l <"$n" | tr -d ' ')"
: >"$n"
rc=0; retry flaky 99 >/dev/null || rc=$?; check "retry: gives up after the delays (3 runs)" "1 3" "$rc $(wc -l <"$n" | tr -d ' ')"
: >"$n"
# shellcheck disable=SC2329
broken() { echo x >>"$n"; echo "manifest unknown"; return 1; }
rc=0; retry broken >/dev/null || rc=$?; check "retry: another error is not retried" "1 1" "$rc $(wc -l <"$n" | tr -d ' ')"
unset RETRY_DELAYS

exit $fail
