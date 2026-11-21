import unittest

import release_gate as rg

SHA = "a" * 40
HEAD = "b" * 40
REPO = "o/r"


def world(**over):
    """A GitHub that has everything a good promote needs; a test breaks one thing."""
    w = {
        "tags": ["v1.10.0-rc.1", "v1.10.0-rc.2"],
        "commit": {"v1.10.0-rc.2": SHA},
        "compare": {"status": "ahead"},
        "pulls": [{"number": 7, "merged_at": "t", "merge_commit_sha": SHA, "base": {"ref": "release/1.10"}, "head": {"sha": HEAD}, "labels": []}],
        "checks": [{"id": 1, "name": "Check passed", "conclusion": "success", "app": {"slug": "github-actions"}}],
        "files": [{"filename": "a.go", "additions": 5, "deletions": 1}],
        "changed_files": 1,
    }
    w.update(over)
    return w


def api_of(w):
    def api(path, paginate=False):
        if w.get("fail") and w["fail"] in path:
            raise rg.GateError(f"boom {path}")
        if path.startswith(f"repos/{REPO}/tags"):
            return [{"name": t} for t in w["tags"]]
        if path.startswith(f"repos/{REPO}/commits/") and path.endswith("/pulls"):
            return w["pulls"]
        if "/check-runs" in path:
            return w["checks"]
        if path.startswith(f"repos/{REPO}/git/ref/tags/"):
            name = path.rsplit("/", 1)[1]
            if name in w.get("annotated", {}):
                return {"object": {"type": "tag", "sha": "t" * 40}}
            return {"object": {"type": "commit", "sha": w["commit"][name]}}
        if path == f"repos/{REPO}/git/tags/{'t' * 40}":
            return {"object": {"type": "commit", "sha": w["annotated"]["v1.10.0-rc.2"]}}
        if path.startswith(f"repos/{REPO}/commits/") and "/" not in path[len(f"repos/{REPO}/commits/"):]:
            raise AssertionError("a tag must not be resolved through commits/NAME (a branch of that name would win): " + path)
        if path.startswith(f"repos/{REPO}/compare/"):
            return w["compare"]
        if path.endswith("/files?per_page=100"):
            return w["files"]
        if path.startswith(f"repos/{REPO}/pulls/"):
            return {"changed_files": w["changed_files"]}
        raise AssertionError(path)

    return api


def verify(w, rc="v1.10.0-rc.2"):
    return rg.verify_candidate(rc, REPO, api_of(w))


