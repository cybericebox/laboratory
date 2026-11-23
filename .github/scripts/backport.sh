#!/usr/bin/env bash
# After a merge into a release line, brings the same change into develop as a pull request, so a fix never exists only in the line.
#   backport.sh     LINE_BRANCH=release/X.Y  BEFORE=<head of the line before the push>  AFTER=<head after>
# The commits that the push put on the first-parent path of the line (BEFORE..AFTER: one merge commit, one squash commit, or the commits of
# a rebase merge) are cherry-picked, in order, on the head of develop in a scratch clone (git, nothing of it is pushed). The result is
# then committed through the GitHub API (backport_commit.py: GraphQL createCommitOnBranch with the token of the job), because the branches
# require signed commits and only a commit made by GitHub is signed; one commit with the whole change goes on the new branch
# backport/X.Y/<7 hex of AFTER>, and one pull request into develop is opened for it.
#   - a commit already in develop, or whose change is already there (an empty pick), is left out; when nothing is left, nothing is opened;
#   - a commit carried from develop by the Security workflow (security.sh) is left out: every commit it brings ends with the trailer
#     "Carried-from: <commit of develop>", so the change is already in develop and is never sent back (no loop). A trailer is only text
#     that any author of a commit in a pull request can write, so it counts only on a commit that GitHub made for the Security workflow
#     (verified, author github-actions[bot], committer web-flow, asked of the API); a trailer anywhere else, or one whose origin cannot
#     be checked, is ignored and the change is brought back (a duplicate pull request is harmless, a silently lost fix is not);
#   - the branch exists with a commit on it: this push was brought back before (a re-run does not open it twice); an empty branch left by
#     a failed run is made again;
#   - a pick does not apply cleanly, or the change cannot be sent through the API (a file mode, a symlink, a submodule, too large): no
#     branch; an issue is opened that says why and how to do it by hand (a branch with no commits cannot be a pull request);
#   - the commit that GitHub made must have the tree that the cherry-picks made and must be verified; otherwise the branch is deleted, an
#     issue is opened and the job fails.
# The token is the job's GITHUB_TOKEN (contents, pull requests, issues write); the checks of the pull request wait for the owner's approval
# ("Approve workflows to run"), see CONTRIBUTING.md. The repository setting "Allow GitHub Actions to create and approve pull requests" is needed.
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

made_by_security() { # made_by_security COMMIT: true when GitHub made it for the Security workflow (createCommitOnBranch, verified)
  local row
  row=$(gh api "repos/$GITHUB_REPOSITORY/commits/$1" --jq '[.commit.verification.verified, .author.login, .committer.login] | @tsv' 2>/dev/null) || return 1
  [[ "$row" == $'true\tgithub-actions[bot]\tweb-flow' ]]
}

carried() { # carried COMMIT: true when every commit it brings is the Security workflow's and has a Carried-from trailer naming a commit of develop
  local c=$1 k from n=0 own=()
  if [[ $(git rev-list --parents -n 1 "$c" | wc -w) -gt 2 ]]; then
    while read -r k; do [[ -n "$k" ]] && own+=("$k"); done < <(git rev-list --no-merges "$c^1..$c")
  else
    own=("$c")
  fi
  for k in "${own[@]}"; do
    from=$(git log -1 --format='%(trailers:key=Carried-from,valueonly)' "$k" | sed -n '/^[0-9a-f]\{40\}$/p' | tail -n 1)
    [[ -n "$from" ]] && git merge-base --is-ancestor "$from" "$remote/$base" && made_by_security "$k" || return 1
    n=$((n + 1))
  done
  [[ "$n" -gt 0 ]]
}

short=${after:0:7}
branch="backport/$line/$short"
todo=()
while read -r c; do
  [[ -n "$c" ]] || continue
  if git merge-base --is-ancestor "$c" "$remote/$base"; then
    echo "${c:0:7} is already in $base"
  elif carried "$c"; then
    echo "${c:0:7} was carried from $base by the Security workflow: it is already there"
  else
    todo+=("$c")
  fi
