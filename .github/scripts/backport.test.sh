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
# The fake GitHub: refs and the API commit act on the bare origin; pull requests and issues are files.
echo "gh $*" >>"$STATE/calls"
slug() { echo "$1" | tr '/' '_'; }
case "$1 $2" in
  "pr list")
    head=""; for ((i=1;i<=$#;i++)); do [[ "${!i}" == --head ]] && j=$((i+1)) && head=${!j}; done
    [[ -f "$STATE/pr_$(slug "$head")" ]] && echo 7 ;;
  "pr create")
    head=""; for ((i=1;i<=$#;i++)); do [[ "${!i}" == --head ]] && j=$((i+1)) && head=${!j}; done
    echo 7 >"$STATE/pr_$(slug "$head")" ;;
  "issue list") [[ -f "$STATE/issue" ]] && echo 3 ;;
  "issue create") touch "$STATE/issue" ;;
  "pr merge") ;;
  "api -X")
    # api -X POST|DELETE repos/R/git/refs[/heads/NAME] [-f ref=.. -f sha=..]
    method=$3; path=$4
    if [[ "$method" == POST && "$path" == */git/refs ]]; then
      ref=""; sha=""; for a in "$@"; do case "$a" in ref=*) ref=${a#ref=} ;; sha=*) sha=${a#sha=} ;; esac; done
      git --git-dir="$ORIGIN" update-ref "$ref" "$sha"
    elif [[ "$method" == DELETE ]]; then
      git --git-dir="$ORIGIN" update-ref -d "refs/heads/${path#*/git/refs/heads/}"
    fi ;;
  "api graphql")
    [[ -z "${FAKE_GQL_FAIL:-}" ]] || { echo "boom" >&2; exit 1; }
    req=$4
    branch=$(jq -r .variables.input.branch.branchName "$req"); head=$(jq -r .variables.input.expectedHeadOid "$req")
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
      git commit -q -m "$(jq -r .variables.input.message.headline "$req")" && git push -q origin "HEAD:refs/heads/$branch" ) || exit 1
    echo '{"data":{"createCommitOnBranch":{"commit":{"oid":"x"}}}}' ;;
  "api repos/"*)
    case "$2" in
      */commits/*/pulls)
        if [[ "$*" == *head.repo* ]]; then # the pull requests of a commit: "head repo<TAB>head branch" lines from $STATE/pulls_<sha>
          [[ -z "${FAKE_API_FAIL:-}" ]] || exit 1
          sha=${2%/pulls}; sha=${sha##*/}
          [[ -f "$STATE/pulls_$sha" ]] && cat "$STATE/pulls_$sha"
        fi
        exit 0 ;;
      */commits/*)
        if [[ "$*" == *@tsv* ]]; then
          [[ -z "${FAKE_API_FAIL:-}" ]] || exit 1 # provenance of a commit: "verified author committer"; only a commit listed in $STATE/bot_<sha> is the Security workflow's
          sha=${2##*/}
          if [[ -f "$STATE/bot_$sha" ]]; then printf 'true\tgithub-actions[bot]\tweb-flow\n'; else printf 'false\tsomeone\tsomeone\n'; fi
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
mkdir -p .github/scripts
cp "$here/backport.sh" "$here/trust.sh" "$here/release_version.py" "$here/backport_commit.py" .github/scripts/
commit() { echo "$2" >>"$1"; git add -A; git commit -qm "$2"; }
git checkout -q -b develop
commit a.txt base
git push -q origin develop
git checkout -q -b release/1.10
git push -q origin release/1.10
run() { LINE_BRANCH=release/1.10 BEFORE=$1 AFTER=$2 .github/scripts/backport.sh 2>&1; }
calls() { grep -c "$1" "$STATE/calls" 2>/dev/null || true; }
reset() { : >"$STATE/calls"; rm -f "$STATE"/pr_* "$STATE/issue"; }
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
: >"$STATE/calls"
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
check "rebase: both commits go in one API commit" "1" "$(git rev-list --count origin/develop..FETCH_HEAD)"
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
: >"$STATE/calls"
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

