#!/usr/bin/env bash
# Carries a security fix that was merged into develop to the supported release lines, as one pull request per line.
#   security.sh     PR=<number> MERGE_SHA=<merge_commit_sha> PR_TITLE=<title> [PR_LABELS=a,b]
# The supported lines are the LINES (default 3) newest release/X.Y branches that exist (release_version.py newest-lines). For each one,
# in its own run of this script (LINE_BRANCH set), the change of the pull request is cherry-picked onto the head of the line in a scratch
# clone (git, nothing of it is pushed): the merge commit with -m 1, or the commits of a rebase merge in order (those are the newest commits
# of develop, as many as the pull request has commits of its own, refs/pull/<PR>/head; each must have the patch-id of its original, or
# nothing is carried and an issue says so). The result is
# committed through the GitHub API (backport_commit.py: GraphQL createCommitOnBranch with the token of the job), because the branches
# require signed commits and only a commit made by GitHub is signed; one commit goes on the new branch security/X.Y/<7 hex of MERGE_SHA>,
# and one pull request into release/X.Y with the label security is opened for it (no auto-merge: the owner merges into a line by hand,
# and the Line gate judges it like any fix; the label large-fix of the source pull request is carried along).
# The commit message ends with the trailer "Carried-from: <MERGE_SHA>": backport.sh sees it after the merge into the line and does not
# bring the change back into develop, where it already is. backport.sh honours the trailer only on a commit that this workflow made
# in this repository (verified, author github-actions[bot], committer web-flow, and a pull request from a branch security/X.Y/* of this
# repository), since anyone can write the text in a pull request. A manual carry therefore has no trailer.
#   - the line already has the change (the commits are in it, a commit of it carries the same trailer, or the pick is empty): nothing;
#   - the branch exists with a commit on it: this line was done before (a re-run does not open it twice); an empty branch left by a failed
#     run is made again;
#   - a pick does not apply cleanly, or the change cannot be sent through the API (a file mode, a symlink, a submodule, a workflow file,
#     too large): no branch; an issue with the label security is opened that says why and how to do it by hand (once: a closed issue
#     is not opened again);
#   - the commit that GitHub made must have the tree that the cherry-picks made and must be verified; otherwise the branch is deleted, an
#     issue is opened and the run of that line fails. A failed line does not stop the others; the job fails at the end.
# The token is the job's GITHUB_TOKEN (contents, pull requests, issues write); the checks of the pull request wait for the owner's approval
# ("Approve workflows to run"), see CONTRIBUTING.md. The repository setting "Allow GitHub Actions to create and approve pull requests" is needed.
# Environment: GH_TOKEN (gh), REMOTE (default origin), BASE (default develop), LINES (default 3).
set -euo pipefail

