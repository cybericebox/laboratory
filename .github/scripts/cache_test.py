"""Safety tests for registry cache maintenance; no network or Docker required."""
import importlib.util
import json
from pathlib import Path
import unittest

SCRIPT = Path(__file__).with_name("cache.py")
if not SCRIPT.exists():
    raise AssertionError("Registry cache maintenance has not been implemented")
spec = importlib.util.spec_from_file_location("cache", SCRIPT)
cache = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cache)


def manifest(config="application/vnd.buildkit.cacheconfig.v0"):
    return json.dumps({"schemaVersion": 2, "mediaType": cache.IMAGE,
                       "config": {"mediaType": config}, "layers": []}).encode()


OLD = "sha256:" + "a" * 64
NEW = "sha256:" + "b" * 64
IMAGE = "sha256:" + "c" * 64
NOW = 1000000
HISTORY = "buildcache-history-100-" + "a" * 64


class Registry:
    def __init__(self):
        self.refs = {"buildcache-develop": NEW, HISTORY: OLD, "v1.0.0": IMAGE}
        self.objects = {OLD: manifest(), NEW: manifest(), IMAGE: manifest("application/vnd.oci.image.config.v1+json")}
        self.deleted = []
        self.blocked = False

    def tags(self):
        return dict(self.refs)

    def get(self, ref):
        digest = self.refs.get(ref, ref)
        return digest, self.objects[digest]

    def put(self, tag, raw):
        self.refs[tag] = next(d for d, body in self.objects.items() if body == raw)

    def delete_tag(self, tag):
        del self.refs[tag]

    def delete_manifest(self, digest):
        if self.blocked:
            raise cache.Referenced("referenced")
        self.deleted.append(digest)
        del self.objects[digest]


