#!/usr/bin/env bash
# Tests of security.sh against a local bare repository as "origin" and a faked gh: .github/scripts/security.test.sh
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
mkdir "$tmp/bin" "$tmp/state" "$tmp/scripts"
# The scripts run from outside the clone: the clone's checkout moves while they run.
cp "$here/security.sh" "$here/backport.sh" "$here/trust.sh" "$here/release_version.py" "$here/backport_commit.py" "$tmp/scripts/"
cat >"$tmp/bin/gh" <<'GH'
#!/usr/bin/env bash
# The fake GitHub: refs and the API commit act on the bare origin; pull requests and issues are files.
echo "gh $*" >>"$STATE/calls"
slug() { echo "$1" | tr '/' '_'; }
arg() { local want=$1 i j; shift; for ((i=1;i<=$#;i++)); do [[ "${!i}" == "$want" ]] && j=$((i+1)) && echo "${!j}"; done; }
case "$1 $2" in
  "pr list") [[ -f "$STATE/pr_$(slug "$(arg --head "$@")")" ]] && echo 7 ;;
  "pr create") echo "$*" >"$STATE/pr_$(slug "$(arg --head "$@")")" ;;
  "issue list")
    s=$(arg --search "$@"); s=${s#in:title \"}; s=${s%\"}
    [[ -f "$STATE/issues" ]] && grep -F -- "$s" "$STATE/issues" ;;
  "issue create") arg --title "$@" >>"$STATE/issues"; arg --label "$@" >>"$STATE/issue_labels"; echo "$*" >>"$STATE/issue_bodies" ;;
  "pr merge") ;;
  "api -X")
    method=$3; path=$4
    if [[ "$method" == POST && "$path" == */git/refs ]]; then
      ref=""; sha=""; for a in "$@"; do case "$a" in ref=*) ref=${a#ref=} ;; sha=*) sha=${a#sha=} ;; esac; done
      git --git-dir="$ORIGIN" update-ref "$ref" "$sha"
    elif [[ "$method" == DELETE ]]; then
      git --git-dir="$ORIGIN" update-ref -d "refs/heads/${path#*/git/refs/heads/}"
    fi ;;
  "api graphql")
    req=$4
    branch=$(jq -r .variables.input.branch.branchName "$req"); head=$(jq -r .variables.input.expectedHeadOid "$req")
    [[ -z "${FAKE_GQL_FAIL:-}" || "$branch" != *"$FAKE_GQL_FAIL"* ]] || { echo "boom" >&2; exit 1; }
    d=$(mktemp -d); git clone -q "$ORIGIN" "$d" 2>/dev/null
    ( cd "$d" || exit 1
      git checkout -q -B work "origin/$branch"
      [[ "$(git rev-parse HEAD)" == "$head" ]] || { echo "head moved" >&2; exit 1; }
      jq -r '.variables.input.fileChanges.deletions[]?.path' "$req" | while IFS= read -r p; do git rm -q -- "$p"; done
      n=$(jq '.variables.input.fileChanges.additions|length' "$req")
      for ((k=0;k<n;k++)); do
        p=$(jq -r ".variables.input.fileChanges.additions[$k].path" "$req")
        mkdir -p "$(dirname "$p")"; jq -r ".variables.input.fileChanges.additions[$k].contents" "$req" | base64 -d >"$p"; git add -- "$p"
      done
      git commit -q -m "$(jq -r .variables.input.message.headline "$req")" -m "$(jq -r .variables.input.message.body "$req")" && git push -q origin "HEAD:refs/heads/$branch" && touch "$STATE/bot_$(git rev-parse HEAD)" && printf 'o/r\t%s\n' "$branch" >"$STATE/pulls_$(git rev-parse HEAD)" ) || exit 1
    rm -rf "$d"
    echo '{"data":{"createCommitOnBranch":{"commit":{"oid":"x"}}}}' ;;
  "api repos/"*)
    case "$2" in
      */commits/*/pulls)
        if [[ "$*" == *head.repo* ]]; then sha=${2%/pulls}; sha=${sha##*/}; [[ -f "$STATE/pulls_$sha" ]] && cat "$STATE/pulls_$sha"; fi
        exit 0 ;;
      */commits/*)
        if [[ "$*" == *@tsv* ]]; then # provenance: only the commits of the fake commit API are the Security workflow's
          if [[ -f "$STATE/bot_${2##*/}" ]]; then printf 'true\tgithub-actions[bot]\tweb-flow\n'; else printf 'false\tsomeone\tsomeone\n'; fi
          exit 0
        fi
        echo "${FAKE_VERIFIED:-true}" ;;
    esac ;;
