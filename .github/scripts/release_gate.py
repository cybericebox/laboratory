#!/usr/bin/env python3
"""The preconditions of a promote. Standard library only; talks to GitHub through the `gh` CLI.

  release_gate.py verify-candidate --repo OWNER/NAME --rc vX.Y.Z-rc.N

Prints the full SHA of the candidate on stdout when it may be promoted, and the reasons on stderr; exit 1 otherwise. It fails closed: any
lookup that fails, or an answer that is incomplete, is a refusal, never a pass.

A candidate may be promoted when:
  1. the tag is vX.Y.Z-rc.N, it is the newest candidate of vX.Y.Z, and no different commit already carries vX.Y.Z;
  2. its commit is on the branch release/X.Y;
  3. its commit is the result of a merged pull request into release/X.Y (the merge commit of that pull request), or, for the very first
     candidate of a line (vX.Y.0, cut from develop), the result of a merged pull request into develop;
  4. every required check (REQUIRED_CHECKS, made by GitHub Actions) of the head of that pull request succeeded;
  5. for a pull request into the line, the line gate (line_gate.py) passes again, evaluated here from the trusted copy of the script.
There is no review requirement: the maintainer works alone and cannot approve their own pull requests.
"""
import argparse
import json
import re
import subprocess
import sys

import line_gate
import release_version

REQUIRED_CHECKS = ("Check passed",)
RC = re.compile(r"v(\d+)\.(\d+)\.(\d+)-rc\.(\d+)", re.ASCII)


class GateError(Exception):
    pass


def gh_api(path, paginate=False):
    """The one place that touches the network. A list endpoint is read in full (--paginate --slurp) and flattened."""
    cmd = ["gh", "api", path]
    if paginate:
        cmd = ["gh", "api", "--paginate", "--slurp", path]
    p = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if p.returncode != 0:
        raise GateError(f"gh api {path} failed: {(p.stdout + p.stderr).strip()[:300]}")
    try:
        data = json.loads(p.stdout)
    except ValueError as e:
        raise GateError(f"gh api {path}: not JSON ({e})") from e
    if paginate:
        out = []
        for page in data:
            out += page if isinstance(page, list) else next((v for v in page.values() if isinstance(v, list)), [])
        return out
    return data


def tag_commit(api, repo, name):
    """The commit a tag points at, read from the tag ref itself (git/ref/tags/NAME, annotated tags peeled). Not repos/.../commits/NAME: a
    branch with the same name as the tag would win there, and anybody who can push a branch could then name the commit."""
    obj = api(f"repos/{repo}/git/ref/tags/{name}")["object"]
    for _ in range(5):
        if obj["type"] == "commit":
            return obj["sha"]
        if obj["type"] != "tag":
            break
        obj = api(f"repos/{repo}/git/tags/{obj['sha']}")["object"]
    raise GateError(f"tag {name} does not point at a commit")


def verify_candidate(rc, repo, api=gh_api, required=REQUIRED_CHECKS, max_lines=line_gate.MAX_CHANGED_LINES):
    """Returns {"sha", "kind" ("line" or "cut"), "pr"}; raises GateError with the reason otherwise. `api(path, paginate=False)` is injected."""
    m = RC.fullmatch(rc or "")
    if not m:
        raise GateError(f"{rc!r} is not a candidate tag (vX.Y.Z-rc.N)")
    x, y, z, n = (int(g) for g in m.groups())
    line, version, branch = f"{x}.{y}", f"v{x}.{y}.{z}", f"release/{x}.{y}"

    tags = [t["name"] for t in api(f"repos/{repo}/tags?per_page=100", True)]
    if rc not in tags:
        raise GateError(f"tag {rc} does not exist")
    newer = [c for c in release_version.candidates(tags) if c[:3] == (x, y, z) and c[3] > n]
    if newer:
        raise GateError(f"{rc} is not the newest candidate of {version}: v{x}.{y}.{z}-rc.{max(c[3] for c in newer)} exists")
    expected = release_version.next_rc(line, [t for t in tags if t != rc])[0]
    if version != expected and version not in tags:
        raise GateError(f"{rc}: the next release of line {line} is {expected}, not {version} (a patch is never skipped or taken out of order)")
    sha = tag_commit(api, repo, rc)
    if version in tags:
        final = tag_commit(api, repo, version)
        if final != sha:
            raise GateError(f"{version} already exists at {final[:7]}, not at the commit of {rc} ({sha[:7]})")

    cmp = api(f"repos/{repo}/compare/{sha}...{branch}")
    if cmp.get("status") not in ("identical", "ahead"):
        raise GateError(f"the commit of {rc} ({sha[:7]}) is not on {branch} (compare says {cmp.get('status')!r})")

    prs = [p for p in api(f"repos/{repo}/commits/{sha}/pulls", True) if p.get("merged_at") and p.get("merge_commit_sha") == sha]
    into_line = [p for p in prs if p["base"]["ref"] == branch]
    into_develop = [p for p in prs if p["base"]["ref"] == "develop"]
    if into_line:
        kind, pr = "line", into_line[0]
    elif z == 0 and into_develop:
        kind, pr = "cut", into_develop[0]
    else:
        raise GateError(
            f"the commit of {rc} ({sha[:7]}) is not the result of a merged pull request into {branch}"
            + (" (nor, for the first candidate of a line, into develop)" if z == 0 else "")
            + ": a candidate must come through a pull request"
        )

    head = pr["head"]["sha"]
    runs = api(f"repos/{repo}/commits/{head}/check-runs?per_page=100", True)
    for name in required:
        mine = [r for r in runs if r.get("name") == name and (r.get("app") or {}).get("slug") == "github-actions"]
        if not mine:
            raise GateError(f"pull request #{pr['number']}: no check {name!r} by GitHub Actions on its head {head[:7]}")
        last = max(mine, key=lambda r: r.get("id", 0))
        if last.get("conclusion") != "success":
            raise GateError(f"pull request #{pr['number']}: check {name!r} is {last.get('conclusion') or last.get('status')!r}, not success")

    if kind == "line":
        number = pr["number"]
        files = api(f"repos/{repo}/pulls/{number}/files?per_page=100", True)
        changed = api(f"repos/{repo}/pulls/{number}")["changed_files"]
        labels = [lab["name"] for lab in pr.get("labels", [])]
        found = line_gate.problems(files, changed, labels, max_lines)
        if found:
            raise GateError(f"pull request #{number} would not pass the line gate now: " + " ".join(found))
    return {"sha": sha, "kind": kind, "pr": pr["number"]}


def main(argv):
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    v = sub.add_parser("verify-candidate")
    v.add_argument("--repo", required=True)
    v.add_argument("--rc", required=True)
    a = ap.parse_args(argv)
    try:
        r = verify_candidate(a.rc, a.repo)
    except (GateError, KeyError, TypeError) as e:
        print(f"release-gate: refused: {e}", file=sys.stderr)
        return 1
    print(f"release-gate: {a.rc} ok ({r['kind']} pull request #{r['pr']})", file=sys.stderr)
    print(r["sha"])
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
