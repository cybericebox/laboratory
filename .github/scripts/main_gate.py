#!/usr/bin/env python3
"""The check of a pull request into main that Promote opens. Standard library only; the pull request's own workflow runs it.

  main_gate.py is-promote HEAD_REF            exit 0 when the head branch is promote/vX.Y.Z (a promote pull request)
  main_gate.py check HEAD_REF HEAD_SHA        run in a clone that has the tags; exit 0 when the pull request may carry a release into main

main only receives promoted lines. A pull request from promote/vX.Y.Z is light: the commits of the line were checked when each one
entered it, so the tests are not run again; instead this gate requires that vX.Y.Z exists (a final tag is only created by Promote, after
the images, the chart and the release page are published) and that the head of the pull request contains the tagged commit (it is the
tagged commit itself, or the tagged commit plus a conflict resolution merged in by hand). Any other pull request into main (for
example the one-time bootstrap from develop) is not "promote": it gets the full check.
This is a check, not a lock: a pull request runs the workflow of its own head, so the owner's merge is the real control of main.
"""
import re
import subprocess
import sys

PROMOTE = re.compile(r"^promote/(v\d+\.\d+\.\d+)$")


def version_of(head_ref):
    m = PROMOTE.match(head_ref or "")
    return m.group(1) if m else None


def git(*args):
    return subprocess.run(["git", *args], capture_output=True, text=True, check=False)


def problems(head_ref, head_sha, run=git):
    version = version_of(head_ref)
    if not version:
        return [f"{head_ref!r} is not a promote branch (promote/vX.Y.Z)"]
    tagged = run("rev-parse", "-q", "--verify", f"refs/tags/{version}^{{commit}}")
    if tagged.returncode != 0:
        return [f"the release {version} does not exist: Promote creates its tag before it opens this pull request"]
    tag_sha = tagged.stdout.strip()
    if run("merge-base", "--is-ancestor", tag_sha, head_sha).returncode != 0:
        return [f"the head {head_sha[:7]} does not contain the commit of {version} ({tag_sha[:7]})"]
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