esac
exit 0
GH
chmod +x "$tmp/bin/gh"
export PATH=$tmp/bin:$PATH STATE=$tmp/state GITHUB_REPOSITORY=o/r ORIGIN=$tmp/origin.git
git init -q --bare "$tmp/origin.git"
git clone -q "$tmp/origin.git" "$tmp/w" 2>/dev/null
cd "$tmp/w" || exit 1
commit() { echo "$2" >>"$1"; git add -A; git commit -qm "$2"; }
calls() { grep -c -- "$1" "$STATE/calls" 2>/dev/null || true; }
reset() { : >"$STATE/calls"; rm -f "$STATE"/pr_* "$STATE/issues" "$STATE/issue_labels" "$STATE/issue_bodies"; }
remote_branch() { git ls-remote origin "refs/heads/$1" | cut -f1; }
on_branch() { git fetch -q origin "$1" && git show "FETCH_HEAD:$2" | tr '\n' ' ' | sed 's/ $//'; }
# merged PR_NUMBER NAME FILE TEXT...: a pull request into develop with one commit per TEXT, merged with a merge commit; prints the merge
merged() {
  local n=$1 name=$2 f=$3 t; shift 3
  git checkout -q -b "$name" develop
  for t in "$@"; do commit "$f" "$t"; done
  git checkout -q develop
  git merge -q --no-ff -m "Merge pull request #$n $name" "$name"
  git push -q origin develop
  git --git-dir="$ORIGIN" update-ref "refs/pull/$n/head" "$(git rev-parse "$name")"
  git rev-parse HEAD
}
run() { # run MERGE PR [LABELS]   (TITLE overrides the title)
  PR=$2 MERGE_SHA=$1 PR_TITLE="${TITLE:-Fix the hole}" PR_LABELS=${3:-security} "$tmp/scripts/security.sh" 2>&1
}

git checkout -q -b develop
commit a.txt base
commit s.txt "shared base"
git push -q origin develop

# 1. no release line yet: nothing
reset
m0=$(merged 10 early e.txt early)
out=$(run "$m0" 10); rc=$?
check "no line: success" "0" "$rc"
check "no line: says so" "1" "$(grep -c 'no release line exists' <<<"$out")"
check "no line: nothing opened" "0" "$(calls 'pr create')"

for v in 1.8 1.9 1.10 1.11; do
  git checkout -q -b "release/$v" develop
  commit "line-$v.txt" "line $v"
  git push -q origin "release/$v"
done
git checkout -q develop

# 2. a pull request merged with a merge commit: the three newest lines (by number) each get a branch and a pull request
reset
m1=$(merged 12 fix1 fix.txt one two)
s1=${m1:0:7}
out=$(run "$m1" 12); rc=$?
check "merge: success" "0" "$rc"
for v in 1.11 1.10 1.9; do
  check "merge: branch security/$v" "1" "$(remote_branch "security/$v/$s1" | grep -c .)"
  check "merge: pull request into release/$v" "1" "$(calls "pr create --base release/$v --head security/$v/$s1")"
  git fetch -q origin "security/$v/$s1"
  check "merge: one commit on top of release/$v" "1" "$(git rev-list --count "origin/release/$v..FETCH_HEAD")"
  check "merge: the whole change on $v" "one two" "$(on_branch "security/$v/$s1" fix.txt)"
  check "merge: the line keeps its own files on $v" "line $v" "$(on_branch "security/$v/$s1" "line-$v.txt")"
  check "merge: the trailer on $v" "Carried-from: $m1" "$(git log -1 --format=%B FETCH_HEAD | grep '^Carried-from:')"
