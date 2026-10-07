#!/usr/bin/env python3
"""Track replaced BuildKit caches on Docker Hub and delete only verified caches.

The current buildcache-develop/buildcache tags survive between builds. Before export,
snapshot creates a durable history tag, so interrupted jobs do not lose the
previous digest. Prune removes history older than 72 hours after a successful
export. Retire clears verified caches after a full release. It never deletes
layer blobs, runnable images or referenced manifests.
"""
import argparse
import base64
import hashlib
import json
import os
import re
import time
from urllib.error import HTTPError
from urllib.parse import urlencode, urlparse
from urllib.request import Request, urlopen

IMAGE = "application/vnd.oci.image.manifest.v1+json"
INDEX = "application/vnd.oci.image.index.v1+json"
CACHE = "application/vnd.buildkit.cacheconfig.v0"
HISTORY = re.compile(r"^buildcache-history-([0-9]+)-([0-9a-f]{64})$")
RUN = re.compile(r"^buildcache-run-([0-9]+)-([0-9]+)-([0-9]+)$")
DIGEST = re.compile(r"^sha256:[0-9a-f]{64}$")
ACCEPT = ", ".join([IMAGE, INDEX, "application/vnd.docker.distribution.manifest.v2+json",
                    "application/vnd.docker.distribution.manifest.list.v2+json"])


class Referenced(Exception):
    """The registry refused to delete a referenced manifest."""


class Missing(Exception):
    """The requested manifest does not exist."""


def is_cache(raw):
    obj = json.loads(raw)
    if obj.get("config", {}).get("mediaType") == CACHE:
        return True
    # Older BuildKit exported a cache index containing layers and cache config,
    # rather than references to architecture-specific image manifests.
    entries = obj.get("manifests", [])
    return bool(entries) and sum(x.get("mediaType") == CACHE for x in entries) == 1 and all(
        x.get("mediaType") == CACHE or x.get("mediaType", "").startswith(
            ("application/vnd.oci.image.layer.", "application/vnd.docker.image.rootfs.diff."))
        for x in entries)


class HubRegistry:
    def __init__(self, image):
        if not re.fullmatch(r"[a-z0-9_-]+/[a-z0-9._-]+", image):
            raise ValueError("Use a Docker Hub namespace/repository name")
        self.image = image
        username = os.environ["DOCKERHUB_USERNAME"]
        password = os.environ["DOCKERHUB_TOKEN"]
        basic = base64.b64encode(f"{username}:{password}".encode()).decode()
        query = urlencode({"service": "registry.docker.io", "scope": f"repository:{image}:pull,push,delete"})
        _, body, _ = self.request("GET", "https://auth.docker.io/token?" + query,
                                  {"Authorization": "Basic " + basic})
        self.token = json.loads(body)["token"]
        _, body, _ = self.request("POST", "https://hub.docker.com/v2/users/login/",
                                  {"Content-Type": "application/json"},
                                  json.dumps({"username": username, "password": password}).encode())
        self.hub_token = json.loads(body)["token"]

    @staticmethod
    def request(method, url, headers, body=None):
        # Tokens never appear in output; reject cross-host pagination URLs.
        if urlparse(url).scheme != "https" or urlparse(url).hostname not in {
                "auth.docker.io", "hub.docker.com", "registry-1.docker.io"}:
            raise ValueError("Unexpected Docker API URL")
        try:
            with urlopen(Request(url, data=body, headers=headers, method=method), timeout=60) as response:
                return response.status, response.read(), response.headers
        except HTTPError as error:
            if error.code == 404:
                raise Missing(url) from None
            if method == "DELETE" and error.code == 403:
                raise Referenced(url) from None
            # Docker Hub handles asynchronous manifest deletion in the background.
            if method == "DELETE" and "registry-1.docker.io" in url and error.code == 500:
                print("Manifest deletion queued by Docker Hub; verify in Image Management")
                return 500, b"", error.headers
            raise RuntimeError(f"Docker API {method} failed: HTTP {error.code}") from None

    def registry(self, method, ref, body=None, media_type=None):
        headers = {"Authorization": "Bearer " + self.token, "Accept": ACCEPT}
        if media_type:
            headers["Content-Type"] = media_type
        return self.request(method, f"https://registry-1.docker.io/v2/{self.image}/manifests/{ref}", headers, body)

    def get(self, ref):
        _, raw, headers = self.registry("GET", ref)
        digest = "sha256:" + hashlib.sha256(raw).hexdigest()
        if headers.get("Docker-Content-Digest") != digest:
            raise ValueError("Manifest content does not match the registry digest")
        return digest, raw

    def put(self, tag, raw):
        self.registry("PUT", tag, raw, json.loads(raw)["mediaType"])

    def tags(self):
        result = {}
        url = f"https://hub.docker.com/v2/repositories/{self.image}/tags/?page_size=100"
        while url:
            _, body, _ = self.request("GET", url, {"Authorization": "JWT " + self.hub_token})
            page = json.loads(body)
            for tag in page["results"]:
                result[tag["name"]] = tag.get("digest")
            url = page.get("next")
        return result

    def delete_tag(self, tag):
        self.request("DELETE", f"https://hub.docker.com/v2/repositories/{self.image}/tags/{tag}/",
                     {"Authorization": "JWT " + self.hub_token})

    def delete_manifest(self, digest):
        self.registry("DELETE", digest)