done < <(git rev-list --reverse --first-parent "$before..$after")
if [[ ${#todo[@]} -eq 0 ]]; then
  echo "nothing to bring back into $base"
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
tree=$(git rev-parse 'HEAD^{tree}')
base_oid=$(git rev-parse "$remote/$base")
if [[ "$tree" == "$(git rev-parse "$remote/$base^{tree}")" ]]; then
  echo "the picks leave $base as it is: nothing to bring back"
  exit 0
fi

# the branch: new, or left empty by a failed run (made again at the head of develop), or already carrying the commit (have)
state=new
if git ls-remote --exit-code --heads "$remote" "$branch" >/dev/null 2>&1; then
  git fetch --quiet "$remote" "$branch"
  if git merge-base --is-ancestor "$remote/$branch" "$remote/$base"; then
    echo "$branch has no commit of its own (a failed run): making it again"
    gh api -X DELETE "repos/$GITHUB_REPOSITORY/git/refs/heads/$branch" >/dev/null
  else
    echo "$branch exists with its commit: this push was brought back before"
    state=have
  fi
fi

if [[ "$state" == new ]]; then
  msg=$(mktemp)
  req=$(mktemp)
  err=$(mktemp)
  trap 'rm -f "$msg" "$req" "$err"' EXIT
  {
    echo "Brought back from $line_branch by the Backport workflow:"
    for c in "${todo[@]}"; do echo "- ${c:0:7} $(git log -1 --format=%s "$c")"; done
  } >"$msg"
  rc=0
  python3 "$here/backport_commit.py" body --base "$base_oid" --tree "$tree" --repo "$GITHUB_REPOSITORY" --branch "$branch" --head "$base_oid" \
    --headline="$subject" --message-file "$msg" >"$req" 2>"$err" || rc=$?
  case "$rc" in
    0) ;;
    3) open_issue "$subject" "it cannot be sent through the GitHub commit API: $(cat "$err")"; exit 0 ;;
    4) echo "the picks leave $base as it is: nothing to bring back"; exit 0 ;;
    *) cat "$err" >&2; echo "::error::backport_commit.py failed"; exit 1 ;;
  esac
  gh api -X POST "repos/$GITHUB_REPOSITORY/git/refs" -f "ref=refs/heads/$branch" -f "sha=$base_oid" >/dev/null
  if ! res=$(gh api graphql --input "$req") || ! jq -e '.data.createCommitOnBranch.commit.oid' <<<"$res" >/dev/null; then
    echo "::error::the API commit on $branch failed; the empty branch stays and the next run makes it again"
    exit 1
  fi
  git fetch --quiet "$remote" "$branch"
  made=$(git rev-parse FETCH_HEAD)
  verified=$(gh api "repos/$GITHUB_REPOSITORY/commits/$made" --jq .commit.verification.verified)
  if [[ "$(git rev-parse "$made^{tree}")" != "$tree" ]]; then
    gh api -X DELETE "repos/$GITHUB_REPOSITORY/git/refs/heads/$branch" >/dev/null
    open_issue "$subject" "the commit that GitHub made is not the same as the cherry-pick (a file that the API cannot reproduce exactly)."
    echo "::error::the API commit differs from the cherry-pick: $branch was deleted, an issue was opened"
    exit 1
  fi
  if [[ "$verified" != true ]]; then
    gh api -X DELETE "repos/$GITHUB_REPOSITORY/git/refs/heads/$branch" >/dev/null
    open_issue "$subject" "the commit that GitHub made is not verified, and the branches require signed commits."
    echo "::error::the API commit on $branch is not verified (verification.verified=$verified): the branch was deleted, an issue was opened"
    exit 1
  fi
  echo "$branch: commit ${made:0:7} (verified)"
fi

pr=$(gh pr list --head "$branch" --base "$base" --state all --json number --jq '.[0].number // empty')
if [[ -n "$pr" ]]; then
  echo "pull request #$pr for $branch exists"
  exit 0
fi
origin_pr=$(gh api "repos/$GITHUB_REPOSITORY/commits/$after/pulls" --jq '.[0].number // empty' 2>/dev/null || true)
body="The same change as ${short} in $line_branch${origin_pr:+ (pull request #$origin_pr)}, cherry-picked onto $base by the Backport workflow and committed through the GitHub API (so it is signed), so the fix is not lost when the line is replaced. Merge it with a merge commit."
gh pr create --base "$base" --head "$branch" --title "Backport to $base: $subject" --body "$body"
pr=$(gh pr list --head "$branch" --base "$base" --state open --json number --jq '.[0].number // empty')
if [[ -n "$pr" ]]; then
  gh pr merge "$pr" --auto --merge || echo "::notice::auto-merge is not available for #$pr; merge it by hand"
fi
