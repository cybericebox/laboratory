#!/usr/bin/env bash
# The local branch cycle, the same file in every CyberICEBox repository (no cross-repo import).
#   dev.sh start NAME     feature/NAME from the fresh develop
#   dev.sh push           push the branch, open or update the PR into develop, turn on auto-merge
#                         (MINOR=1 labels the PR "minor"; scripts/dev-pre-push.sh, when present, runs before the push)
#   dev.sh done           back to develop, pull it, delete the merged local branch
set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }

git rev-parse --show-toplevel >/dev/null 2>&1 || die "not a git repository"
cd "$(git rev-parse --show-toplevel)"
branch=$(git rev-parse --abbrev-ref HEAD)

refuse_protected() {
  case "$branch" in
    develop | main) die "you are on $branch; work in a feature branch (make dev-start NAME=<x>)" ;;
  esac
}

start() {
  local name=${NAME:-}
  [[ -n "$name" ]] || die "NAME is required: make dev-start NAME=<x>"
  [[ "$name" =~ ^[a-z0-9][a-z0-9._-]*$ ]] || die "NAME must be lowercase letters, digits, '.', '_' or '-'"
  git show-ref --verify --quiet "refs/heads/feature/$name" && die "branch feature/$name already exists"
  git fetch origin develop
  git checkout --no-track -b "feature/$name" origin/develop
}

push() {
  refuse_protected
  need gh
  if [[ -n "$(git status --porcelain)" ]]; then
    echo "warning: uncommitted changes are not part of the push:" >&2
    git status --short >&2
  fi
  if [[ -x scripts/dev-pre-push.sh ]]; then
    scripts/dev-pre-push.sh
  fi
  git push -u origin HEAD
  local pr
  pr=$(gh pr list --head "$branch" --base develop --state open --json number --jq '.[0].number // empty')
  if [[ -z "$pr" ]]; then
    gh pr create --base develop --head "$branch" --fill
    pr=$(gh pr list --head "$branch" --base develop --state open --json number --jq '.[0].number')
  else
    echo "PR #$pr is updated"
  fi
  if [[ -n "${MINOR:-}" && "${MINOR}" != "0" ]]; then
    gh pr edit "$pr" --add-label minor
  fi
  gh pr merge "$pr" --auto --merge
}

finish() {
  refuse_protected
  [[ -z "$(git status --porcelain)" ]] || die "uncommitted changes; commit or stash them first"
  git fetch --prune origin
  git checkout develop
  git pull --ff-only origin develop
  git branch -d "$branch" || die "$branch is not merged into develop yet (the PR may still be waiting for its checks)"
}

case "${1:-}" in
  start) start ;;
  push) push ;;
  done) finish ;;
  *) die "usage: dev.sh start|push|done" ;;
esac