done
check "merge: the oldest line is not supported" "0" "$(remote_branch "security/1.8/$s1" | grep -c .)"
check "merge: three pull requests" "3" "$(calls 'pr create')"
check "merge: each has the label security" "3" "$(grep -c -- '--label security' "$STATE/calls")"
check "merge: no large-fix without it on the source" "0" "$(calls '--label large-fix')"
check "merge: no auto-merge into a line" "0" "$(calls 'pr merge')"

# 3. the same run again: nothing is opened twice
: >"$STATE/calls"
run "$m1" 12 >/dev/null
check "re-run: no second pull request" "0" "$(calls 'pr create')"
check "re-run: no second API commit" "0" "$(calls 'api graphql')"

# 4. the security pull request is merged into a line: Backport does not bring it back into develop, and a new run sees it there
git fetch -q origin "security/1.11/$s1" release/1.11
git checkout -q -B release/1.11 origin/release/1.11
before=$(git rev-parse HEAD)
git merge -q --no-ff -m "Merge pull request #13 security/1.11/$s1" "origin/security/1.11/$s1"
after=$(git rev-parse HEAD)
git push -q origin release/1.11
git checkout -q develop
: >"$STATE/calls"
out=$(LINE_BRANCH=release/1.11 BEFORE=$before AFTER=$after "$tmp/scripts/backport.sh" 2>&1); rc=$?
check "backport of the merged security pull request: success" "0" "$rc"
check "backport: no pull request into develop" "0" "$(calls 'pr create')"
check "backport: no issue" "0" "$(calls 'issue create')"
check "backport: says it was carried" "1" "$(grep -c 'carried from develop by the Security workflow' <<<"$out")"
git push -q origin --delete "security/1.11/$s1"
rm -f "$STATE"/pr_*
: >"$STATE/calls"
out=$(run "$m1" 12)
check "after the merge into 1.11: it is seen there" "1" "$(grep -c 'release/1.11 already has .* (carried before)' <<<"$out")"
check "after the merge into 1.11: no new branch" "0" "$(remote_branch "security/1.11/$s1" | grep -c .)"

# 5. a fix plus a change of the line merged together: Backport still brings back what is not from develop
git checkout -q -B sec2 "origin/security/1.10/$s1"
commit extra.txt "line-only follow-up"
git checkout -q -B release/1.10 origin/release/1.10
before=$(git rev-parse HEAD)
git merge -q --no-ff -m "Merge pull request #14" sec2
after=$(git rev-parse HEAD)
git push -q origin release/1.10
git checkout -q develop
reset
LINE_BRANCH=release/1.10 BEFORE=$before AFTER=$after "$tmp/scripts/backport.sh" >/dev/null 2>&1
check "mixed merge: Backport opens its pull request" "1" "$(calls 'pr create --base develop')"
git fetch -q origin "backport/1.10/${after:0:7}" develop
check "mixed merge: only the line-only change goes back" "extra.txt" "$(git diff --name-only origin/develop FETCH_HEAD)"

# 6. a line cut after the merge already has the change; large-fix of the source is carried along
git branch -q release/1.12 develop
git push -q origin release/1.12
reset
m2=$(merged 15 fix2 big.txt big)
s2=${m2:0:7}
git checkout -q release/1.12 2>/dev/null || git checkout -q -b release/1.12 origin/release/1.12
git merge -q --ff-only develop && git push -q origin release/1.12
git checkout -q develop
out=$(run "$m2" 15 "security,large-fix")
check "cut after: the newest line already has it" "1" "$(grep -c "release/1.12 already has $s2" <<<"$out")"
check "cut after: supported are 1.12 1.11 1.10" "1" "$(grep -c 'supported lines: release/1.12 release/1.11 release/1.10' <<<"$out")"
check "cut after: two pull requests" "2" "$(calls 'pr create')"
check "cut after: 1.9 is no longer supported" "0" "$(remote_branch "security/1.9/$s2" | grep -c .)"
check "large-fix carried" "2" "$(grep -c -- '--label security --label large-fix' "$STATE/calls")"

