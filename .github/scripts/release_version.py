#!/usr/bin/env python3
"""Version arithmetic of the release lines. Standard library only.

A release line is the branch release/X.Y. Everything is computed from the branch name and the existing git tags, nothing else
(no labels, no commit messages).

  release_version.py line-of BRANCH       X.Y of release/X.Y (exit 1 for any other branch)
  release_version.py next-rc LINE         stdin: the tags. Prints  version=vX.Y.Z  tag=vX.Y.Z-rc.N
  release_version.py existing-rc LINE [TAG...]
                                          stdin: all tags; TAG...: the tags on the commit. Prints version= and tag= of the candidate the commit has
  release_version.py is-latest VERSION    stdin: the tags. Exit 0 when VERSION is the highest final release
  release_version.py stale-rc             stdin: the tags. Prints the candidates whose version has a final release (to be deleted)
  release_version.py prune [--from X.Y] [--keep N]
                                          stdin: the tags. Prints the final releases whose page and images are deleted (the git tag stays)
  release_version.py newest-lines [--count N]
                                          stdin: branch names (or refs/heads/...). Prints the N (default 3) newest release/X.Y, newest
                                          first: the lines that take security fixes

Final tags are vX.Y.Z (the old two-number vX.Y counts as vX.Y.0). Candidates are vX.Y.Z-rc.N. Any other tag is ignored.
Z of the next candidate is 0 when the line has no final release, otherwise the newest final patch + 1, so after vX.Y.0 is promoted the
next merge into the line gives vX.Y.1-rc.1. N is the number of the newest candidate of that version + 1 (a gap or an out-of-order tag
does not matter: only the maximum counts).
"""
import re
import sys

# ASCII digits only and the whole string (fullmatch): a Unicode digit or a trailing newline must never read as a version.
FINAL = re.compile(r"v(\d+)\.(\d+)(?:\.(\d+))?", re.ASCII)
RC = re.compile(r"v(\d+)\.(\d+)\.(\d+)-rc\.(\d+)", re.ASCII)
BRANCH = re.compile(r"release/(\d+)\.(\d+)", re.ASCII)
LINE = re.compile(r"(\d+)\.(\d+)", re.ASCII)


def finals(tags):
    """{(x, y, z): tag} of the final releases."""
    out = {}
    for t in tags:
        m = FINAL.fullmatch(t.strip())
        if m:
            out[(int(m[1]), int(m[2]), int(m[3] or 0))] = t.strip()
    return out


def candidates(tags):
    """[(x, y, z, n)] of the release candidates."""
    out = []
    for t in tags:
        m = RC.fullmatch(t.strip())
        if m:
            out.append(tuple(int(g) for g in m.groups()))
    return out


def line_of(branch):
    m = BRANCH.fullmatch(branch)
    if not m:
        raise ValueError(f"{branch!r} is not a release line (release/X.Y)")
    return f"{int(m[1])}.{int(m[2])}"


def parse_line(line):
    m = LINE.fullmatch(line)
    if not m:
        raise ValueError(f"{line!r} is not a line (X.Y)")
    return int(m[1]), int(m[2])


def next_rc(line, tags):
    """(version, tag) of the next candidate of the line."""
    x, y = parse_line(line)
    z_final = [k[2] for k in finals(tags) if k[:2] == (x, y)]
    z = max(z_final) + 1 if z_final else 0
    n = max([c[3] for c in candidates(tags) if c[:3] == (x, y, z)], default=0) + 1
    version = f"v{x}.{y}.{z}"
    return version, f"{version}-rc.{n}"


def existing_rc(line, at_commit, tags):
    """(version, tag) of the candidate a commit already carries, or None. Only a tag that the arithmetic itself would have given counts
    (X.Y of the line, Z = the next patch): a hand-made tag such as v1.2.9-rc.1 on the commit is ignored, never adopted."""
    x, y = parse_line(line)
    z_final = [k[2] for k in finals(tags) if k[:2] == (x, y)]
    z = max(z_final) + 1 if z_final else 0
    mine = [c for c in candidates(at_commit) if c[:3] == (x, y, z)]
    if not mine:
        return None
    n = max(c[3] for c in mine)
    return f"v{x}.{y}.{z}", f"v{x}.{y}.{z}-rc.{n}"


def is_latest(version, tags):
    m = FINAL.fullmatch(version)
    if not m:
        raise ValueError(f"{version!r} is not a final version")
    mine = (int(m[1]), int(m[2]), int(m[3] or 0))
    return all(k <= mine for k in finals(tags))


def stale_rc(tags):
    """Candidates of versions that have been released: nothing needs them any more."""
    released = set(finals(tags))
    out = [f"v{x}.{y}.{z}-rc.{n}" for x, y, z, n in candidates(tags) if (x, y, z) in released]
    return sorted(out, key=lambda t: [int(p) for p in re.findall(r"\d+", t)])


def prune(tags, keep=10, floor=(0, 0)):
    """Final tags to delete (page and images): the newest line keeps every patch, each of the `keep` previous lines keeps its newest
    patch, older lines keep nothing. Lines below `floor` are outside the policy and never listed."""
    by_line = {}
    for k, t in finals(tags).items():
        if k[:2] >= floor:
            by_line.setdefault(k[:2], []).append((k, t))
    doomed = []
    for i, ln in enumerate(sorted(by_line, reverse=True)):
        rel = sorted(by_line[ln])
        if i == 0:
            continue
        doomed += [t for _, t in (rel[:-1] if i <= keep else rel)]
    return sorted(doomed, key=lambda t: [int(p) for p in re.findall(r"\d+", t)])


def newest_lines(branches, count=3):
    """The `count` newest release lines among the branch names, newest first (release/X.Y only; any other name is ignored)."""
    lines = {}
    for b in branches:
        name = b.strip().removeprefix("refs/heads/")
        try:
            lines[parse_line(line_of(name))] = name
        except ValueError:
            continue
    return [lines[k] for k in sorted(lines, reverse=True)[:count]]


def main(argv):
    if not argv:
        print(__doc__, file=sys.stderr)
        return 2
    cmd, args = argv[0], argv[1:]
    try:
        if cmd == "line-of":
            print(line_of(args[0]))
        elif cmd == "next-rc":
            version, tag = next_rc(args[0], sys.stdin.read().split())
            print(f"version={version}")
            print(f"tag={tag}")
        elif cmd == "existing-rc":
            found = existing_rc(args[0], args[1:], sys.stdin.read().split())
            if found:
                print(f"version={found[0]}")
                print(f"tag={found[1]}")
        elif cmd == "is-latest":
            return 0 if is_latest(args[0], sys.stdin.read().split()) else 1
        elif cmd == "stale-rc":
            for t in stale_rc(sys.stdin.read().split()):
                print(t)
        elif cmd == "prune":
            keep, floor = 10, (0, 0)
            while args:
                opt = args.pop(0)
                if opt == "--keep":
                    keep = int(args.pop(0))
                elif opt == "--from":
                    floor = parse_line(args.pop(0))
                else:
                    raise ValueError(f"unknown option {opt}")
            for t in prune(sys.stdin.read().split(), keep, floor):
                print(t)
        elif cmd == "newest-lines":
            count = 3
            while args:
                opt = args.pop(0)
                if opt == "--count":
                    count = int(args.pop(0))
                else:
                    raise ValueError(f"unknown option {opt}")
            for b in newest_lines(sys.stdin.read().split(), count):
                print(b)
        else:
            raise ValueError(f"unknown command {cmd}")
    except (ValueError, IndexError) as e:
        print(f"error: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
