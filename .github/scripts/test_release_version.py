import io
import unittest
from contextlib import redirect_stdout
from unittest import mock

import release_version as rv


class LineTests(unittest.TestCase):
    def test_line_of(self):
        self.assertEqual(rv.line_of("release/1.10"), "1.10")
        self.assertEqual(rv.line_of("release/01.010"), "1.10")

    def test_line_of_refuses_other_branches(self):
        for b in ["develop", "main", "release/1", "release/1.10.1", "release/1.x", "feature/release/1.10", "release/1.10/x", ""]:
            with self.assertRaises(ValueError, msg=b):
                rv.line_of(b)

    def test_parse_line(self):
        self.assertEqual(rv.parse_line("1.10"), (1, 10))
        for v in ["1", "1.10.0", "v1.10", "a.b"]:
            with self.assertRaises(ValueError, msg=v):
                rv.parse_line(v)


class NextRcTests(unittest.TestCase):
    def test_first_candidate_of_a_new_line(self):
        self.assertEqual(rv.next_rc("1.10", ["v1.9.0", "v1.9.1"]), ("v1.10.0", "v1.10.0-rc.1"))

    def test_next_candidate_of_the_same_version(self):
        self.assertEqual(rv.next_rc("1.10", ["v1.10.0-rc.1", "v1.10.0-rc.2"]), ("v1.10.0", "v1.10.0-rc.3"))

    def test_after_the_release_the_next_patch_starts(self):
        tags = ["v1.10.0-rc.1", "v1.10.0-rc.2", "v1.10.0"]
        self.assertEqual(rv.next_rc("1.10", tags), ("v1.10.1", "v1.10.1-rc.1"))

    def test_patch_candidates_continue(self):
        tags = ["v1.10.0", "v1.10.1-rc.1", "v1.10.1-rc.2"]
        self.assertEqual(rv.next_rc("1.10", tags), ("v1.10.1", "v1.10.1-rc.3"))

    def test_gaps_and_order_do_not_matter(self):
        self.assertEqual(rv.next_rc("1.10", ["v1.10.0-rc.7", "v1.10.0-rc.2"]), ("v1.10.0", "v1.10.0-rc.8"))

    def test_other_lines_and_junk_are_ignored(self):
        tags = ["v1.9.4", "v1.9.5-rc.3", "v1.1.0-rc.9", "v1.100.0", "sha-abcdef0", "latest", "v1.10.0-rc.x", "v1.10.0-beta.1"]
        self.assertEqual(rv.next_rc("1.10", tags), ("v1.10.0", "v1.10.0-rc.1"))

    def test_two_number_tags_count_as_x_y_0(self):
        self.assertEqual(rv.next_rc("1.0", ["v1.0"]), ("v1.0.1", "v1.0.1-rc.1"))

    def test_rejects_a_bad_line(self):
        with self.assertRaises(ValueError):
            rv.next_rc("1.10.0", [])


class LatestTests(unittest.TestCase):
    def test_is_latest(self):
        tags = ["v1.9.0", "v1.10.0", "v1.10.1"]
        self.assertTrue(rv.is_latest("v1.10.1", tags))
        self.assertFalse(rv.is_latest("v1.10.0", tags))
        self.assertFalse(rv.is_latest("v1.9.0", tags))

    def test_a_patch_of_an_older_line_is_not_latest(self):
        self.assertFalse(rv.is_latest("v1.9.5", ["v1.9.5", "v1.10.0"]))

    def test_candidates_do_not_count(self):
        self.assertTrue(rv.is_latest("v1.9.0", ["v1.9.0", "v1.10.0-rc.1"]))

    def test_not_a_final_version(self):
        with self.assertRaises(ValueError):
            rv.is_latest("v1.10.0-rc.1", [])


class StaleTests(unittest.TestCase):
    def test_candidates_of_released_versions_only(self):
        tags = ["v1.10.0-rc.1", "v1.10.0-rc.2", "v1.10.0", "v1.10.1-rc.1", "v1.9.0-rc.1", "v1.9.0"]
        self.assertEqual(rv.stale_rc(tags), ["v1.9.0-rc.1", "v1.10.0-rc.1", "v1.10.0-rc.2"])

    def test_nothing_when_nothing_is_released(self):
        self.assertEqual(rv.stale_rc(["v1.10.0-rc.1"]), [])


class PruneTests(unittest.TestCase):
    def tags(self, *lines_patches):
        return [f"v1.{y}.{z}" for y, patches in lines_patches for z in range(patches)]

    def test_newest_line_keeps_everything(self):
        self.assertEqual(rv.prune(self.tags((12, 4))), [])

    def test_older_lines_keep_the_latest_patch(self):
        self.assertEqual(rv.prune(self.tags((11, 3), (12, 1))), ["v1.11.0", "v1.11.1"])

    def test_window_of_ten_previous_lines(self):
        tags = self.tags(*[(y, 1) for y in range(1, 13)])  # 1.1 .. 1.12, one patch each
        self.assertEqual(rv.prune(tags, keep=10), ["v1.1.0"])  # 1.12 current, 1.2..1.11 kept, 1.1 out

    def test_a_line_outside_the_window_loses_even_its_latest_patch(self):
        tags = self.tags((1, 3), (2, 1), (3, 1))
        self.assertEqual(rv.prune(tags, keep=1), ["v1.1.0", "v1.1.1", "v1.1.2"])

    def test_floor_protects_the_history_before_the_policy(self):
        tags = ["v1.9.0", "v1.9.1", "v1.10.0", "v1.10.1", "v1.11.0"]
        self.assertEqual(rv.prune(tags, floor=(1, 10)), ["v1.10.0"])
        self.assertEqual(rv.prune(tags), ["v1.9.0", "v1.10.0"])

    def test_candidates_and_junk_are_not_final_releases(self):
        self.assertEqual(rv.prune(["v1.10.0-rc.1", "latest", "v1.10.0", "v1.11.0"]), [])


