#!/usr/bin/env bash
# Tests of dev.sh (start, push, done) against a local bare repository as "origin" and a faked gh: scripts/dev.test.sh
set -uo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/.." && pwd)
fail=0
check() { # name expected actual
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: want '$2', got '$3'"; fail=1; fi
}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
mkdir "$tmp/bin"
cat >"$tmp/bin/gh" <<'GH'
#!/usr/bin/env bash
echo "gh $*" >>"$CALLS"
case "$1 $2" in
  "pr list") [[ -f "$CALLS.pr" ]] && cat "$CALLS.pr" ;;
  "pr create") echo 5 >"$CALLS.pr" ;;
esac
exit 0
GH
chmod +x "$tmp/bin/gh"
export PATH=$tmp/bin:$PATH CALLS=$tmp/calls
git init -q --bare "$tmp/origin.git"
git clone -q "$tmp/origin.git" "$tmp/w" 2>/dev/null
cd "$tmp/w" || exit 1
mkdir -p scripts
cp "$repo/scripts/dev.sh" scripts/
git checkout -q -b develop && echo a >a && git add -A && git commit -qm init && git push -q origin develop
git checkout -q -b release/1.10 && echo b >>a && git commit -qam line && git push -q origin release/1.10
git checkout -q develop && echo c >>a && git commit -qam dev2 && git push -q origin develop

# a feature starts from develop; its pull request goes into develop and merges itself
scripts/dev.sh start feat1 >/dev/null 2>&1
check "feature branch" "feature/feat1" "$(git rev-parse --abbrev-ref HEAD)"
check "feature base is develop" "$(git rev-parse origin/develop)" "$(git rev-parse HEAD)"
echo f >f && git add -A && git commit -qm "feat: f"
: >"$CALLS"
scripts/dev.sh push >/dev/null 2>&1
check "feature PR into develop" "1" "$(grep -c 'pr create --base develop --head feature/feat1' "$CALLS")"
check "feature: auto-merge asked" "1" "$(grep -c 'pr merge 5 --auto --merge' "$CALLS")"
git push -q origin HEAD:develop 2>/dev/null; git fetch -q
scripts/dev.sh "done" >/dev/null 2>&1
check "feature done: back on develop" "develop" "$(git rev-parse --abbrev-ref HEAD)"
check "feature done: local branch deleted" "0" "$(git branch --list 'feature/feat1' | wc -l | tr -d ' ')"

# a fix for a line starts from the line and its pull request goes into the line, with NO auto-merge
rm -f "$CALLS.pr"
scripts/dev.sh start bug1 --line 1.10 >/dev/null 2>&1
check "fix branch" "fix/1.10/bug1" "$(git rev-parse --abbrev-ref HEAD)"
check "fix starts from the line" "$(git rev-parse origin/release/1.10)" "$(git rev-parse HEAD)"
echo x >x && git add -A && git commit -qm "x"
: >"$CALLS"
out=$(scripts/dev.sh push 2>&1)
check "fix PR into the line" "1" "$(grep -c 'pr create --base release/1.10 --head fix/1.10/bug1' "$CALLS")"
check "fix: auto-merge is never asked" "0" "$(grep -c 'pr merge' "$CALLS")"
check "fix: says to merge by hand" "1" "$(grep -c 'no auto-merge' <<<"$out")"
# the fix is merged into the line by the owner; done deletes the branch and goes back to develop
git push -q origin HEAD:release/1.10 2>/dev/null
scripts/dev.sh "done" >/dev/null 2>&1
check "fix done: back on develop" "develop" "$(git rev-parse --abbrev-ref HEAD)"
check "fix done: local branch deleted" "0" "$(git branch --list 'fix/1.10/bug1' | wc -l | tr -d ' ')"

# --line forms, and refusals
git checkout -q develop
scripts/dev.sh start --line 1.10 bug2 >/dev/null 2>&1
check "--line before the name" "fix/1.10/bug2" "$(git rev-parse --abbrev-ref HEAD)"
git checkout -q develop
rc=0; scripts/dev.sh start bug3 --line 9.9 >/dev/null 2>&1 || rc=$?
check "unknown line is refused" "1" "$rc"
rc=0; scripts/dev.sh start bug3 --line abc >/dev/null 2>&1 || rc=$?
check "malformed line is refused" "1" "$rc"
# done refuses an unmerged fix
git checkout -q fix/1.10/bug2 && echo y >y && git add -A && git commit -qm y
rc=0; scripts/dev.sh "done" >/dev/null 2>&1 || rc=$?
check "done refuses a fix that is not in the line" "1" "$rc"
# the protected branches are refused
for b in develop release/1.10; do
  git checkout -q "$b"
  rc=0; scripts/dev.sh push >/dev/null 2>&1 || rc=$?
  check "push from $b is refused" "1" "$rc"
done
exit $fail
