#!/usr/bin/env bash
# Tests of the version logic of ci.sh: .github/scripts/ci.test.sh (needs git only; gh is faked).
set -uo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ci=$here/ci.sh
fail=0
check() { # name expected actual
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: want '$2', got '$3'"; fail=1; fi
}

# shellcheck source=/dev/null
source "$ci"
check "norm vX.Y" v1.0.0 "$(norm_ver v1.0)"
check "norm vX.Y.Z" v1.2.3 "$(norm_ver v1.2.3)"
check "bump patch" v1.2.4 "$(bump_ver v1.2.3 patch)"
check "bump minor" v1.3.0 "$(bump_ver v1.2.3 minor)"
check "bump major" v2.0.0 "$(bump_ver v1.2.3 major)"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp" || exit 1
git init -q .
mkdir bin
# shellcheck disable=SC2016
printf '#!/bin/sh\n[ -n "${FAKE_SHAS:-}" ] && printf "%%s\\n" $FAKE_SHAS\nexit 0\n' >bin/gh
chmod +x bin/gh
export PATH=$tmp/bin:$PATH GITHUB_REPOSITORY=x/y GITHUB_OUTPUT=$tmp/out
g() { git -c user.email=a@b -c user.name=n -c tag.gpgsign=false -c commit.gpgsign=false "$@"; }
commit() { echo "$1" >f; g add f; g commit -qm "$1"; }
run() { : >"$tmp/out"; "$ci" next-version >/dev/null 2>&1; tr '\n' ' ' <"$tmp/out"; }

commit a
check "no tags: first rc" "skip=false version=v0.0.1 tag=v0.0.1-rc.1 " "$(run)"
check "no tags: explicit" "skip=false version=v0.1.0 tag=v0.1.0-rc.1 " "$(VERSION=0.1.0 run)"
g tag v0.9
g tag v1.0
commit b
check "vX.Y tags: last is 1.0.0" "skip=false version=v1.0.1 tag=v1.0.1-rc.1 " "$(run)"
check "explicit 2.0.0" "skip=false version=v2.0.0 tag=v2.0.0-rc.1 " "$(VERSION=2.0.0 run)"
check "explicit not above the last release" "" "$(VERSION=1.0.0 run)"
check "explicit not X.Y.Z" "" "$(VERSION=2.0 run)"
check "minor label" "skip=false version=v1.1.0 tag=v1.1.0-rc.1 " "$(FAKE_SHAS=0123456789abcdef0123456789abcdef01234567 run)"
check "minor PR already in the last release" "skip=false version=v1.0.1 tag=v1.0.1-rc.1 " "$(FAKE_SHAS=$(git rev-parse v1.0) run)"
check "major" "skip=false version=v2.0.0 tag=v2.0.0-rc.1 " "$(BUMP=major run)"
g tag v1.0.1-rc.1
check "same tree as the last rc" "skip=true " "$(run)"
commit c
check "next rc of the same version" "skip=false version=v1.0.1 tag=v1.0.1-rc.2 " "$(run)"
g tag v1.0.1
g tag v1.1.0
commit d
check "vX.Y.Z after vX.Y" "skip=false version=v1.1.1 tag=v1.1.1-rc.1 " "$(run)"

# delete_rc exits 0 when the last tag does not match, and deletes only the rc tags of the version
# shellcheck disable=SC2329
hub_tags() { printf '%s\n' v1.2.3-rc.1 v1.2.3-rc.2 v1.2.30-rc.1 sha-abcdef0 latest; }
# shellcheck disable=SC2329
hub_delete() { echo "del $1 $2"; }
out=$(delete_rc v1.2.3 img); rc=$?
check "delete-rc status with a non-matching last tag" 0 "$rc"
check "delete-rc deletes only the rc tags of the version" "del img v1.2.3-rc.1 del img v1.2.3-rc.2" "$(tr '\n' ' ' <<<"$out" | sed 's/ $//')"

# rc_source and cleanup in a repository with a develop branch merged into main
mkdir "$tmp/r2" && cd "$tmp/r2" || exit 1
git init -q -b main .
commit() { echo "$1" >"$1"; g add "$1"; g commit -qm "$1"; }
commit m0
g checkout -q -b develop
commit d1
d1=$(git rev-parse HEAD)
g checkout -q main
g merge -q --no-ff -m merge develop
# shellcheck disable=SC2329
image_exists() { [[ "$fake_exists" == yes ]]; }
fake_exists=yes
check "rc_source: same tree, images exist" "retag sha-${d1:0:7}" "$(rc_source img1 img2)"
fake_exists=no
check "rc_source: image missing" "build: img1:sha-${d1:0:7} does not exist" "$(rc_source img1)"
fake_exists=yes
release=$(git rev-parse HEAD)
commit m1
check "rc_source: not a merge commit" "build: HEAD is not a merge commit" "$(rc_source img1)"
g checkout -q develop
commit d2
g checkout -q main
commit m2
g merge -q --no-ff -m merge2 develop
case "$(rc_source img1)" in "build: the tree of main differs"*) check "rc_source: tree differs" ok ok ;; *) check "rc_source: tree differs" build "$(rc_source img1)" ;; esac

# cleanup deletes the ancestor sha tags and the cache tags, nothing else
newer=$(git rev-parse develop)
# shellcheck disable=SC2329
hub_tags() { printf '%s\n' "sha-${d1:0:7}" "sha-${newer:0:7}" sha-0000000 buildcache-develop buildcache latest v1.0.0 v1.0.0-rc.1 sha-ABCDEF0; }
# shellcheck disable=SC2329
hub_delete() { echo "del $2"; }
want="del sha-${d1:0:7} del buildcache-develop del buildcache"
check "cleanup (release before newer commits)" "$want" "$(cleanup "$release" false img | grep '^del' | tr '\n' ' ' | sed 's/ $//')"
check "cleanup dry run deletes nothing" "" "$(cleanup "$release" true img | grep '^del')"
check "cleanup dry run lists the cache tags" 2 "$(cleanup "$release" true img | grep -c '^  buildcache')"

exit $fail