# 7. a rebase merge: the newest commits of develop, as many as the pull request has of its own (a merge of develop into it does not
#    count), each proven by its patch-id, are picked in order
reset
git checkout -q -b fixr develop
commit r.txt r1
git checkout -q develop
commit t.txt "develop moved on"
git checkout -q fixr
git merge -q --no-ff -m "Merge develop into fixr" develop
commit r.txt r2
git push -q origin fixr:refs/pull/20/head
git checkout -q develop
for c in $(git rev-list --reverse --no-merges fixr --not develop); do git cherry-pick "$c" >/dev/null; done
git push -q origin develop
m3=$(git rev-parse HEAD)
s3=${m3:0:7}
out=$(run "$m3" 20); rc=$?
check "rebase: success" "0" "$rc"
check "rebase: both commits on 1.11" "r1 r2" "$(on_branch "security/1.11/$s3" r.txt)"
git fetch -q origin "security/1.11/$s3"
check "rebase: one API commit" "1" "$(git rev-list --count origin/release/1.11..FETCH_HEAD)"
check "rebase: nothing else of develop came along" "0" "$(git ls-tree --name-only FETCH_HEAD | grep -c '^big.txt$')"

# 7b. a rebase merge whose commits in develop do not match the pull request: nothing is carried, one issue says so
reset
git checkout -q -b fixq develop
commit q.txt q1
commit q.txt q2
git push -q origin fixq:refs/pull/21/head
git checkout -q develop
commit q.txt "something else"
commit q.txt q2
git push -q origin develop
mq=$(git rev-parse HEAD)
out=$(run "$mq" 21); rc=$?
check "rebase mismatch: the job fails (nothing was carried)" "1" "$rc"
check "rebase mismatch: no branch" "0" "$(git ls-remote origin "refs/heads/security/*/${mq:0:7}" | grep -c .)"
check "rebase mismatch: one issue for the lines" "1" "$(grep -c "Security fix ${mq:0:7} into the release lines does not apply" "$STATE/issues")"
check "rebase mismatch: no pull request" "0" "$(calls 'pr create')"

# 8. a conflict on one line: an issue for that line only, the other lines get their pull requests
git checkout -q release/1.11 2>/dev/null || git checkout -q -b release/1.11 origin/release/1.11
git reset -q --hard origin/release/1.11
echo "the line wrote this" >c.txt && git add -A && git commit -qm "line c" && git push -q origin release/1.11
git checkout -q develop
reset
m4=$(merged 16 fix4 c.txt "develop wrote this")
s4=${m4:0:7}
out=$(run "$m4" 16); rc=$?
check "conflict: the job does not fail" "0" "$rc"
check "conflict: no branch for 1.11" "0" "$(remote_branch "security/1.11/$s4" | grep -c .)"
check "conflict: one issue" "1" "$(calls 'issue create')"
check "conflict: the issue names the line" "1" "$(grep -c "Security fix $s4 into release/1.11 does not apply" "$STATE/issues")"
check "conflict: the issue has the label" "security" "$(cat "$STATE/issue_labels")"
check "conflict: the issue asks for no trailer" "0" "$(grep -c -- '--trailer\|Carried-from' "$STATE/issue_bodies")"
check "conflict: the issue says a manual carry needs none" "1" "$(grep -c 'No trailer is needed' "$STATE/issue_bodies")"
check "conflict: the other lines get pull requests" "2" "$(calls 'pr create')"
: >"$STATE/calls"
run "$m4" 16 >/dev/null
check "conflict re-run: the issue is not opened twice" "0" "$(calls 'issue create')"

# 9. the API call fails on one line: the others go on, the job fails, the next run makes it again
reset
m5=$(merged 17 fix5 f5.txt five)
s5=${m5:0:7}
out=$(TITLE=--help FAKE_GQL_FAIL=security/1.10/ run "$m5" 17); rc=$?
check "api failure: the job fails" "1" "$rc"
check "api failure: the line is named" "1" "$(grep -c 'failed for: release/1.10' <<<"$out")"
check "api failure: the others got pull requests" "2" "$(calls 'pr create')"
check "api failure: the empty branch stays at the line" "$(remote_branch release/1.10)" "$(remote_branch "security/1.10/$s5")"
: >"$STATE/calls"
out=$(TITLE=--help run "$m5" 17); rc=$?
check "retry: success (a title that looks like an option)" "0" "$rc"
check "retry: only the failed line is made" "1" "$(calls 'pr create')"
check "retry: the commit is on the branch" "five" "$(on_branch "security/1.10/$s5" f5.txt)"

# 10. the commit is not verified: the branch is deleted, an issue per line, the job fails
reset
m6=$(merged 18 fix6 f6.txt six)
s6=${m6:0:7}
out=$(FAKE_VERIFIED=false run "$m6" 18); rc=$?
check "unverified: the job fails" "1" "$rc"
check "unverified: branches deleted" "0" "$(git ls-remote origin "refs/heads/security/*/$s6" | grep -c .)"
check "unverified: an issue per line" "3" "$(calls 'issue create')"
check "unverified: no pull request" "0" "$(calls 'pr create')"

