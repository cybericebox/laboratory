#!/usr/bin/env python3
"""The body of the GitHub API call that commits a backport. Standard library only; reads git, never writes it.

  backport_commit.py body --base OID --tree OID --repo OWNER/NAME --branch NAME --head OID --headline TEXT --message-file FILE
                          [--max-bytes N] [--max-files N]

Why an API commit: the branches of this repository require signed commits, and a commit made with git on a runner is not signed. A commit
made with the GraphQL mutation createCommitOnBranch is signed by GitHub and shown as verified (GitHub documentation, "Commit Signing" of
the mutation); it is authored by the token's owner (github-actions[bot]) and takes no author or committer.
The change is the difference between two trees (BASE, the head of develop, and TREE, the result of the cherry-picks made in a scratch clone);
it is sent as FileAdditions (the whole new content, base64: text and binary alike) and FileDeletions. That API cannot say:
  - a file mode: an executable file, a mode change, a symlink or a submodule cannot be expressed (an addition is always a plain file);
  - a rename: it is sent as a deletion and an addition (the content is the same, only the history differs).
So a change that touches a mode other than 100644, before or after, is refused ("unsupported"), as is a change above --max-bytes of
content or --max-files files (the request would be too large for one call). The caller then opens an issue instead of a pull request.
Exit 0: the GraphQL request body (JSON) is on stdout. Exit 3: unsupported or too large, the reason on stderr. Exit 4: no difference.
"""
import argparse
import base64
import json
import subprocess
import sys

PLAIN = "100644"
MUTATION = (
    "mutation($input: CreateCommitOnBranchInput!) { createCommitOnBranch(input: $input) { commit { oid url } } }"
)
MAX_BYTES = 10 * 1024 * 1024
MAX_FILES = 300


def git(*args, binary=False):
    p = subprocess.run(["git", *args], capture_output=True, check=True)
    return p.stdout if binary else p.stdout.decode("utf-8", "surrogateescape")


def entries(base, tree):
    """[(src_mode, dst_mode, status, path)] of the difference, renames shown as a deletion and an addition."""
    raw = git("diff-tree", "-r", "-z", "--no-renames", "--raw", base, tree)
    parts = raw.split("\0")
    out = []
    i = 0
    while i + 1 < len(parts) and parts[i].startswith(":"):
        meta = parts[i][1:].split()
        out.append((meta[0], meta[1], meta[4][0], parts[i + 1]))
        i += 2
    return out


class Unsupported(Exception):
    pass


def plan(items, read_blob, max_bytes=MAX_BYTES, max_files=MAX_FILES, size_of=None):
    """{"additions": [...], "deletions": [...]} or Unsupported. read_blob(path) -> bytes of the new content."""
    if len(items) > max_files:
        raise Unsupported(f"{len(items)} files changed, more than the {max_files} one API commit takes")
    bad = []
    for src, dst, status, path in items:
        try:
            path.encode("utf-8")
        except UnicodeEncodeError:
            bad.append(f"{path.encode('utf-8', 'backslashreplace').decode('ascii')}: the name is not valid UTF-8, which the API cannot carry")
            continue
        if path.startswith(".github/workflows/"):
            bad.append(f"{path}: a workflow file cannot be changed with the job token (no workflows permission)")
            continue
        if status == "A" and dst != PLAIN:
            bad.append(f"{path}: new file with mode {dst}")
        elif status == "M" and (src != PLAIN or dst != PLAIN):
            bad.append(f"{path}: mode {src} -> {dst}")
        elif status == "D" and src != PLAIN:
            bad.append(f"{path}: deleted file of mode {src}")
        elif status not in "AMD":
            bad.append(f"{path}: change of kind {status}")
    if bad:
        raise Unsupported(
            "the GitHub commit API cannot express file modes, symlinks or submodules (an addition is always a plain file): " + "; ".join(bad[:5])
        )
    additions, deletions, total = [], [], 0
    for src, dst, status, path in items:
        if status == "D":
            deletions.append({"path": path})
            continue
        data = read_blob(path)
        total += len(data)
        if total > max_bytes:
            raise Unsupported(f"more than {max_bytes // (1024 * 1024)} MiB of changed content: too large for one API commit")
        additions.append({"path": path, "contents": base64.b64encode(data).decode("ascii")})
    return {"additions": additions, "deletions": deletions}


def body(plan_, repo, branch, head, headline, message):
    return {
        "query": MUTATION,
        "variables": {
            "input": {
                "branch": {"repositoryNameWithOwner": repo, "branchName": branch},
                "expectedHeadOid": head,
                "message": {"headline": headline, "body": message},
                "fileChanges": plan_,
            }
        },
    }


def main(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["body"])
    ap.add_argument("--base", required=True)
    ap.add_argument("--tree", required=True)
    ap.add_argument("--repo", required=True)
    ap.add_argument("--branch", required=True)
    ap.add_argument("--head", required=True)
    ap.add_argument("--headline", required=True)
    ap.add_argument("--message-file", required=True)
    ap.add_argument("--max-bytes", type=int, default=MAX_BYTES)
    ap.add_argument("--max-files", type=int, default=MAX_FILES)
    a = ap.parse_args(argv)
    items = entries(a.base, a.tree)
    if not items:
        print("no difference between the trees", file=sys.stderr)
        return 4
    try:
        p = plan(items, lambda path: git("cat-file", "blob", f"{a.tree}:{path}", binary=True), a.max_bytes, a.max_files)
    except Unsupported as e:
        print(str(e), file=sys.stderr)
        return 3
    with open(a.message_file, encoding="utf-8") as f:
        message = f.read()
    json.dump(body(p, a.repo, a.branch, a.head, a.headline, message), sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
