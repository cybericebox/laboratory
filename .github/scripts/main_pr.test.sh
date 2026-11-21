#!/usr/bin/env bash
# Tests of main_pr.sh against a local bare repository as "origin" and a faked gh: .github/scripts/main_pr.test.sh
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
mkdir "$tmp/bin" "$tmp/st"
cat >"$tmp/bin/gh" <<'GH'
#!/usr/bin/env bash
echo "gh $*" >>"$ST/calls"
case "$1 $2" in
  "pr list") [[ -f "$ST/pr" ]] && cat "$ST/pr" ;;
  "pr create") echo 9 >"$ST/pr" ;;
  "pr view") if [[ "$*" == *url* ]]; then echo https://example/pull/9; else echo "${FAKE_MERGEABLE:-MERGEABLE}"; fi ;;
  "pr merge") [[ -z "${FAKE_NO_AUTOMERGE:-}" ]] || exit 1 ;;
esac
exit 0
GH
chmod +x "$tmp/bin/gh"
export PATH=$tmp/bin:$PATH ST=$tmp/st POLL_SLEEP=0 GITHUB_STEP_SUMMARY=$tmp/summary
git init -q --bare "$tmp/origin.git"
git clone -q "$tmp/origin.git" "$tmp/w" 2>/dev/null
cd "$tmp/w" || exit 1
commit() { echo "$2" >>"$1"; git add -A; git commit -qm "$2"; }
git checkout -q -b main && commit a base && git push -q origin main
git checkout -q -b release/1.10 && commit b line1 && git push -q origin release/1.10
sha=$(git rev-parse HEAD)
run() { VERSION=v1.10.0 SHA=$sha "$here/main_pr.sh" 2>&1; }
calls() { grep -c "$1" "$ST/calls" 2>/dev/null || true; }
reset() { : >"$ST/calls"; : >"$GITHUB_STEP_SUMMARY"; rm -f "$ST/pr"; }

# 1. first run: branch at SHA, one pull request into main, auto-merge with a merge commit, no push to main
reset
out=$(run); rc=$?
check "first run: ok" 0 "$rc"
check "first run: branch at the promoted commit" "$sha" "$(git ls-remote origin refs/heads/promote/v1.10.0 | cut -f1)"
check "first run: main untouched" "$(git rev-parse origin/main)" "$(git ls-remote origin refs/heads/main | cut -f1)"
check "first run: pull request into main" 1 "$(calls 'pr create --base main --head promote/v1.10.0')"
check "first run: auto-merge with a merge commit" 1 "$(calls 'pr merge 9 --auto --merge')"
check "first run: the summary names the pull request" 1 "$(grep -c 'pull/9' "$GITHUB_STEP_SUMMARY")"
check "first run: the summary says how to start the checks" 1 "$(grep -c 'Approve workflows to run' "$GITHUB_STEP_SUMMARY")"
: "$out"

# 2. a re-run finds the open pull request and the branch: nothing is created again
reset; echo 9 >"$ST/pr"
run >/dev/null; rc=$?
check "re-run: ok" 0 "$rc"
check "re-run: no second pull request" 0 "$(calls 'pr create')"
check "re-run: auto-merge is asked again (idempotent)" 1 "$(calls 'pr merge 9')"

# 3. a branch that carries a hand-made conflict resolution on top of SHA is kept
git checkout -q -b fixup origin/promote/v1.10.0 && commit r resolved && git push -q origin fixup:promote/v1.10.0
reset; echo 9 >"$ST/pr"
before=$(git ls-remote origin refs/heads/promote/v1.10.0 | cut -f1)
run >/dev/null; rc=$?
check "resolved branch: ok" 0 "$rc"
check "resolved branch: left as it is" "$before" "$(git ls-remote origin refs/heads/promote/v1.10.0 | cut -f1)"

# 4. a branch that does not contain SHA is refused
git push -q origin "$(git rev-parse origin/main)":refs/heads/promote/v1.10.1 2>/dev/null
reset
rc=0; VERSION=v1.10.1 SHA=$sha "$here/main_pr.sh" >/dev/null 2>&1 || rc=$?
check "foreign branch: refused" 1 "$rc"
check "foreign branch: no pull request" 0 "$(calls 'pr create')"

# 5. a conflict: the job fails and the summary says what to do
git push -q origin --delete promote/v1.10.1 2>/dev/null
reset
out=$(FAKE_MERGEABLE=CONFLICTING VERSION=v1.10.1 SHA=$sha "$here/main_pr.sh" 2>&1); rc=$?
check "conflict: the job fails" 1 "$rc"
check "conflict: the error names the cause" 1 "$(grep -c 'changed by hand' <<<"$out")"
check "conflict: the summary has the commands" 1 "$(grep -c 'git merge origin/main' "$GITHUB_STEP_SUMMARY")"

# 6. auto-merge unavailable: a warning, not a failure
reset
git push -q origin --delete promote/v1.10.1 2>/dev/null
out=$(FAKE_NO_AUTOMERGE=1 VERSION=v1.10.1 SHA=$sha "$here/main_pr.sh" 2>&1); rc=$?
check "no auto-merge: still ok" 0 "$rc"
check "no auto-merge: a warning" 1 "$(grep -c '::warning::' <<<"$out")"

# 7. main already has the commit: nothing at all
git checkout -q main && git merge -q --no-ff -m "Release" "$sha" && git push -q origin main
reset
out=$(run); rc=$?
check "already in main: ok" 0 "$rc"
check "already in main: no gh call" 0 "$(calls 'gh')"
exit $fail