class VerifyTests(unittest.TestCase):
    def refused(self, w, text, rc="v1.10.0-rc.2"):
        with self.assertRaises(rg.GateError) as cm:
            verify(w, rc)
        self.assertIn(text, str(cm.exception))

    def test_good_candidate(self):
        self.assertEqual(verify(world()), {"sha": SHA, "kind": "line", "pr": 7})

    def test_not_a_candidate_tag(self):
        for rc in ["v1.10.0", "main", "v1.10-rc.1", "", "v1.10.0-rc.x"]:
            self.refused(world(), "not a candidate tag", rc)

    def test_missing_tag(self):
        self.refused(world(tags=["v1.10.0-rc.1"]), "does not exist")

    def test_older_candidate_is_refused(self):
        w = world(tags=["v1.10.0-rc.1", "v1.10.0-rc.2", "v1.10.0-rc.3"])
        self.refused(w, "not the newest candidate", "v1.10.0-rc.2")

    def test_final_tag_on_another_commit_is_refused(self):
        w = world(tags=["v1.10.0-rc.2", "v1.10.0"])
        w["commit"]["v1.10.0"] = "c" * 40
        self.refused(w, "already exists")

    def test_final_tag_on_the_same_commit_is_a_rerun(self):
        w = world(tags=["v1.10.0-rc.2", "v1.10.0"])
        w["commit"]["v1.10.0"] = SHA
        self.assertEqual(verify(w)["sha"], SHA)

    def test_commit_not_on_the_line(self):
        for status in ["diverged", "behind", None]:
            self.refused(world(compare={"status": status}), "not on release/1.10")

    def test_commit_that_is_the_branch_head_is_on_the_line(self):
        self.assertEqual(verify(world(compare={"status": "identical"}))["sha"], SHA)

    def test_no_pull_request(self):
        self.refused(world(pulls=[]), "merged pull request")

    def test_unmerged_pull_request(self):
        w = world()
        w["pulls"][0]["merged_at"] = None
        self.refused(w, "merged pull request")

    def test_pull_request_whose_merge_commit_is_another(self):
        w = world()
        w["pulls"][0]["merge_commit_sha"] = "d" * 40
        self.refused(w, "merged pull request")

    def test_pull_request_into_another_branch(self):
        w = world()
        w["pulls"][0]["base"]["ref"] = "main"
        self.refused(w, "merged pull request")

    def test_first_candidate_may_be_a_develop_commit(self):
        w = world(tags=["v1.10.0-rc.1"], commit={"v1.10.0-rc.1": SHA})
        w["pulls"][0]["base"]["ref"] = "develop"
        self.assertEqual(verify(w, "v1.10.0-rc.1"), {"sha": SHA, "kind": "cut", "pr": 7})

    def test_a_patch_candidate_may_not_be_a_develop_commit(self):
        w = world(tags=["v1.10.0", "v1.10.1-rc.1"], commit={"v1.10.1-rc.1": SHA, "v1.10.0": "c" * 40})
        w["pulls"][0]["base"]["ref"] = "develop"
        self.refused(w, "merged pull request", "v1.10.1-rc.1")

    def test_required_check_missing(self):
        self.refused(world(checks=[]), "no check")

    def test_required_check_from_another_app(self):
        w = world()
        w["checks"][0]["app"] = {"slug": "evil"}
        self.refused(w, "no check")

    def test_required_check_failed_or_running(self):
        for c in ["failure", "cancelled", "skipped", None]:
            w = world()
            w["checks"][0]["conclusion"] = c
            self.refused(w, "not success")

    def test_the_newest_run_of_a_check_counts(self):
        w = world(checks=[
            {"id": 1, "name": "Check passed", "conclusion": "failure", "app": {"slug": "github-actions"}},
            {"id": 2, "name": "Check passed", "conclusion": "success", "app": {"slug": "github-actions"}},
        ])
        self.assertEqual(verify(w)["sha"], SHA)
        w["checks"][1]["conclusion"] = "failure"
        self.refused(w, "not success")

    def test_gate_is_evaluated_again(self):
        self.refused(world(files=[{"filename": "a.go", "additions": 900, "deletions": 0}]), "line gate")
        self.refused(world(files=[{"filename": ".github/workflows/x.yml", "additions": 1, "deletions": 0}]), "line gate")

    def test_override_label_is_honoured(self):
        w = world(files=[{"filename": "a.go", "additions": 900, "deletions": 0}])
        w["pulls"][0]["labels"] = [{"name": "large-fix"}]
        self.assertEqual(verify(w)["kind"], "line")

    def test_incomplete_file_list_fails_closed(self):
        self.refused(world(changed_files=3), "line gate")

    def test_a_failed_lookup_fails_closed(self):
        for part in ["/tags", "/compare/", "/check-runs", "/files", "/pulls"]:
            with self.assertRaises(rg.GateError, msg=part):
                verify(world(fail=part))

    def test_cut_pull_request_needs_no_gate(self):
        w = world(tags=["v1.10.0-rc.1"], commit={"v1.10.0-rc.1": SHA}, files=[{"filename": ".github/x", "additions": 9999, "deletions": 0}])
        w["pulls"][0]["base"]["ref"] = "develop"
        self.assertEqual(verify(w, "v1.10.0-rc.1")["kind"], "cut")


class HardeningTests(unittest.TestCase):
    def refused(self, w, text, rc="v1.10.0-rc.2"):
        with self.assertRaises(rg.GateError) as cm:
            verify(w, rc)
        self.assertIn(text, str(cm.exception))

    def test_unicode_digits_and_a_trailing_newline_are_not_a_candidate(self):
        for rc in ["v1.10.0-rc.\u0662", "v\u0661.10.0-rc.2", "v1.10.0-rc.2\n"]:
            self.refused(world(), "not a candidate tag", rc)

    def test_a_patch_is_not_skipped(self):
        w = world(tags=["v1.10.0", "v1.10.9-rc.1"], commit={"v1.10.9-rc.1": SHA, "v1.10.0": "c" * 40})
        self.refused(w, "next release of line 1.10 is v1.10.1", "v1.10.9-rc.1")

    def test_a_candidate_of_an_older_patch_is_refused(self):
        w = world(tags=["v1.10.0", "v1.10.1", "v1.10.1-rc.1", "v1.10.0-rc.5"], commit={"v1.10.0-rc.5": SHA, "v1.10.0": "c" * 40})
        # v1.10.0 is final already, at another commit
        self.refused(w, "already exists", "v1.10.0-rc.5")

    def test_the_first_candidate_of_a_line_must_be_patch_zero(self):
        self.refused(world(tags=["v1.10.1-rc.1"], commit={"v1.10.1-rc.1": SHA}), "next release of line 1.10 is v1.10.0", "v1.10.1-rc.1")

    def test_an_annotated_tag_is_peeled_to_its_commit(self):
        w = world(annotated={"v1.10.0-rc.2": SHA})
        self.assertEqual(verify(w)["sha"], SHA)

    def test_the_tag_is_read_from_the_tag_ref_not_from_commits(self):
        # api_of raises when commits/NAME is asked (a branch of the same name would answer instead of the tag)
        self.assertEqual(verify(world())["sha"], SHA)


if __name__ == "__main__":
    unittest.main()
