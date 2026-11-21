#!/usr/bin/env bash
# Tests of backport.sh against a local bare repository as "origin" and a faked gh: .github/scripts/backport.test.sh
set -uo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
fail=0
check() { # name expected actual
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: want '$2', got '$3'"; fail=1; fi
}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
mkdir "$tmp/bin" "$tmp/state"
cat >"$tmp/bin/gh" <<'GH'
#!/usr/bin/env bash
echo "gh $*" >>"$STATE/calls"
case "$1 $2" in
  "pr list") echo 7 ;;
  "issue list") [[ -f "$STATE/issue" ]] && echo 3 ;;
  "issue create") touch "$STATE/issue" ;;
esac
exit 0
GH
chmod +x "$tmp/bin/gh"
export PATH=$tmp/bin:$PATH STATE=$tmp/state GITHUB_REPOSITORY=o/r
git init -q --bare "$tmp/origin.git"
git clone -q "$tmp/origin.git" "$tmp/w" 2>/dev/null
cd "$tmp/w" || exit 1
mkdir -p .github/scripts
cp "$here/backport.sh" "$here/release_version.py" .github/scripts/
commit() { echo "$2" >>"$1"; git add -A; git commit -qm "$2"; }
git checkout -q -b develop
commit a.txt base
git push -q origin develop
git checkout -q -b release/1.10
git push -q origin release/1.10
run() { LINE_BRANCH=release/1.10 BEFORE=$1 AFTER=$2 .github/scripts/backport.sh 2>&1; }
calls() { grep -c "$1" "$STATE/calls" 2>/dev/null || true; }
reset() { : >"$STATE/calls"; }
head_of() { git rev-parse "$1"; }
remote_branch() { git ls-remote origin "refs/heads/$1" | cut -f1; }

# 1. a new line (all-zero BEFORE): nothing
reset
out=$(run 0000000000000000000000000000000000000000 "$(head_of HEAD)")
check "new line: nothing opened" "0" "$(calls 'pr create')"

# 2. a pull request merged with a merge commit: one backport branch holding one cherry-pick of the whole change
b0=$(head_of release/1.10)
git checkout -q -b fix1 release/1.10
commit f1.txt one
commit f1.txt two
git checkout -q release/1.10
git merge -q --no-ff -m "Merge pull request #5 fix1" fix1
m1=$(head_of HEAD)
git push -q origin release/1.10
reset
run "$b0" "$m1" >/dev/null
short=${m1:0:7}
check "merge commit: backport branch exists" "1" "$(remote_branch "backport/1.10/$short" | grep -c .)"
check "merge commit: pull request into develop created" "1" "$(calls "pr create --base develop --head backport/1.10/$short")"
check "merge commit: auto-merge requested" "1" "$(calls 'pr merge 7 --auto --merge')"
git fetch -q origin "backport/1.10/$short"
check "merge commit: one commit on top of develop" "1" "$(git rev-list --count origin/develop..FETCH_HEAD)"
check "merge commit: the whole change is there" "one two" "$(git show FETCH_HEAD:f1.txt | tr '\n' ' ' | sed 's/ $//')"

# 3. the same run again: nothing is opened twice
reset
run "$b0" "$m1" >/dev/null
check "re-run: no second pull request" "0" "$(calls 'pr create')"

# 4. a squashed or rebased change: each commit of the push is picked
git checkout -q release/1.10
b1=$(head_of HEAD)
commit g.txt g1
c1=$(head_of HEAD)
commit g.txt g2
c2=$(head_of HEAD)
git push -q origin release/1.10
reset
run "$b1" "$c2" >/dev/null
check "rebase: one pull request for the push" "1" "$(calls 'pr create')"
check "rebase: branch named by the last commit" "1" "$(remote_branch "backport/1.10/${c2:0:7}" | grep -c .)"
git fetch -q origin "backport/1.10/${c2:0:7}"
check "rebase: both commits are picked in order" "2" "$(git rev-list --count origin/develop..FETCH_HEAD)"
check "rebase: the stacked change is there" "g1 g2" "$(git show FETCH_HEAD:g.txt | tr '\n' ' ' | sed 's/ $//')"
: "$c1"

# 5. a commit that is already in develop: nothing
git checkout -q develop
git merge -q --ff-only origin/release/1.10 2>/dev/null || git merge -q -m sync release/1.10
git push -q origin develop
git checkout -q release/1.10
reset
run "$b1" "$c2" >/dev/null
check "already in develop: nothing" "0" "$(calls 'pr create')"

# 6. a change that does not apply: an issue, no branch
git checkout -q develop
echo "develop wrote this" >h.txt && git add -A && git commit -qm "dev h" && git push -q origin develop
git checkout -q release/1.10
b2=$(head_of HEAD)
echo "line wrote this" >h.txt && git add -A && git commit -qm "line h"
conflict=$(head_of HEAD)
git push -q origin release/1.10
rm -f "$STATE/issue"
reset
out=$(run "$b2" "$conflict")
check "conflict: no branch" "0" "$(remote_branch "backport/1.10/${conflict:0:7}" | grep -c .)"
check "conflict: no pull request" "0" "$(calls 'pr create')"
check "conflict: an issue is opened" "1" "$(calls 'issue create')"
check "conflict: a warning is printed" "1" "$(grep -c '::warning::' <<<"$out")"
reset
run "$b2" "$conflict" >/dev/null
check "conflict re-run: the issue is not opened twice" "0" "$(calls 'issue create')"

# 7. the change is already in develop in another form (an empty pick)
git checkout -q develop
echo "same" >k.txt && git add -A && git commit -qm "dev k" && git push -q origin develop
git checkout -q release/1.10
b3=$(head_of HEAD)
echo "same" >k.txt && git add -A && git commit -qm "line k"
k=$(head_of HEAD)
git push -q origin release/1.10
reset
out=$(run "$b3" "$k")
check "empty pick: nothing opened" "0" "$(( $(calls 'pr create') + $(calls 'issue create') ))"
check "empty pick: says so" "1" "$(grep -c 'already in develop' <<<"$out")"

exit $fail
