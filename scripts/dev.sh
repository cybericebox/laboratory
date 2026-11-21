#!/usr/bin/env bash
# The local branch cycle, the same file in every CyberICEBox repository (no cross-repo import).
#   dev.sh start NAME [--line X.Y]   feature/NAME from the fresh develop; with --line, fix/X.Y/NAME from the fresh release/X.Y
#   dev.sh push                      push the branch, open or update the pull request into its base (develop; release/X.Y for fix/X.Y/*).
#                                    Into develop it turns on auto-merge; into a release line it does NOT: the owner merges a line by hand.
#                                    (scripts/dev-pre-push.sh, when present, runs before the push)
#   dev.sh done                      back to develop, pull it, delete the merged local branch
set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }

git rev-parse --show-toplevel >/dev/null 2>&1 || die "not a git repository"
cd "$(git rev-parse --show-toplevel)"
branch=$(git rev-parse --abbrev-ref HEAD)

# the release line of a fix branch (fix/X.Y/name), empty for any other branch
line_of_branch() {
  if [[ "$1" =~ ^fix/([0-9]+\.[0-9]+)/ ]]; then echo "${BASH_REMATCH[1]}"; fi
}

refuse_protected() {
  case "$branch" in
    develop | main | release/*) die "you are on $branch; work in a branch (mise run dev-start <name>, or --line X.Y for a fix in a release line)" ;;
  esac
}

start() {
  local name="" line=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --line) [[ $# -ge 2 ]] || die "--line needs a value (X.Y)"; line=$2; shift 2 ;;
      --line=*) line=${1#--line=}; shift ;;
      -*) die "unknown option $1" ;;
      *) [[ -z "$name" ]] || die "one name only"; name=$1; shift ;;
    esac
  done
  name=${name:-${NAME:-}}
  [[ -n "$name" ]] || die "a name is required: mise run dev-start <name> [--line X.Y]"
  [[ "$name" =~ ^[a-z0-9][a-z0-9._-]*$ ]] || die "the name must be lowercase letters, digits, '.', '_' or '-'"
  if [[ -n "$line" ]]; then
    [[ "$line" =~ ^[0-9]+\.[0-9]+$ ]] || die "--line is two numbers, X.Y (got '$line')"
    git ls-remote --exit-code --heads origin "release/$line" >/dev/null 2>&1 || die "origin has no release/$line"
    git show-ref --verify --quiet "refs/heads/fix/$line/$name" && die "branch fix/$line/$name already exists"
    git fetch origin "release/$line"
    git checkout --no-track -b "fix/$line/$name" "origin/release/$line"
    echo "A fix for release/$line. Keep it small: the line takes pull requests with green checks and at most 400 changed lines."
    return 0
  fi
  git show-ref --verify --quiet "refs/heads/feature/$name" && die "branch feature/$name already exists"
  git fetch origin develop
  git checkout --no-track -b "feature/$name" origin/develop
}

push() {
  refuse_protected
  need gh
  local line base pr
  line=$(line_of_branch "$branch")
  base=develop
  [[ -z "$line" ]] || base="release/$line"
  if [[ -n "$(git status --porcelain)" ]]; then
    echo "warning: uncommitted changes are not part of the push:" >&2
    git status --short >&2
  fi
  if [[ -x scripts/dev-pre-push.sh ]]; then
    scripts/dev-pre-push.sh
  fi
  git push -u origin HEAD
  pr=$(gh pr list --head "$branch" --base "$base" --state open --json number --jq '.[0].number // empty')
  if [[ -z "$pr" ]]; then
    gh pr create --base "$base" --head "$branch" --fill
    pr=$(gh pr list --head "$branch" --base "$base" --state open --json number --jq '.[0].number')
  else
    echo "PR #$pr is updated"
  fi
  if [[ -n "$line" ]]; then
    echo "PR #$pr into $base: no auto-merge. Merge it by hand when the checks are green (a change above 400 lines needs the label large-fix)."
    echo "A merge builds the next candidate; the Backport workflow then opens the same change as a pull request into develop."
  else
    gh pr merge "$pr" --auto --merge
  fi
}

finish() {
  refuse_protected
  [[ -z "$(git status --porcelain)" ]] || die "uncommitted changes; commit or stash them first"
  local line
  line=$(line_of_branch "$branch")
  git fetch --prune origin
  if [[ -n "$line" ]]; then
    # a fix is merged into the line, not into develop (develop gets it through the backport)
    git merge-base --is-ancestor "$branch" "origin/release/$line" || die "$branch is not merged into release/$line yet"
    git checkout develop
    git pull --ff-only origin develop
    git branch -D "$branch"
    return 0
  fi
  git checkout develop
  git pull --ff-only origin develop
  git branch -d "$branch" || die "$branch is not merged into develop yet (the PR may still be waiting for its checks)"
}

case "${1:-}" in
  start) shift; start "$@" ;;
  push) push ;;
  done) finish ;;
  *) die "usage: dev.sh start NAME [--line X.Y] | push | done" ;;
esac
