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
printf '#!/bin/sh\necho "${FAKE_COUNT:-0}"\n' >bin/gh
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
check "minor label" "skip=false version=v1.1.0 tag=v1.1.0-rc.1 " "$(FAKE_COUNT=1 run)"
check "major" "skip=false version=v2.0.0 tag=v2.0.0-rc.1 " "$(BUMP=major run)"
g tag v1.0.1-rc.1
check "same tree as the last rc" "skip=true " "$(run)"
commit c
check "next rc of the same version" "skip=false version=v1.0.1 tag=v1.0.1-rc.2 " "$(run)"
g tag v1.0.1
g tag v1.1.0
commit d
check "vX.Y.Z after vX.Y" "skip=false version=v1.1.1 tag=v1.1.1-rc.1 " "$(run)"

exit $fail