remote=${REMOTE:-origin}
base=${BASE:-develop}
count=${LINES:-3}
pr=${PR:?PR is required}
merge=${MERGE_SHA:?MERGE_SHA is required}
title=${PR_TITLE:?PR_TITLE is required}
labels=${PR_LABELS:-}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
short=${merge:0:7}
subject=$title
subject_md=${subject//\`/\'}  # for a code span in the bodies

export GIT_AUTHOR_NAME=${GIT_AUTHOR_NAME:-github-actions[bot]} GIT_COMMITTER_NAME=${GIT_COMMITTER_NAME:-github-actions[bot]}
export GIT_AUTHOR_EMAIL=${GIT_AUTHOR_EMAIL:-41898282+github-actions[bot]@users.noreply.github.com}
export GIT_COMMITTER_EMAIL=${GIT_COMMITTER_EMAIL:-41898282+github-actions[bot]@users.noreply.github.com}

[[ "$merge" =~ ^[0-9a-f]{40}$ ]] || { echo "::error::MERGE_SHA is not a commit id: $merge"; exit 1; }

open_issue() { # open_issue TARGET REASON
  local target=$1 reason=$2 t body p found cmds=""
  for p in ${picks[@]+"${picks[@]}"}; do
    cmds+="git cherry-pick -x $([[ $(git rev-list --parents -n 1 "$p" | wc -w) -gt 2 ]] && echo '-m 1 ' || true)$p"$'\n'
  done
  t="Security fix $short into $target does not apply: $subject"
  found=$(gh issue list --state all --search "in:title \"$short into $target\"" --json title --jq '.[].title')
  if grep -qxF -- "$t" <<<"$found"; then
    echo "an issue for $short into $target exists"
    return 0
  fi
  body="The security fix of pull request #$pr (merged into $base as $short, \`$subject_md\`) could not be carried to $target automatically: $reason

Do it by hand, for each line that needs it (release/X.Y):

\`\`\`
git fetch origin
git checkout -b security/X.Y/$short origin/release/X.Y
$cmds# resolve each conflict, then: git cherry-pick --continue
git reset --soft origin/release/X.Y
git commit -m 'Security fix: <what it fixes>'
git push -u origin HEAD
\`\`\`

Then open a pull request into release/X.Y with the label security (the owner merges it by hand). No trailer is needed: after the merge
the Backport workflow cherry-picks the change onto $base, finds it already there (an empty pick) and opens nothing; if $base has changed
the same lines since, it opens an issue that says so, and that issue can be closed. If a line does not need the fix, close this issue."
  gh issue create --title "$t" --body "$body" --label security
  echo "::warning::$short does not apply to $target: an issue was opened"
}

if [[ -z "${LINE_BRANCH:-}" ]]; then
  git fetch --quiet "$remote" "$base" "+refs/heads/release/*:refs/remotes/$remote/release/*" "+refs/pull/$pr/head:refs/remotes/$remote/pr/$pr"
  git merge-base --is-ancestor "$merge" "$remote/$base" || { echo "::error::$short is not part of $base"; exit 1; }

  # The commits that carry the change, in order: the merge commit (picked with -m 1), or the commits a rebase merge put on develop.
  picks=()
  if [[ $(git rev-list --parents -n 1 "$merge" | wc -w) -gt 2 ]]; then
    picks=("$merge")
  else
    # A rebase merge: GitHub rewrote each commit of the pull request that is not a merge onto develop. Take as many of the newest
    # commits of develop, and prove that each is the rewrite of its original (same patch-id); otherwise carry nothing.
    own=$(git rev-list --reverse --no-merges "$remote/pr/$pr" --not "$remote/$base")
    n=$(grep -c . <<<"$own" || true)
    new=$(git rev-list --reverse --first-parent --max-count="$n" "$merge")
    same=$([[ "$n" -gt 0 ]] && echo yes || echo no)
    i=0
    origs=()
    while read -r o; do [[ -n "$o" ]] && origs+=("$o"); done <<<"$own"
    while read -r c; do
      [[ -n "$c" && "$same" == yes ]] || continue
      if [[ $(git rev-list --parents -n 1 "$c" | wc -w) -gt 2 ]] ||
        [[ "$(git show --format= "$c" | git patch-id --stable | cut -d' ' -f1)" != "$(git show --format= "${origs[$i]}" | git patch-id --stable | cut -d' ' -f1)" ]]; then
        same=no
      fi
      picks+=("$c")
      i=$((i + 1))
    done <<<"$new"
    if [[ "$same" != yes || ${#picks[@]} -ne "$n" ]]; then
      echo "::error::#$pr was rebased onto $base, but its commits there cannot be matched to the pull request ($n of its own)"
      picks=()
      open_issue "the release lines" "the rebase merge cannot be matched commit by commit to the pull request: find its commits in $base and pick them."
      exit 1
    fi
  fi

  heads=$(git ls-remote --heads "$remote" 'release/*')
  names=$(cut -f2 <<<"$heads" | "$here/release_version.py" newest-lines --count "$count")
  lines=()
  while read -r b; do
    [[ -n "$b" ]] && lines+=("$b")
  done <<<"$names"
  if [[ ${#lines[@]} -eq 0 ]]; then
    echo "no release line exists: nothing to carry"
    exit 0
  fi
  echo "supported lines: ${lines[*]}"
  failed=()
  for b in "${lines[@]}"; do
    echo "== $b"
    # Each line in a process of its own, so set -e holds inside it and a failed line does not stop the next one.
    if ! LINE_BRANCH=$b PICKS="${picks[*]}" "${BASH_SOURCE[0]}"; then
      failed+=("$b")
    fi
  done
  if [[ ${#failed[@]} -gt 0 ]]; then
    echo "::error::carrying $short failed for: ${failed[*]} (see above; a re-run of the job continues)"
    exit 1
  fi
  exit 0
fi

read -r -a picks <<<"${PICKS:?PICKS is required with LINE_BRANCH}"
last=${picks[${#picks[@]}-1]}
line_branch=$LINE_BRANCH
line=$("$here/release_version.py" line-of "$line_branch")
head="$remote/$line_branch"
head_oid=$(git rev-parse "$head")
branch="security/$line/$short"

if git merge-base --is-ancestor "$last" "$head"; then
  echo "$line_branch already has $short"
  exit 0
fi
carried=$(git log --format='%(trailers:key=Carried-from,valueonly)' "$head")
if grep -qxF "$merge" <<<"$carried"; then
  echo "$line_branch already has $short (carried before)"
  exit 0
fi

# the branch: new, or left empty by a failed run (made again at the head of the line), or already carrying the commit (have)
state=new
if git ls-remote --exit-code --heads "$remote" "$branch" >/dev/null 2>&1; then
  git fetch --quiet "$remote" "$branch"
  if git merge-base --is-ancestor FETCH_HEAD "$head"; then
    echo "$branch has no commit of its own (a failed run): making it again"
    gh api -X DELETE "repos/$GITHUB_REPOSITORY/git/refs/heads/$branch" >/dev/null
  else
    echo "$branch exists with its commit: this line was done before"
    state=have
  fi
fi

if [[ "$state" == new ]]; then
  git checkout --quiet --detach "$head"
  picked=0
  for c in "${picks[@]}"; do
    pick=(git cherry-pick -x)
    [[ $(git rev-list --parents -n 1 "$c" | wc -w) -gt 2 ]] && pick+=(-m 1)
    if "${pick[@]}" "$c" >/dev/null 2>&1; then
      picked=$((picked + 1))
    elif [[ -z "$(git ls-files -u)" ]] && git diff --quiet HEAD; then
      git cherry-pick --abort >/dev/null 2>&1 || true
      echo "the change of ${c:0:7} is already in $line_branch (the pick is empty)"
    else
      git cherry-pick --abort >/dev/null 2>&1 || true
      open_issue "$line_branch" "${c:0:7} does not apply cleanly to the head of $line_branch."
      exit 0
    fi
  done
  tree=$(git rev-parse 'HEAD^{tree}')
  if [[ "$picked" -eq 0 || "$tree" == "$(git rev-parse "$head^{tree}")" ]]; then
    echo "the picks leave $line_branch as it is: nothing to carry"
    exit 0
  fi
  msg=$(mktemp)
  req=$(mktemp)
  err=$(mktemp)
  trap 'rm -f "$msg" "$req" "$err"' EXIT
  {
    echo "Carried from $base (pull request #$pr, $short) to $line_branch by the Security workflow:"
    for c in "${picks[@]}"; do echo "- ${c:0:7} $(git log -1 --format=%s "$c")"; done
    echo
    echo "Carried-from: $merge"
  } >"$msg"
  rc=0
  python3 "$here/backport_commit.py" body --base "$head_oid" --tree "$tree" --repo "$GITHUB_REPOSITORY" --branch "$branch" --head "$head_oid" \
    --headline="$subject" --message-file "$msg" >"$req" 2>"$err" || rc=$?
  case "$rc" in
    0) ;;
    3) open_issue "$line_branch" "it cannot be sent through the GitHub commit API: $(cat "$err")"; exit 0 ;;
    4) echo "the picks leave $line_branch as it is: nothing to carry"; exit 0 ;;
    *) cat "$err" >&2; echo "::error::backport_commit.py failed"; exit 1 ;;
  esac
  gh api -X POST "repos/$GITHUB_REPOSITORY/git/refs" -f "ref=refs/heads/$branch" -f "sha=$head_oid" >/dev/null
  if ! res=$(gh api graphql --input "$req") || ! jq -e '.data.createCommitOnBranch.commit.oid' <<<"$res" >/dev/null; then
    echo "::error::the API commit on $branch failed; the empty branch stays and the next run makes it again"
    exit 1
  fi
  git fetch --quiet "$remote" "$branch"
  made=$(git rev-parse FETCH_HEAD)
  verified=$(gh api "repos/$GITHUB_REPOSITORY/commits/$made" --jq .commit.verification.verified)
  if [[ "$(git rev-parse "$made^{tree}")" != "$tree" ]]; then
    gh api -X DELETE "repos/$GITHUB_REPOSITORY/git/refs/heads/$branch" >/dev/null
    open_issue "$line_branch" "the commit that GitHub made is not the same as the cherry-pick (a file that the API cannot reproduce exactly)."
    echo "::error::the API commit differs from the cherry-pick: $branch was deleted, an issue was opened"
    exit 1
  fi
  if [[ "$verified" != true ]]; then
    gh api -X DELETE "repos/$GITHUB_REPOSITORY/git/refs/heads/$branch" >/dev/null
    open_issue "$line_branch" "the commit that GitHub made is not verified, and the branches require signed commits."
    echo "::error::the API commit on $branch is not verified (verification.verified=$verified): the branch was deleted, an issue was opened"
    exit 1
  fi
  echo "$branch: commit ${made:0:7} (verified)"
fi

existing=$(gh pr list --head "$branch" --base "$line_branch" --state all --json number --jq '.[0].number // empty')
if [[ -n "$existing" ]]; then
  echo "pull request #$existing for $branch exists"
  exit 0
fi
add=(--label security)
[[ ",$labels," == *",large-fix,"* ]] && add+=(--label large-fix)
body="The security fix of pull request #$pr (merged into $base as $short, \`$subject_md\`), cherry-picked onto $line_branch by the Security workflow and committed through the GitHub API (so it is signed). The three newest release lines get it; the owner merges each by hand. The Line gate judges it like any fix (no harness files; above 400 changed lines it needs the label large-fix). The trailer Carried-from keeps the Backport workflow from bringing it back into $base, where it already is. If Check passed or Line gate does not start, close and reopen this pull request (an event of a person starts both)."
gh pr create --base "$line_branch" --head "$branch" --title "Security fix to $line_branch: $subject" --body "$body" "${add[@]}"
