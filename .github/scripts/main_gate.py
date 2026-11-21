#!/usr/bin/env python3
"""The gate of a pull request into main. Standard library only. It reads git objects as data and runs nothing of the pull request.

  main_gate.py is-promote HEAD_REF            exit 0 when the head branch is promote/vX.Y.Z (a promote pull request)
  main_gate.py check HEAD_REF HEAD_SHA        run in a clone that has the tags and every branch; exit 0 when the pull request may carry a
                                              release into main

main only receives promoted lines. The pull request that Promote opens (head promote/vX.Y.Z) carries commits that were checked when each one
entered the line, so the tests are not run again. What the gate requires, exactly:
  1. the head branch is promote/vX.Y.Z (ASCII, the whole name);
  2. the tag vX.Y.Z exists, and its commit T is part of the branch release/X.Y (a final tag is only made by Promote after publication;
     a tag made by hand on another commit is not on the line, so it does not count);
  3. the head IS T, or the head is a merge commit of exactly two parents, T and a commit P that main already has, whose tree is the tree
     of the automatic merge of T and P (git merge-tree). Nothing else rides along: an extra commit, a merge with a conflict resolved by
     hand, a different tree are all refused. A conflict resolved by hand is a different kind of change: it goes in a pull request from
     any other branch, which gets the full check (check.yml).
Anything else, including every pull request into main that is not from promote/vX.Y.Z, fails: main takes lines only (the one-time
bootstrap develop -> main is done by the owner, see CONTRIBUTING.md).
This runs from the default branch through pull_request_target (main-gate.yml), so the pull request cannot change the gate.
"""
import re
import subprocess
import sys

PROMOTE = re.compile(r"promote/(v(\d+)\.(\d+)\.\d+)", re.ASCII)


def version_of(head_ref):
    m = PROMOTE.fullmatch(head_ref or "")
    return m.group(1) if m else None


def line_of(head_ref):
    m = PROMOTE.fullmatch(head_ref or "")
    return f"{int(m.group(2))}.{int(m.group(3))}" if m else None


def git(*args):
    return subprocess.run(["git", "-c", "core.hooksPath=/dev/null", *args], capture_output=True, text=True, check=False)


def problems(head_ref, head_sha, run=git, main_ref="refs/remotes/origin/main"):
    version = version_of(head_ref)
    if not version:
        return [f"{head_ref!r} is not a promote branch (promote/vX.Y.Z): main takes promoted lines only"]
    if not re.fullmatch(r"[0-9a-f]{40}", head_sha or ""):
        return [f"{head_sha!r} is not a commit id"]
    tagged = run("rev-parse", "-q", "--verify", f"refs/tags/{version}^{{commit}}")
    if tagged.returncode != 0:
        return [f"the release {version} does not exist: Promote creates its tag before it opens this pull request"]
    tag_sha = tagged.stdout.strip()
    line = line_of(head_ref)
    if run("merge-base", "--is-ancestor", tag_sha, f"refs/remotes/origin/release/{line}").returncode != 0:
        return [f"the commit of {version} ({tag_sha[:7]}) is not part of release/{line}"]
    if head_sha == tag_sha:
        return []
    parents = run("rev-list", "--parents", "-n", "1", head_sha).stdout.split()[1:]
    if len(parents) != 2 or parents[0] != tag_sha:
        return [f"the head {head_sha[:7]} is neither the commit of {version} ({tag_sha[:7]}) nor a merge of main into it: no other commit may ride along"]
    if run("merge-base", "--is-ancestor", parents[1], main_ref).returncode != 0:
        return [f"the second parent {parents[1][:7]} of the head is not a commit that main has"]
    merged = run("merge-tree", "--write-tree", tag_sha, parents[1])
    if merged.returncode != 0:
        return ["the merge of main into the release conflicts (or cannot be computed): a hand-made resolution is not accepted here, use a pull request from another branch (full check)"]
    tree = run("rev-parse", f"{head_sha}^{{tree}}").stdout.strip()
    if not tree or merged.stdout.split()[0] != tree:
        return ["the tree of the merge commit is not the automatic merge of the release and main: it carries other changes"]
    return []


def main(argv):
    if len(argv) == 2 and argv[0] == "is-promote":
        return 0 if version_of(argv[1]) else 1
    if len(argv) == 3 and argv[0] == "check":
        found = problems(argv[1], argv[2])
        for p in found:
            print(f"main-gate: {p}", file=sys.stderr)
        if not found:
            print(f"main-gate: ok ({argv[1]})")
        return 1 if found else 0
    print(__doc__, file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