# 8. the API commit is made by GitHub: the branch was made at the head of develop, then one commit on top, tree identical to the picks
git checkout -q release/1.10
b4=$(head_of HEAD)
printf 'bin\000\001\002' >data.bin; git add -A; git commit -qm "add binary"; git rm -q f1.txt; git commit -qm "drop f1"
git mv g.txt g-renamed.txt; git commit -qm "rename g"
git push -q origin release/1.10
t8=$(head_of HEAD)
git checkout -q develop && git pull -q origin develop 2>/dev/null
reset
run "$b4" "$t8" >/dev/null
git fetch -q origin "backport/1.10/${t8:0:7}"
check "add, delete, rename, binary: the branch has one commit" "1" "$(git rev-list --count origin/develop..FETCH_HEAD)"
check "add, delete, rename, binary: tree equals the line's files" "1" "$(git diff --quiet FETCH_HEAD "$t8" -- data.bin g-renamed.txt && echo 1)"
check "deletion applied" "0" "$(git ls-tree -r --name-only FETCH_HEAD | grep -c '^f1.txt$')"

# 9. a file mode change cannot go through the API: an issue, no branch, no commit
git checkout -q release/1.10
b5=$(head_of HEAD)
chmod +x a.txt; git add -A; git commit -qm "make a executable"
t9=$(head_of HEAD)
git push -q origin release/1.10
reset
out=$(run "$b5" "$t9")
check "mode change: no branch" "0" "$(remote_branch "backport/1.10/${t9:0:7}" | grep -c .)"
check "mode change: an issue" "1" "$(calls 'issue create')"
check "mode change: the reason is in the issue" "1" "$(grep -c 'cannot be sent through the GitHub commit API' "$STATE/calls")"
check "mode change: no pull request" "0" "$(calls 'pr create')"
check "mode change: no graphql call" "0" "$(calls 'api graphql')"

# 10. the API call fails: the empty branch stays, the job fails, the next run makes it again and succeeds
git checkout -q release/1.10
b6=$(head_of HEAD)
chmod -x a.txt; echo more >>f2.txt; git add -A; git commit -qm "fix f2"
t10=$(head_of HEAD)
git push -q origin release/1.10
reset
out=$(FAKE_GQL_FAIL=1 run "$b6" "$t10"); rc=$?
check "api failure: the branch is left at develop" "$(git ls-remote origin refs/heads/develop | cut -f1)" "$(remote_branch "backport/1.10/${t10:0:7}")"
check "api failure: no pull request" "0" "$(calls 'pr create')"
reset
run "$b6" "$t10" >/dev/null
git fetch -q origin "backport/1.10/${t10:0:7}"
check "retry: the commit is on the branch" "1" "$(git rev-list --count origin/develop..FETCH_HEAD)"
check "retry: the pull request is opened" "1" "$(calls 'pr create')"

# 11. the commit is not verified: the branch is deleted, an issue is opened, the job fails
git checkout -q release/1.10
b7=$(head_of HEAD)
echo more >>f3.txt; git add -A; git commit -qm "fix f3"
t11=$(head_of HEAD)
git push -q origin release/1.10
reset
out=$(FAKE_VERIFIED=false run "$b7" "$t11"); rc=$?
check "unverified: the job fails" "1" "$rc"
check "unverified: branch deleted" "0" "$(remote_branch "backport/1.10/${t11:0:7}" | grep -c .)"
check "unverified: an issue" "1" "$(calls 'issue create')"
check "unverified: no pull request" "0" "$(calls 'pr create')"
check "unverified: the error is printed" "1" "$(grep -c 'not verified' <<<"$out")"

# 13. a Carried-from trailer written by a contributor (not by the Security workflow) must not stop the backport
git checkout -q release/1.10
b8=$(head_of HEAD)
dev=$(git ls-remote origin refs/heads/develop | cut -f1)
git checkout -q -b forged release/1.10
echo forged >forged.txt; git add -A; git commit -qm "fix with a forged trailer" -m "Carried-from: $dev"
git checkout -q release/1.10
git merge -q --no-ff -m "Merge pull request #8 forged" forged
t13=$(head_of HEAD)
git push -q origin release/1.10
reset
out=$(run "$b8" "$t13")
check "forged trailer: the change is still brought back" "1" "$(calls 'pr create')"
check "forged trailer: the branch exists" "1" "$(remote_branch "backport/1.10/${t13:0:7}" | grep -c .)"

