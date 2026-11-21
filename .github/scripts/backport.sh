#!/usr/bin/env bash
# After a merge into a release line, brings the same change into develop as a pull request, so a fix never exists only in the line.
#   backport.sh     LINE_BRANCH=release/X.Y  BEFORE=<head of the line before the push>  AFTER=<head after>
# The commits that the push put on the first-parent path of the line (BEFORE..AFTER: one merge commit, one squash commit, or the commits of
# a rebase merge) are cherry-picked, in order, on the head of develop into the branch backport/X.Y/<7 hex of AFTER>, and one pull request into
# develop is opened for them (a merge commit is picked as one change, with -m 1).
#   - a commit already in develop, or whose change is already there (an empty pick), is left out; when nothing is left, nothing is opened;
#   - the branch exists: this push was brought back before (a re-run does not open it twice);
#   - a pick does not apply cleanly: no branch is pushed; an issue is opened that says so and how to do it by hand
#     (a branch with no commits cannot be a pull request, so the issue stands where the draft pull request would be).
# A pull request made with the default GITHUB_TOKEN does not start the checks. Set the secret BACKPORT_TOKEN (a fine-grained token or an
# app token with contents, pull requests and issues write) and they start; the repository setting "Allow GitHub Actions to create and
# approve pull requests" is needed either way.
# Environment: GH_TOKEN (gh), REMOTE (default origin), BASE (default develop).
set -euo pipefail

remote=${REMOTE:-origin}
base=${BASE:-develop}
line_branch=${LINE_BRANCH:?LINE_BRANCH is required}
before=${BEFORE:?BEFORE is required}
after=${AFTER:?AFTER is required}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
line=$("$here/release_version.py" line-of "$line_branch")

export GIT_AUTHOR_NAME=${GIT_AUTHOR_NAME:-github-actions[bot]} GIT_COMMITTER_NAME=${GIT_COMMITTER_NAME:-github-actions[bot]}
export GIT_AUTHOR_EMAIL=${GIT_AUTHOR_EMAIL:-41898282+github-actions[bot]@users.noreply.github.com}
export GIT_COMMITTER_EMAIL=${GIT_COMMITTER_EMAIL:-41898282+github-actions[bot]@users.noreply.github.com}

if [[ "$before" =~ ^0+$ ]]; then
  echo "$line_branch was just created: nothing to bring back"
  exit 0
fi
git cat-file -e "$before^{commit}" 2>/dev/null || { echo "::error::the previous head $before is not known here (a forced push?): bring $line_branch back by hand"; exit 1; }

git fetch --quiet "$remote" "$base"

open_issue() { # open_issue SUBJECT REASON: the commits are in $todo
  local subject=$1 reason=$2 title body picks=""
  local c
  for c in "${todo[@]}"; do
    picks+="git cherry-pick -x $([[ $(git rev-list --parents -n 1 "$c" | wc -w) -gt 2 ]] && echo '-m 1 ' || true)$c"$'\n'
  done
  title="Backport of ${after:0:7} into $base does not apply: $subject"
  if [[ -n "$(gh issue list --state open --search "in:title \"${after:0:7} into $base\"" --json number --jq '.[0].number // empty')" ]]; then
    echo "an issue for ${after:0:7} is already open"
    return 0
  fi
  body="The change merged into $line_branch (${after:0:7}, $subject) could not be brought into $base automatically: $reason

Do it by hand:

\`\`\`
git fetch origin
git checkout -b backport/$line/${after:0:7} origin/$base
$picks# resolve each conflict, then: git cherry-pick --continue
git push -u origin HEAD
\`\`\`

Then open a pull request into $base. If $base already has this fix in another form, close this issue."
  gh issue create --title "$title" --body "$body"
  echo "::warning::${after:0:7} does not apply to $base: an issue was opened"
}

short=${after:0:7}
branch="backport/$line/$short"
todo=()
while read -r c; do
  [[ -n "$c" ]] || continue
  if git merge-base --is-ancestor "$c" "$remote/$base"; then
    echo "${c:0:7} is already in $base"
  else
    todo+=("$c")
  fi
done < <(git rev-list --reverse --first-parent "$before..$after")
if [[ ${#todo[@]} -eq 0 ]]; then
  echo "nothing to bring back into $base"
  exit 0
fi
if git ls-remote --exit-code --heads "$remote" "$branch" >/dev/null 2>&1; then
  echo "$branch exists: this push was brought back before"
  exit 0
fi

subject=$(git log -1 --format=%s "$after")
git checkout --quiet --detach "$remote/$base"
picked=0
for c in "${todo[@]}"; do
  pick=(git cherry-pick -x)
  [[ $(git rev-list --parents -n 1 "$c" | wc -w) -gt 2 ]] && pick+=(-m 1)
  if "${pick[@]}" "$c" >/dev/null 2>&1; then
    picked=$((picked + 1))
  elif [[ -z "$(git ls-files -u)" ]] && git diff --quiet HEAD; then
    git cherry-pick --abort >/dev/null 2>&1 || true
    echo "the change of ${c:0:7} is already in $base (the pick is empty)"
  else
    git cherry-pick --abort >/dev/null 2>&1 || true
    open_issue "$subject" "${c:0:7} does not apply cleanly to the head of $base."
    exit 0
  fi
done
if [[ "$picked" -eq 0 ]]; then
  echo "nothing to bring back into $base"
  exit 0
fi
git push --quiet "$remote" "HEAD:refs/heads/$branch"
origin_pr=$(gh api "repos/$GITHUB_REPOSITORY/commits/$after/pulls" --jq '.[0].number // empty' 2>/dev/null || true)
body="The same change as ${short} in $line_branch${origin_pr:+ (pull request #$origin_pr)}, cherry-picked onto $base by the Backport workflow, so the fix is not lost when the line is replaced. Merge it with a merge commit."
gh pr create --base "$base" --head "$branch" --title "Backport to $base: $subject" --body "$body"
pr=$(gh pr list --head "$branch" --base "$base" --state open --json number --jq '.[0].number // empty')
if [[ -n "$pr" ]]; then
  gh pr merge "$pr" --auto --merge || echo "::notice::auto-merge is not available for #$pr; merge it by hand"
fi