class Safety(unittest.TestCase):
    def setUp(self):
        self.registry = Registry()
        # Distinguish cache versions without depending on their fake digest.
        obj = json.loads(self.registry.objects[NEW]); obj["annotations"] = {"version": "new"}
        self.registry.objects[NEW] = json.dumps(obj).encode()
        self.manager = cache.CacheManager(self.registry)

    def test_no_expired_cache_does_not_fetch_live_image_manifests(self):
        del self.registry.refs[HISTORY]
        calls = []
        original = self.registry.get
        def counted_get(ref):
            calls.append(ref)
            return original(ref)
        self.registry.get = counted_get
        self.manager.prune(now=NOW)
        self.assertEqual(calls, [])

    def test_current_cache_survives_even_if_history_is_old(self):
        self.registry.refs[HISTORY] = NEW
        self.manager.prune(now=NOW)
        self.assertEqual(self.registry.refs["buildcache-develop"], NEW)
        self.assertFalse(self.registry.deleted)

    def test_expired_cache_is_deleted_but_release_survives(self):
        self.manager.prune(now=NOW)
        self.assertEqual(self.registry.deleted, [OLD])
        self.assertEqual(self.registry.refs["v1.0.0"], IMAGE)

    def test_recent_history_survives(self):
        del self.registry.refs[HISTORY]
        self.registry.refs[f"buildcache-history-{NOW - 10}-" + "a" * 64] = OLD
        self.manager.prune(now=NOW)
        self.assertFalse(self.registry.deleted)

    def test_dry_run_has_no_mutations(self):
        before = dict(self.registry.refs)
        self.manager.prune(now=NOW, dry_run=True)
        self.assertEqual(self.registry.refs, before)
        self.assertFalse(self.registry.deleted)

    def test_release_alias_protects_cache_digest(self):
        self.registry.refs["special"] = OLD
        self.manager.prune(now=NOW)
        self.assertFalse(self.registry.deleted)
        self.assertEqual(self.registry.refs["special"], OLD)

    def test_child_of_tagged_index_is_protected(self):
        self.registry.objects[IMAGE] = json.dumps({"mediaType": cache.INDEX, "manifests": [{"digest": OLD}]}).encode()
        self.manager.prune(now=NOW)
        self.assertFalse(self.registry.deleted)

    def test_ordinary_image_with_history_name_is_not_deleted(self):
        self.registry.objects[OLD] = manifest("application/vnd.oci.image.config.v1+json")
        self.manager.prune(now=NOW)
        self.assertIn(HISTORY, self.registry.refs)
        self.assertFalse(self.registry.deleted)

    def test_registry_reference_guard_restores_tracking_tag(self):
        self.registry.blocked = True
        self.manager.prune(now=NOW)
        self.assertEqual(self.registry.refs[HISTORY], OLD)
        self.assertFalse(self.registry.deleted)

    def test_snapshot_is_durable_and_does_not_refresh_old_history(self):
        self.registry.refs["buildcache-develop"] = OLD
        self.manager.snapshot(now=NOW)
        self.assertEqual([t for t in self.registry.refs if t.startswith("buildcache-history-")], [HISTORY])

    def test_snapshot_rejects_regular_image(self):
        self.registry.refs["buildcache-develop"] = IMAGE
        with self.assertRaises(ValueError):
            self.manager.snapshot(now=NOW)

    def test_publish_keeps_export_reference_until_cleanup(self):
        tag = f"buildcache-run-{NOW}-123-1"
        self.registry.refs[tag] = OLD
        self.manager.publish(tag, now=NOW)
        self.assertEqual(self.registry.refs["buildcache-develop"], OLD)
        self.assertEqual(self.registry.refs[tag], OLD)

    def test_expired_run_cache_is_deleted(self):
        del self.registry.refs[HISTORY]
        self.registry.refs["buildcache-run-100-123-1"] = OLD
        self.manager.prune(now=NOW)
        self.assertEqual(self.registry.deleted, [OLD])

    def test_all_expired_aliases_removed_before_deleting_manifest(self):
        self.registry.refs["buildcache-run-100-123-1"] = OLD
        self.manager.prune(now=NOW)
        self.assertEqual(self.registry.deleted, [OLD])
        self.assertNotIn(HISTORY, self.registry.refs)
        self.assertNotIn("buildcache-run-100-123-1", self.registry.refs)

    def test_recent_alias_preserves_old_digest(self):
        self.registry.refs[f"buildcache-run-{NOW}-123-1"] = OLD
        self.manager.prune(now=NOW)
        self.assertFalse(self.registry.deleted)

    def test_full_release_removes_cache_aliases_without_deleting_digests(self):
        self.registry.refs["buildcache-run-100-123-1"] = OLD
        self.manager.retire(now=NOW)
        self.assertFalse(self.registry.deleted)
        self.assertNotIn("buildcache-develop", self.registry.refs)
        self.assertNotIn("buildcache-run-100-123-1", self.registry.refs)
        self.assertEqual(self.registry.refs["v1.0.0"], IMAGE)
        self.assertIn(OLD, self.registry.refs.values())
        self.assertIn(NEW, self.registry.refs.values())
        self.manager.prune(now=NOW + 73 * 3600)
        self.assertCountEqual(self.registry.deleted, [OLD, NEW])
        self.assertEqual(self.registry.refs, {"v1.0.0": IMAGE})

    def test_retire_preserves_unreleased_dev_image(self):
        self.registry.refs["sha-1234567"] = IMAGE
        self.manager.retire(now=NOW)
        self.assertEqual(self.registry.refs["sha-1234567"], IMAGE)

    def test_retire_dry_run_has_no_mutations(self):
        before = dict(self.registry.refs)
        self.manager.retire(now=NOW, dry_run=True)
        self.assertEqual(self.registry.refs, before)
        self.assertFalse(self.registry.deleted)

    def test_retire_preserves_regular_image_with_cache_tag(self):
        self.registry.refs["buildcache-develop"] = IMAGE
        self.manager.retire(now=NOW)
        self.assertEqual(self.registry.refs["buildcache-develop"], IMAGE)
        self.assertFalse(self.registry.deleted)

    def test_unknown_orphan_must_be_cache_and_unreferenced(self):
        self.manager.orphans([IMAGE, NEW, OLD], dry_run=True)
        self.assertFalse(self.registry.deleted)
        # An orphaned runnable image must also survive a real run.
        del self.registry.refs["v1.0.0"]
        self.manager.orphans([IMAGE])
        self.assertIn(IMAGE, self.registry.objects)


if __name__ == "__main__":
    unittest.main()