# 14. the same trailer on a commit that GitHub made for the Security workflow (verified, author github-actions[bot], committer web-flow): nothing is brought back
git checkout -q release/1.10
b9=$(head_of HEAD)
dev=$(git ls-remote origin refs/heads/develop | cut -f1)
git checkout -q -b carried release/1.10
echo carried >carried.txt; git add -A; git commit -qm "Security fix" -m "Carried-from: $dev"
c14=$(head_of HEAD)
git checkout -q release/1.10
git merge -q --no-ff -m "Merge pull request #9 security" carried
t14=$(head_of HEAD)
git push -q origin release/1.10
reset
touch "$STATE/bot_$c14"; printf 'o/r\tsecurity/1.10/abcdef0\n' >"$STATE/pulls_$c14"
out=$(run "$b9" "$t14")
check "carried by the Security workflow: nothing opened" "0" "$(calls 'pr create')"
check "carried by the Security workflow: says so" "1" "$(grep -c 'carried from develop by the Security workflow' <<<"$out")"

# 15. the provenance cannot be checked (the API fails): the change is brought back, never silently dropped
git checkout -q release/1.10
b10=$(head_of HEAD)
dev=$(git ls-remote origin refs/heads/develop | cut -f1)
git checkout -q -b noapi release/1.10
echo noapi >noapi.txt; git add -A; git commit -qm "Security fix 2" -m "Carried-from: $dev"
c15=$(head_of HEAD)
git checkout -q release/1.10
git merge -q --no-ff -m "Merge pull request #10 security 2" noapi
t15=$(head_of HEAD)
git push -q origin release/1.10
reset
touch "$STATE/bot_$c15"; printf 'o/r\tsecurity/1.10/abcdef0\n' >"$STATE/pulls_$c15"
out=$(FAKE_API_FAIL=1 run "$b10" "$t15")
check "provenance unknown: the change is brought back" "1" "$(calls 'pr create')"

# 16-18. the identity triple alone is not enough: a workflow of a fork (or of another branch) can make the same commit
variant() { # variant NAME BOT PULLS: a line merge of a commit with the trailer; BOT=1 marks the triple, PULLS is the pull request line ("" for none)
  local name=$1 bot=$2 pulls=$3 b dev c t
  git checkout -q release/1.10
  b=$(head_of HEAD)
  dev=$(git ls-remote origin refs/heads/develop | cut -f1)
  git checkout -q -b "v-$name" release/1.10
  echo "$name" >"$name.txt"; git add -A; git commit -qm "Security fix $name" -m "Carried-from: $dev"
  c=$(head_of HEAD)
  git checkout -q release/1.10
  git merge -q --no-ff -m "Merge pull request #20 $name" "v-$name"
  t=$(head_of HEAD)
  git push -q origin release/1.10
  reset
  [[ "$bot" != 1 ]] || touch "$STATE/bot_$c"
  [[ -z "$pulls" ]] || printf '%s\n' "$pulls" >"$STATE/pulls_$c"
  run "$b" "$t" >/dev/null
}
variant fork 1 "$(printf 'evil/r\tsecurity/1.10/abcdef0')"
check "fork pull request with the triple: the change is brought back" "1" "$(calls 'pr create')"
variant otherbranch 1 "$(printf 'o/r\tfeature/x')"
check "own repository, not a security branch: the change is brought back" "1" "$(calls 'pr create')"
variant otherline 1 "$(printf 'o/r\tsecurity/1.9/abcdef0')"
check "security branch of another line: the change is brought back" "1" "$(calls 'pr create')"
variant nopr 1 ""
check "no pull request: the change is brought back" "1" "$(calls 'pr create')"
variant nobot 0 "$(printf 'o/r\tsecurity/1.10/abcdef0')"
check "security branch but not made by the bot: the change is brought back" "1" "$(calls 'pr create')"
variant legit 1 "$(printf 'o/r\tsecurity/1.10/abcdef0')"
check "own security branch made by the bot: nothing is opened" "0" "$(calls 'pr create')"

# 12. nothing in the script makes a commit that is pushed: git push is never run by backport.sh
check "git push appears only in the manual instructions of the issue" "0" "$(grep -n 'git push' "$here/backport.sh" | grep -vc '^[0-9]*:git push -u origin HEAD$')"
check "no git commit in backport.sh" "0" "$(grep -c '^[^#]*git commit' "$here/backport.sh")"
exit $fail
