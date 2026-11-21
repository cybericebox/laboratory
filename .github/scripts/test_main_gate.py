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
    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.old = os.getcwd()
        os.chdir(self.d.name)
        env = {"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null", "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t",
               "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"}
        os.environ.update(env)
        self.git("init", "-q", "-b", "main")
        self.git("commit", "-q", "--allow-empty", "-m", "a")
        self.a = self.git("rev-parse", "HEAD")
        self.git("commit", "-q", "--allow-empty", "-m", "b")
        self.b = self.git("rev-parse", "HEAD")

    def tearDown(self):
        os.chdir(self.old)
        self.d.cleanup()

    def git(self, *a):
        return subprocess.run(["git", *a], capture_output=True, text=True, check=True).stdout.strip()

    def test_head_is_the_tagged_commit(self):
        self.git("tag", "v1.10.0", self.b)
        self.assertEqual(mg.problems("promote/v1.10.0", self.b), [])

    def test_head_contains_the_tagged_commit(self):
        self.git("tag", "v1.10.0", self.a)
        self.assertEqual(mg.problems("promote/v1.10.0", self.b), [])

    def test_head_that_does_not_contain_it_is_refused(self):
        self.git("tag", "v1.10.0", self.b)
        self.assertEqual(len(mg.problems("promote/v1.10.0", self.a)), 1)

    def test_no_tag_is_refused(self):
        self.assertEqual(len(mg.problems("promote/v1.10.0", self.b)), 1)

    def test_a_candidate_tag_is_not_a_release(self):
        self.git("tag", "v1.10.0-rc.1", self.b)
        self.assertEqual(len(mg.problems("promote/v1.10.0", self.b)), 1)

    def test_a_branch_that_is_not_promote_is_refused(self):
        self.git("tag", "v1.10.0", self.b)
        self.assertEqual(len(mg.problems("feature/x", self.b)), 1)

    def test_cli_exit_codes(self):
        self.git("tag", "v1.10.0", self.b)
        self.assertEqual(mg.main(["check", "promote/v1.10.0", self.b]), 0)
        self.assertEqual(mg.main(["check", "promote/v1.11.0", self.b]), 1)
        self.assertEqual(mg.main([]), 2)


if __name__ == "__main__":
    unittest.main()