class CacheManager:
    def __init__(self, registry):
        self.registry = registry

    def snapshot(self, now=None):
        try:
            digest, raw = self.registry.get("buildcache-develop")
        except Missing:
            print("No current cache: the first build will create it")
            return
        if not is_cache(raw):
            raise ValueError("buildcache-develop is not a BuildKit cache; refusing to replace it")
        for tag in self.registry.tags():
            match = HISTORY.fullmatch(tag)
            if match and match[2] == digest.removeprefix("sha256:"):
                existing, _ = self.registry.get(tag)
                if existing == digest:
                    print(f"Already tracked {digest}")
                    return
        tag = f"buildcache-history-{int(time.time() if now is None else now)}-{digest[7:]}"
        self.registry.put(tag, raw)
        print(f"Tracked previous cache as {tag}")

    def publish(self, tag, now=None):
        if not RUN.fullmatch(tag):
            raise ValueError("Expected a buildcache-run timestamp/run/attempt tag")
        _, raw = self.registry.get(tag)
        if not is_cache(raw):
            raise ValueError("The exported manifest is not a BuildKit cache")
        self.snapshot(now)
        # Keep the unique export tag: even concurrent publishers cannot lose
        # an overwritten digest, and a cancelled job remains discoverable.
        self.registry.put("buildcache-develop", raw)
        print(f"Published {tag} as buildcache-develop")

    def protected(self, tags):
        protected = set()

        def visit(ref):
            digest, raw = self.registry.get(ref)
            if digest in protected:
                return
            protected.add(digest)
            if not is_cache(raw):
                for entry in json.loads(raw).get("manifests", []):
                    visit(entry["digest"])

        for tag in tags:
            visit(tag)
        return protected

    def prune(self, keep_hours=72, dry_run=False, now=None, retire=False):
        now = time.time() if now is None else now
        tags = self.registry.tags()
        def expired(tag):
            if retire and tag in {"buildcache-develop", "buildcache"}:
                return True
            match = HISTORY.fullmatch(tag) or RUN.fullmatch(tag)
            return bool(match and now - int(match[1]) >= keep_hours * 3600)

        eligible = {}
        for tag in tags:
            if not expired(tag):
                continue
            history = HISTORY.fullmatch(tag)
            digest, raw = self.registry.get(tag)
            if (history and digest != "sha256:" + history[2]) or not is_cache(raw):
                print(f"Keep {tag}: not the expected BuildKit cache")
                continue
            eligible[tag] = (digest, raw)
        # Any failed read aborts before the first deletion. Include recent run
        # tags, all release/special tags, unexpected objects and their children.
        protected = self.protected(t for t in tags if t not in eligible)
        candidates = {}
        for tag, (digest, raw) in eligible.items():
            if digest in protected:
                print(f"Keep {tag}: cache still referenced by a live tag/image")
                continue
            candidates.setdefault(digest, (raw, []))[1].append(tag)
        for digest, (raw, aliases) in candidates.items():
            print(f"{'Would delete' if dry_run else 'Delete'} old cache {digest}: {', '.join(aliases)}")
            if dry_run:
                continue
            removed = []
            try:
                for tag in aliases:
                    # Unique run/history tags are immutable by convention.
                    if self.registry.get(tag)[0] != digest:
                        raise ValueError("Tracking tag changed; refusing deletion")
                    self.registry.delete_tag(tag)
                    removed.append(tag)
                self.registry.delete_manifest(digest)
            except Referenced:
                for tag in removed:
                    self.registry.put(tag, raw)
                print(f"Keep {digest}: registry reports a reference; tracking restored")
            except Missing:
                print(f"Already removed {digest}")
            except Exception:
                for tag in removed:
                    self.registry.put(tag, raw)
                raise

    def orphans(self, digests, dry_run=False):
        protected = self.protected(self.registry.tags())
        for digest in digests:
            if not DIGEST.fullmatch(digest):
                raise ValueError("A full sha256 manifest digest is required")
            if digest in protected:
                print(f"Keep {digest}: referenced by a tag/image")
                continue
            try:
                actual, raw = self.registry.get(digest)
            except Missing:
                print(f"Already removed {digest}")
                continue
            if actual != digest or not is_cache(raw):
                print(f"Keep {digest}: not a verified BuildKit cache")
                continue
            print(f"{'Would delete' if dry_run else 'Delete'} orphan BuildKit cache {digest}")
            if not dry_run:
                try:
                    self.registry.delete_manifest(digest)
                except Referenced:
                    print(f"Keep {digest}: registry reports a reference")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["snapshot", "publish", "prune", "retire", "orphans"])
    parser.add_argument("image")
    parser.add_argument("digests", nargs="*")
    parser.add_argument("--keep-hours", type=int, default=72)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    if args.keep_hours < 0:
        parser.error("--keep-hours must be non-negative")
    manager = CacheManager(HubRegistry(args.image))
    if args.command == "snapshot":
        if args.dry_run:
            parser.error("snapshot creates a tracking tag; --dry-run is not supported")
        manager.snapshot()
    elif args.command == "publish":
        if len(args.digests) != 1 or args.dry_run:
            parser.error("publish requires one buildcache-run tag and does not support --dry-run")
        manager.publish(args.digests[0])
    elif args.command == "prune":
        manager.prune(args.keep_hours, args.dry_run)
    elif args.command == "retire":
        manager.prune(0, args.dry_run, retire=True)
    else:
        if not args.digests:
            parser.error("orphans requires explicit digests from Image Management")
        manager.orphans(args.digests, args.dry_run)


if __name__ == "__main__":
    main()
