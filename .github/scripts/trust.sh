#!/usr/bin/env bash
# Sourced by backport.sh and security.sh (not run on its own).
#
# made_by_security COMMIT LINE: true when the Security workflow of THIS repository made COMMIT for the line X.Y (LINE).
# A trailer "Carried-from: <commit of develop>" is plain text that any author of a commit in a pull request can write, so both scripts count it
# only on a commit for which this holds (backport.sh: "this change is already in develop, do not bring it back"; security.sh: "the line already
# has this change, do not carry it"). Wrongly trusting a trailer silently loses a fix, so every doubt answers false:
#   - the API must show the commit verified, author github-actions[bot], committer web-flow (the identity of createCommitOnBranch with a job
#     token; any workflow with a job token, a fork's included, makes the same identity, so it is necessary but not enough);
#   - a pull request of the commit must have its head in this repository (head.repo.full_name = GITHUB_REPOSITORY) on a branch security/X.Y/*
#     (a fork cannot create branches here; only security.sh makes them);
#   - any API failure, or an empty answer, is false.
# Known limit: GitHub documents that commits/SHA/pulls may list only OPEN pull requests for a commit that is not on the default branch. A
# security commit lives only on its line, so once its pull request is merged the list may be empty; then this fails safe: backport.sh brings
# the change back (a duplicate pull request, or an empty pick that opens nothing) and security.sh sees no carried commit (its own check that
# the commits are in the line, or an empty pick, still stops a second pull request). To verify on the first real Security pull request that
# is merged into a line: gh api repos/<repo>/commits/<sha>/pulls (see "Releases" in CONTRIBUTING.md).
# Needs gh and GITHUB_REPOSITORY.
made_by_security() {
  local row prs
  row=$(gh api "repos/$GITHUB_REPOSITORY/commits/$1" --jq '[.commit.verification.verified, .author.login, .committer.login] | @tsv' 2>/dev/null) || return 1
  [[ "$row" == $'true\tgithub-actions[bot]\tweb-flow' ]] || return 1
  prs=$(gh api "repos/$GITHUB_REPOSITORY/commits/$1/pulls" --jq '.[] | [.head.repo.full_name, .head.ref] | @tsv' 2>/dev/null) || return 1
  awk -F'\t' -v repo="$GITHUB_REPOSITORY" -v pre="security/$2/" '$1 == repo && index($2, pre) == 1 && length($2) > length(pre) { ok = 1 } END { exit !ok }' <<<"$prs"
}
