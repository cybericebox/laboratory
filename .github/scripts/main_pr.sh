#!/usr/bin/env bash
# Brings a promoted release into main through a pull request made with the job's own token (no other credential, no bypass actor).
#   main_pr.sh      VERSION=vX.Y.Z  SHA=<the promoted commit>   environment: GH_TOKEN (gh), REMOTE (default origin), BASE (default main),
#                   POLL_SLEEP (seconds between polls of the mergeable state, default 5)
#   - main already contains SHA: nothing to do (a re-run after the merge);
#   - the branch promote/VERSION is created at SHA, or kept when it exists and contains SHA (a re-run, or a conflict resolution on it);
#   - the open pull request promote/VERSION -> main is reused, otherwise one is opened; auto-merge (a merge commit) is turned on;
#   - when GitHub says the pull request conflicts, the job fails with what to do: main only ever receives lines, so a conflict means that
#     main was changed by hand.
# A pull request opened with GITHUB_TOKEN does not start its checks by itself, see CONTRIBUTING.md ("Pull requests that automation opens").
set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }
remote=${REMOTE:-origin}
base=${BASE:-main}
version=${VERSION:?VERSION is required}
sha=${SHA:?SHA is required}
branch="promote/$version"
summary=${GITHUB_STEP_SUMMARY:-/dev/null}

git fetch --quiet "$remote" "$base"
if git merge-base --is-ancestor "$sha" "$remote/$base"; then
  echo "$base already has ${sha:0:7}: nothing to do"
  exit 0
fi

if git ls-remote --exit-code --heads "$remote" "$branch" >/dev/null 2>&1; then
  git fetch --quiet "$remote" "$branch"
  git merge-base --is-ancestor "$sha" "$remote/$branch" || die "$branch exists but does not contain ${sha:0:7}: delete or fix that branch, then run Promote again"
  echo "$branch exists"
else
  git push --quiet "$remote" "$sha:refs/heads/$branch"
  echo "$branch created at ${sha:0:7}"
fi

pr=$(gh pr list --head "$branch" --base "$base" --state open --json number --jq '.[0].number // empty')
if [[ -z "$pr" ]]; then
  gh pr create --base "$base" --head "$branch" --title "Release $version" \
    --body "Brings the release $version (commit ${sha:0:7}) into $base. Opened by the Promote workflow after the release was published; it merges itself (merge commit) when the check is green."
  pr=$(gh pr list --head "$branch" --base "$base" --state open --json number --jq '.[0].number')
fi
url=$(gh pr view "$pr" --json url --jq .url)
gh pr merge "$pr" --auto --merge --subject "Release $version" || echo "::warning::auto-merge could not be turned on for #$pr: merge it by hand when its check is green"

state=UNKNOWN
for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
  state=$(gh pr view "$pr" --json mergeable --jq .mergeable)
  [[ "$state" != UNKNOWN ]] && break
  sleep "${POLL_SLEEP:-5}"
done

{
  echo "### Release $version: pull request into $base"
  echo
  echo "$url"
  echo
  echo "The release is published. $base gets it when this pull request merges (auto-merge is on, merge commit)."
  echo "- The checks of a pull request opened by a workflow wait for approval: open the pull request and press **Approve workflows to run**."
  echo "  If there is no such button, close the pull request and reopen it."
} >>"$summary"

if [[ "$state" == CONFLICTING ]]; then
  {
    echo
    echo "**Conflict.** $base has changes that $branch does not have and they do not merge: $base only ever receives promoted lines, so it was changed by hand."
    echo "Merge $base into the branch, resolve, push (the check accepts a head that contains the tagged commit):"
    echo
    echo "    git fetch origin && git checkout -B $branch origin/$branch && git merge origin/$base"
    echo "    # resolve, commit, then"
    echo "    git push origin $branch"
  } >>"$summary"
  echo "::error::the pull request #$pr into $base conflicts ($base was changed by hand). The release $version is published. Resolve it: git checkout -B $branch origin/$branch && git merge origin/$base, then push $branch"
  exit 1
fi
echo "pull request #$pr: $url (mergeable: $state)"
