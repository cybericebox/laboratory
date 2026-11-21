import base64
import io
import json
import os
import subprocess
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout

import backport_commit as bc


def git(*a, **kw):
    return subprocess.run(["git", *a], capture_output=True, text=True, check=True, **kw).stdout.strip()


class TreeTests(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.TemporaryDirectory()
        self.old = os.getcwd()
        os.chdir(self.d.name)
        os.environ.update({"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null", "GIT_AUTHOR_NAME": "t",
                           "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"})
        git("init", "-q", "-b", "main")
        for name, text in [("keep.txt", "keep\n"), ("edit.txt", "one\n"), ("gone.txt", "bye\n"), ("old name.txt", "same content\n")]:
            with open(name, "w", encoding="utf-8") as f:
                f.write(text)
        os.mkdir("dir")
        with open("dir/x.sh", "w", encoding="utf-8") as f:
            f.write("#!/bin/sh\n")
        os.chmod("dir/x.sh", 0o755)
        git("add", "-A")
        git("commit", "-q", "-m", "base")
        self.base = git("rev-parse", "HEAD")

    def tearDown(self):
        os.chdir(self.old)
        self.d.cleanup()

    def commit(self):
        git("add", "-A")
        git("commit", "-q", "-m", "change")
        return git("rev-parse", "HEAD^{tree}")

    def run_cli(self, tree, **kw):
        with open("msg.txt", "w", encoding="utf-8") as f:
            f.write("cherry picked from 1234567\n")
        argv = ["body", "--base", self.base, "--tree", tree, "--repo", "o/r", "--branch", "backport/1.10/abc", "--head", self.base,
                "--headline", "Backport: x", "--message-file", "msg.txt"]
        for k, v in kw.items():
            argv += [f"--{k.replace('_', '-')}", str(v)]
        out, err = io.StringIO(), io.StringIO()
        with redirect_stdout(out), redirect_stderr(err):
            rc = bc.main(argv)
        return rc, out.getvalue(), err.getvalue()

    def test_add_modify_delete_binary_and_unicode_names(self):
        with open("edit.txt", "w", encoding="utf-8") as f:
            f.write("one\ntwo\n")
        os.remove("gone.txt")
        with open("new.txt", "w", encoding="utf-8") as f:
            f.write("new\n")
        with open("blob.bin", "wb") as f:
            f.write(bytes(range(256)))
        with open("файл ü.txt", "w", encoding="utf-8") as f:
            f.write("u\n")
        rc, out, err = self.run_cli(self.commit())
        self.assertEqual((rc, err), (0, ""))
        req = json.loads(out)
        fc = req["variables"]["input"]["fileChanges"]
        self.assertEqual(fc["deletions"], [{"path": "gone.txt"}])
        adds = {a["path"]: base64.b64decode(a["contents"]) for a in fc["additions"]}
        self.assertEqual(sorted(adds), ["blob.bin", "edit.txt", "new.txt", "файл ü.txt"])
        self.assertEqual(adds["blob.bin"], bytes(range(256)))
        self.assertEqual(adds["edit.txt"], b"one\ntwo\n")
        inp = req["variables"]["input"]
        self.assertEqual((inp["expectedHeadOid"], inp["branch"]["branchName"], inp["message"]["headline"]),
                         (self.base, "backport/1.10/abc", "Backport: x"))
        self.assertIn("createCommitOnBranch", req["query"])

    def test_a_rename_is_a_deletion_and_an_addition(self):
        os.rename("old name.txt", "new name.txt")
        _, out, _ = self.run_cli(self.commit())
        fc = json.loads(out)["variables"]["input"]["fileChanges"]
        self.assertEqual(fc["deletions"], [{"path": "old name.txt"}])
        self.assertEqual([a["path"] for a in fc["additions"]], ["new name.txt"])

    def test_no_difference(self):
        git("commit", "-q", "--allow-empty", "-m", "empty")
        self.assertEqual(self.run_cli(git("rev-parse", "HEAD^{tree}"))[0], 4)

    def test_executable_bit_change_is_unsupported(self):
        os.chmod("keep.txt", 0o755)
        rc, out, err = self.run_cli(self.commit())
        self.assertEqual((rc, out), (3, ""))
        self.assertIn("keep.txt", err)
        self.assertIn("mode", err)

    def test_content_change_of_an_executable_is_unsupported(self):
        with open("dir/x.sh", "a", encoding="utf-8") as f:
            f.write("echo hi\n")
        self.assertEqual(self.run_cli(self.commit())[0], 3)

    def test_deleting_an_executable_is_unsupported(self):
        os.remove("dir/x.sh")
        self.assertEqual(self.run_cli(self.commit())[0], 3)

    def test_new_executable_symlink_and_submodule_are_unsupported(self):
        with open("run.sh", "w", encoding="utf-8") as f:
            f.write("x\n")
        os.chmod("run.sh", 0o755)
        self.assertEqual(self.run_cli(self.commit())[0], 3)
        git("reset", "-q", "--hard", self.base)
        os.symlink("keep.txt", "link")
        rc, _, err = self.run_cli(self.commit())
        self.assertEqual(rc, 3)
        self.assertIn("120000", err)
        git("reset", "-q", "--hard", self.base)
        git("update-index", "--add", "--cacheinfo", f"160000,{self.base},sub")
        git("commit", "-q", "-m", "sub")
        self.assertEqual(self.run_cli(git("rev-parse", "HEAD^{tree}"))[0], 3)

    def test_too_large_is_unsupported(self):
        with open("big.bin", "wb") as f:
            f.write(b"x" * 2048)
        tree = self.commit()
        self.assertEqual(self.run_cli(tree, max_bytes=1024)[0], 3)
        self.assertEqual(self.run_cli(tree, max_bytes=4096)[0], 0)

    def test_too_many_files_is_unsupported(self):
        for i in range(5):
            with open(f"f{i}.txt", "w", encoding="utf-8") as f:
                f.write(str(i))
        tree = self.commit()
        self.assertEqual(self.run_cli(tree, max_files=4)[0], 3)
        self.assertEqual(self.run_cli(tree, max_files=5)[0], 0)

    def test_deleting_everything_but_plain_files_has_no_additions(self):
        os.remove("gone.txt")
        _, out, _ = self.run_cli(self.commit())
        fc = json.loads(out)["variables"]["input"]["fileChanges"]
        self.assertEqual((fc["additions"], fc["deletions"]), ([], [{"path": "gone.txt"}]))


class PlanTests(unittest.TestCase):
    def test_modes(self):
        ok = bc.plan([("100644", "100644", "M", "a")], lambda p: b"x")
        self.assertEqual(ok["additions"][0]["path"], "a")
        for items in [[("100755", "100755", "M", "a")], [("000000", "100755", "A", "a")], [("100644", "120000", "T", "a")]]:
            with self.assertRaises(bc.Unsupported):
                bc.plan(items, lambda p: b"x")


class PathTests(unittest.TestCase):
    def blob(self, path):
        return b"x"

    def test_a_name_that_is_not_utf8_is_refused_not_sent_broken(self):
        with self.assertRaises(bc.Unsupported) as cm:
            bc.plan([("000000", "100644", "A", "bad\udcff.txt")], self.blob)
        self.assertIn("not valid UTF-8", str(cm.exception))

    def test_a_workflow_file_is_refused(self):
        with self.assertRaises(bc.Unsupported) as cm:
            bc.plan([("100644", "100644", "M", ".github/workflows/x.yml")], self.blob)
        self.assertIn("workflow", str(cm.exception))

    def test_an_ordinary_name_with_odd_characters_is_carried_as_data(self):
        p = bc.plan([("000000", "100644", "A", "a b\nc.txt")], self.blob)
        self.assertEqual(p["additions"][0]["path"], "a b\nc.txt")


if __name__ == "__main__":
    unittest.main()