# 11. a file mode change cannot go through the API: an issue, no branch
reset
git checkout -q -b fix7 develop
chmod +x a.txt; git add -A; git commit -qm "make a executable"
git checkout -q develop
git merge -q --no-ff -m "Merge pull request #19 fix7" fix7 && git push -q origin develop
git --git-dir="$ORIGIN" update-ref refs/pull/19/head "$(git rev-parse fix7)"
m7=$(git rev-parse HEAD)
out=$(run "$m7" 19); rc=$?
check "mode change: success" "0" "$rc"
check "mode change: no branch" "0" "$(git ls-remote origin "refs/heads/security/*/${m7:0:7}" | grep -c .)"
check "mode change: the reason is in the issue" "3" "$(grep -c 'cannot be sent through the GitHub commit API' "$STATE/issue_bodies")"
check "mode change: no graphql call" "0" "$(calls 'api graphql')"

# 11b. a trailer written by a contributor in a commit of the line does not stop the carry (anyone can write the text)
reset
m8=$(merged 21 fix8 f8.txt eight)
s8=${m8:0:7}
git checkout -q -B release/1.11 origin/release/1.11
echo forged >forged.txt; git add -A; git commit -qm "Fix of the line" -m "Carried-from: $m8"
git push -q origin release/1.11
git checkout -q develop
out=$(run "$m8" 21); rc=$?
check "forged trailer on the line: success" "0" "$rc"
check "forged trailer on the line: the line still gets its branch" "1" "$(remote_branch "security/1.11/$s8" | grep -c .)"
check "forged trailer on the line: the other lines too" "1" "$(remote_branch "security/1.10/$s8" | grep -c .)"
check "forged trailer on the line: not called carried before" "0" "$(grep -c 'carried before' <<<"$out")"
# the same run again: the security branches exist, so nothing is opened twice
: >"$STATE/calls"
out=$(run "$m8" 21); rc=$?
check "forged trailer on the line: a re-run opens nothing twice" "0" "$(calls 'pr create')"

# 12. bad input
out=$(PR=1 MERGE_SHA=abc PR_TITLE=t "$tmp/scripts/security.sh" 2>&1); rc=$?
check "a bad MERGE_SHA is refused" "1" "$rc"
out=$(PR=1 MERGE_SHA=$(printf '1%.0s' {1..40}) PR_TITLE=t "$tmp/scripts/security.sh" 2>&1); rc=$?
check "a commit that is not in develop is refused" "1" "$([[ $rc -ne 0 ]] && echo 1)"
out=$(PR=12 MERGE_SHA=$m1 PR_TITLE=t REMOTE=nowhere "$tmp/scripts/security.sh" 2>&1); rc=$?
check "an unreachable remote fails the job" "1" "$([[ $rc -ne 0 ]] && echo 1)"
reset
m8=$(merged 22 fix8 f8.txt eight)
# shellcheck disable=SC2016 # the title is meant literally
out=$(TITLE='$(touch pwned) `touch pwned2`' run "$m8" 22)
check "a title is never run" "0" "$(find . "$tmp" -name 'pwned*' | grep -c .)"
check "a title is passed as it is" "3" "$(grep -cF 'Security fix to release/1.' "$STATE/calls")"

# 13. nothing in the script makes a commit that is pushed
check "git push appears only in the manual instructions of the issue" "0" "$(grep -n 'git push' "$here/security.sh" | grep -vc '^[0-9]*:git push -u origin HEAD$')"
check "no git commit in security.sh but the manual one of the issue" "0" "$(grep '^[^#]*git commit' "$here/security.sh" | grep -vc "^git commit -m 'Security fix")"
exit $fail
