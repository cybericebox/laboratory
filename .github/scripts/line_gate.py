#!/usr/bin/env python3
"""The gate of a pull request into a release line (release/X.Y). Standard library only.

  line_gate.py check --files FILE.json --changed-files N [--labels a,b] [--max-lines N]

FILE.json is the answer of GET /repos/{owner}/{repo}/pulls/{n}/files (a list with filename, previous_filename, additions, deletions),
N is changed_files of GET /repos/{owner}/{repo}/pulls/{n}. The files list is paginated and the API lists 3000 files at most: when the
list is not exactly N long, or has reached that cap, the gate fails (it cannot see the whole change).
Exit 0 when the change may enter the line, 1 with a message that says the rule and how to fix it.

The rule (no title convention):
  1. the harness (workflows, scripts, tool versions, test and build scripts) is not changed through a line: a pull request that edits
     the checks it must pass could weaken them. No override: the change goes into develop;
  2. the change is at most MAX_CHANGED_LINES changed lines (added + deleted), not counting generated files and lock files. The label
     OVERRIDE_LABEL on the pull request waives this limit (only people with write access can set a label).
"""
import argparse
import json
import re
import sys

MAX_CHANGED_LINES = 400
OVERRIDE_LABEL = "large-fix"

# Not counted: files a tool writes. Each pattern is anchored to the place where the tool writes (a file anywhere else, named like a
# generated one, is counted: the name is the attacker's to choose, so a name alone never lowers the count).
UNCOUNTED = re.compile(
    r"^(go\.sum|package-lock\.json|pnpm-lock\.yaml|yarn\.lock|uv\.lock|mise\.lock)$"
    r"|^api/[^/]+/[^/]+/zz_generated\.deepcopy\.go$"
    r"|^pkg/(agent/protobuf|rpc/[^/]+/[^/]+)/[^/]*\.pb\.go$"
    r"|^clientset/"
    r"|^config/crd/bases/"
    r"|^config/rbac/role\.yaml$"
    r"|^charts/laboratory/crds/"
    r"|^internal/proxy/demux/fastpath/[^/]*\.(o|elf)$"
    r"|^internal/proxy/demux/fastpath/[^/]*_bpfe[lb]\.go$"
)

# What each CI job runs. A line takes no change here (develop takes it). Case-insensitive on purpose.
#   .github/**                 the workflows and every script they run, CODEOWNERS
#   mise config                mise.toml, .mise.toml, mise.<env>.toml, mise.lock, .tool-versions, and the directories mise reads at any depth
#                              (mise/, .mise/, .config/mise/, mise-tasks/, .mise-tasks/): a second config file overrides tools and tasks
#   hack/**, scripts/**        the scripts the tasks run, dev.sh and its test
#   golangci-lint config       .golangci.{yml,yaml,toml,json}
# Not protected: the Go sources and their tests, the Dockerfile and the chart: they are the product the checks judge.
# (This is a guard against accidents and weakening of the checks, not a sandbox: a pull request into a line still runs its own Go tests.)
PROTECTED = re.compile(
    r"^\.github/"
    r"|^(hack|scripts)/"
    r"|(^|/)\.?mise(-tasks)?/"
    r"|(^|/)\.?mise(\.[a-z0-9_-]+)?\.(toml|lock)$"
    r"|(^|/)\.tool-versions$"
    r"|(^|/)\.golangci\.(ya?ml|toml|json)$",
    re.IGNORECASE,
)

FILES_CAP = 3000


def changed_lines(files):
    return sum(f.get("additions", 0) + f.get("deletions", 0) for f in files if not UNCOUNTED.search(f.get("filename", "")))


def protected_files(files):
    """Both names of a file count: a rename moves a protected file away under a new name (previous_filename)."""
    out = set()
    for f in files:
        for name in (f.get("filename", ""), f.get("previous_filename", "") or ""):
            if PROTECTED.search(name):
                out.add(name)
    return sorted(out)


def complete(files, changed_files):
    """The list must be exactly the change: as long as changed_files of the pull request, and below the cap of the API."""
    return isinstance(changed_files, int) and changed_files >= 0 and len(files) == changed_files and len(files) < FILES_CAP


def problems(files, changed_files, labels=(), max_lines=MAX_CHANGED_LINES):
    if not complete(files, changed_files):
        return [
            f"Cannot see the whole change: the API listed {len(files)} of {changed_files} files (it lists {FILES_CAP} at most). "
            "A line takes small fixes: split the pull request."
        ]
    out = []
    prot = protected_files(files)
    if prot:
        out.append(
            "A release line does not take changes to CI or the build tooling (.github, mise config, hack, scripts, golangci-lint config): "
            f"{', '.join(json.dumps(p) for p in prot[:5])}. Make the change in develop; if the line's own CI is broken, the owner merges it with the ruleset bypass."
        )
    n = changed_lines(files)
    if n > max_lines and OVERRIDE_LABEL not in labels:
        out.append(
            f"The change is {n} changed lines, above the limit of {max_lines} for a release line (generated files and go.sum not counted). "
            f"Split it, or, for a justified larger fix, the owner adds the label {OVERRIDE_LABEL!r} to the pull request."
        )
    return out


def main(argv):
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("check")
    c.add_argument("--files", required=True)
    c.add_argument("--changed-files", type=int, required=True)
    c.add_argument("--labels", default="")
    c.add_argument("--max-lines", type=int, default=MAX_CHANGED_LINES)
    a = ap.parse_args(argv)
    with open(a.files, encoding="utf-8") as fh:
        files = json.load(fh)
    labels = [x for x in a.labels.split(",") if x]
    found = problems(files, a.changed_files, labels, a.max_lines)
    for p in found:
        print(f"line-gate: {p}", file=sys.stderr)
    if not found:
        waived = " (size limit waived by label)" if changed_lines(files) > a.max_lines else ""
        print(f"line-gate: ok ({changed_lines(files)} changed lines, limit {a.max_lines}){waived}")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