class CliTests(unittest.TestCase):
    def run_cli(self, argv, stdin=""):
        out = io.StringIO()
        with mock.patch("sys.stdin", io.StringIO(stdin)), redirect_stdout(out):
            rc = rv.main(argv)
        return rc, out.getvalue()

    def test_next_rc_output(self):
        self.assertEqual(self.run_cli(["next-rc", "1.10"], "v1.9.0\n"), (0, "version=v1.10.0\ntag=v1.10.0-rc.1\n"))

    def test_is_latest_exit_code(self):
        self.assertEqual(self.run_cli(["is-latest", "v1.10.0"], "v1.10.0\n")[0], 0)
        self.assertEqual(self.run_cli(["is-latest", "v1.9.0"], "v1.10.0\n")[0], 1)

    def test_bad_input_exits_1(self):
        self.assertEqual(self.run_cli(["line-of", "main"])[0], 1)
        self.assertEqual(self.run_cli(["prune", "--bogus"])[0], 1)

    def test_prune_options(self):
        rc, out = self.run_cli(["prune", "--from", "1.10", "--keep", "10"], "v1.9.0 v1.10.0 v1.10.1 v1.11.0")
        self.assertEqual((rc, out), (0, "v1.10.0\n"))


class StrictnessTests(unittest.TestCase):
    def test_unicode_digits_are_not_versions(self):
        for tag in ["v\u0661.\u0662.\u0663", "v1.2.3-rc.\u0665", "v\uff11.2.3"]:
            self.assertEqual(rv.finals([tag]), {}, tag)
            self.assertEqual(rv.candidates([tag]), [], tag)

    def test_unicode_digits_are_not_a_line(self):
        for b in ["release/\u0661.\u0662", "release/1.2\n"]:
            with self.assertRaises(ValueError, msg=repr(b)):
                rv.line_of(b)
        with self.assertRaises(ValueError):
            rv.parse_line("1.2\n")


class ExistingRcTests(unittest.TestCase):
    def test_the_candidate_of_the_commit_is_kept(self):
        self.assertEqual(rv.existing_rc("1.10", ["v1.10.0-rc.1", "v1.10.0-rc.2"], ["v1.10.0-rc.1", "v1.10.0-rc.2"]), ("v1.10.0", "v1.10.0-rc.2"))

    def test_a_hand_made_tag_of_another_patch_is_ignored(self):
        self.assertIsNone(rv.existing_rc("1.10", ["v1.10.9-rc.1"], ["v1.10.9-rc.1"]))
        self.assertIsNone(rv.existing_rc("1.10", ["v1.10.1-rc.1"], []))  # no final yet: the next patch is 0

    def test_a_tag_of_another_line_or_a_free_form_tag_is_ignored(self):
        self.assertIsNone(rv.existing_rc("1.10", ["v1.11.0-rc.1", "v1.10.x-rc.1", "v1.10.0-rc.1-evil"], []))

    def test_after_a_release_the_next_patch_counts(self):
        self.assertEqual(rv.existing_rc("1.10", ["v1.10.1-rc.1"], ["v1.10.0"]), ("v1.10.1", "v1.10.1-rc.1"))

    def test_nothing_on_the_commit(self):
        self.assertIsNone(rv.existing_rc("1.10", [], ["v1.10.0-rc.1"]))

    def test_cli(self):
        with mock.patch("sys.stdin", io.StringIO("v1.10.0-rc.1 v1.10.0-rc.2")), redirect_stdout(io.StringIO()) as out:
            self.assertEqual(rv.main(["existing-rc", "1.10", "v1.10.0-rc.1"]), 0)
        self.assertEqual(out.getvalue(), "version=v1.10.0\ntag=v1.10.0-rc.1\n")



class NewestLinesTests(unittest.TestCase):
    def test_three_newest_by_number_not_by_text(self):
        names = ["release/1.9", "release/1.10", "release/1.8", "release/1.11", "develop", "main"]
        self.assertEqual(rv.newest_lines(names), ["release/1.11", "release/1.10", "release/1.9"])

    def test_fewer_lines_than_the_count(self):
        self.assertEqual(rv.newest_lines(["release/2.20"]), ["release/2.20"])
        self.assertEqual(rv.newest_lines(["develop"]), [])

    def test_other_names_are_ignored(self):
        names = ["refs/heads/release/2.21", "release/2.22/x", "release/2.x", "security/2.21/abc1234", "backport/2.21/abc1234", "release/3"]
        self.assertEqual(rv.newest_lines(names), ["release/2.21"])

    def test_major_counts_first(self):
        self.assertEqual(rv.newest_lines(["release/1.99", "release/2.0", "release/1.100"], 2), ["release/2.0", "release/1.100"])

    def test_cli(self):
        with mock.patch("sys.stdin", io.StringIO("refs/heads/release/1.10\nrefs/heads/release/1.12\nrefs/heads/develop\nrefs/heads/release/1.11\n")), redirect_stdout(io.StringIO()) as out:
            self.assertEqual(rv.main(["newest-lines", "--count", "2"]), 0)
        self.assertEqual(out.getvalue(), "release/1.12\nrelease/1.11\n")


if __name__ == "__main__":
    unittest.main()
