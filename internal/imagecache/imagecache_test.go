package imagecache

import (
	"errors"
	"testing"

	"github.com/cybericebox/laboratory/internal/names"
)

func TestRewrite(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035", Registries: DefaultRegistries}
	dg := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct{ in, want string }{
		{"nginx", "localhost:5035/c/acme/docker.io/library/nginx:latest"},
		{"nginx:1.25", "localhost:5035/c/acme/docker.io/library/nginx:1.25"},
		{"library/nginx:1.25", "localhost:5035/c/acme/docker.io/library/nginx:1.25"},
		{"bitnami/redis:7", "localhost:5035/c/acme/docker.io/bitnami/redis:7"},
		{"docker.io/bitnami/redis:7", "localhost:5035/c/acme/docker.io/bitnami/redis:7"},
		{"index.docker.io/library/alpine:3", "localhost:5035/c/acme/docker.io/library/alpine:3"},
		{"ghcr.io/cybericebox/laboratory-node:v1.2.3", "localhost:5035/c/acme/ghcr.io/cybericebox/laboratory-node:v1.2.3"},
		{"quay.io/prometheus/node-exporter:v1", "localhost:5035/c/acme/quay.io/prometheus/node-exporter:v1"},
		{"registry.k8s.io/pause:3.9", "localhost:5035/c/acme/registry.k8s.io/pause:3.9"},
		{"nginx@" + dg, "localhost:5035/c/acme/docker.io/library/nginx@" + dg},
		{"ghcr.io/o/app:1.0@" + dg, "localhost:5035/c/acme/ghcr.io/o/app@" + dg},
		// Not served by the cache: left alone.
		{"registry.example.com/team/app:1", "registry.example.com/team/app:1"},
		{"localhost:5035/lab/ns/lab/web@" + dg, "localhost:5035/lab/ns/lab/web@" + dg},
		{"localhost:5035/c/acme/docker.io/library/nginx:1", "localhost:5035/c/acme/docker.io/library/nginx:1"},
		{"10.0.0.5:5000/app:1", "10.0.0.5:5000/app:1"},
		// Malformed or empty: left alone.
		{"", ""},
		{"NOT A REF!!", "NOT A REF!!"},
	}
	for _, c := range cases {
		if got := r.RewriteFor("acme", c.in); got != c.want {
			t.Errorf("Rewrite(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRewriteDisabled(t *testing.T) {
	if got := (Rewriter{}).RewriteFor("acme", "nginx"); got != "nginx" {
		t.Fatalf("empty rewriter must not touch refs, got %q", got)
	}
	if got := (Rewriter{Prefix: "localhost:5035"}).RewriteFor("acme", "nginx"); got != "nginx" {
		t.Fatalf("no registries, no rewrite, got %q", got)
	}
}

func TestExtraRegistry(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035", Registries: append([]string{"registry.example.com"}, DefaultRegistries...)}
	if got := r.RewriteFor("acme", "registry.example.com:8443/team/app:1"); got != "registry.example.com:8443/team/app:1" {
		// A registry with a port is a different registry name from the configured one.
		t.Fatalf("got %q", got)
	}
	if got := r.RewriteFor("acme", "registry.example.com/team/app:1"); got != "localhost:5035/c/acme/registry.example.com/team/app:1" {
		t.Fatalf("got %q", got)
	}
}

func TestRepoOf(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035"}
	for in, want := range map[string]string{
		"localhost:5035/c/acme/docker.io/library/nginx:1.25": "c/docker.io/library/nginx",
		"localhost:5035/c/acme/ghcr.io/o/app@sha256:abc":     "c/ghcr.io/o/app",
		"localhost:5035/c/platform/docker.io/library/nginx":  "c/docker.io/library/nginx",
		"localhost:5035/c/t-0123456789ab/quay.io/o/a:1":      "c/quay.io/o/a",
		"localhost:5035/lab/ns/lab/dev:tag":                  "lab/ns/lab/dev",
		"docker.io/library/nginx:1":                     "",
	} {
		if got := r.RepoOf(in); got != want {
			t.Errorf("RepoOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRewritePinned(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035", Registries: DefaultRegistries}
	dg := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got := r.For("acme").RewritePinned("nginx:1.25", dg); got != "localhost:5035/c/acme/docker.io/library/nginx@"+dg {
		t.Fatal(got)
	}
	if got := r.For("acme").RewritePinned("ghcr.io/o/app:v1", dg); got != "localhost:5035/c/acme/ghcr.io/o/app@"+dg {
		t.Fatal(got)
	}
	if got := r.For("acme").RewritePinned("registry.example.com/app:v1", dg); got != "registry.example.com/app:v1" {
		t.Fatalf("a registry the cache does not serve is never pinned or rewritten: %s", got)
	}
	if got := r.For("acme").RewritePinned("nginx:1.25", ""); got != "localhost:5035/c/acme/docker.io/library/nginx:1.25" {
		t.Fatalf("no digest, plain rewrite: %s", got)
	}
}

// A platform image arrives from the agent in node form (localhost:<port>/platform/<name>@sha256:...): the cache leaves it alone, pinned or
// not, because it already points at the registry and its first path element is not an upstream registry.
func TestPlatformImagesAreNeverRewritten(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035", Registries: DefaultRegistries}
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	ref := "localhost:5035/platform/tools@" + digest
	if got := r.RewriteFor("acme", ref); got != ref {
		t.Errorf("Rewrite = %q", got)
	}
	if got := r.For("acme").RewritePinned(ref, ""); got != ref {
		t.Errorf("RewritePinned with no digest = %q", got)
	}
	if got := r.For("acme").RewritePinned(ref, digest); got != ref {
		t.Errorf("RewritePinned = %q", got)
	}
	if DigestOf(ref) != digest {
		t.Errorf("the digest of a platform image is read from the reference: %q", DigestOf(ref))
	}
}

func TestTenantSegmentInReference(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035", Registries: DefaultRegistries}
	for tenant, want := range map[string]string{
		"default":   "localhost:5035/c/default/docker.io/library/nginx:1",
		"acme":      "localhost:5035/c/acme/docker.io/library/nginx:1",
		"Acme Corp": "localhost:5035/c/" + names.TenantSegment("Acme Corp") + "/docker.io/library/nginx:1",
		"platform":  "localhost:5035/c/" + names.TenantSegment("platform") + "/docker.io/library/nginx:1",
	} {
		if got := r.RewriteFor(tenant, "nginx:1"); got != want {
			t.Errorf("%q: %q, want %q", tenant, got, want)
		}
	}
	if seg := names.TenantSegment("Acme Corp"); seg == "Acme Corp" || len(seg) != 14 {
		t.Errorf("an unsafe name is hashed: %q", seg)
	}
	if got := r.ForPlatform().Rewrite("ghcr.io/o/vpn:1"); got != "localhost:5035/c/platform/ghcr.io/o/vpn:1" {
		t.Errorf("platform components use the segment platform: %q", got)
	}
	// A tenant named "platform" cannot take the platform's segment.
	if r.RewriteFor("platform", "nginx:1") == r.ForPlatform().Rewrite("nginx:1") {
		t.Error("tenant platform must not collide with the platform segment")
	}
}

// With the cache on an image that cannot go through it is refused, never pulled directly.
func TestRewriteRequiredRefusesWhatTheCacheCannotServe(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035", Registries: []string{"docker.io", "ghcr.io"}}
	for ref, want := range map[string]string{
		"nginx":                              "localhost:5035/c/acme/docker.io/library/nginx:latest",
		"ghcr.io/org/app:1":                  "localhost:5035/c/acme/ghcr.io/org/app:1",
		"localhost:5035/lab/snapshots/x:tag": "localhost:5035/lab/snapshots/x:tag", // already the cache
	} {
		got, err := r.For("acme").RewriteRequired(ref)
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", ref, got, err, want)
		}
	}
	for _, ref := range []string{"gcr.io/x/y:1", "registry.example.com:5000/a/b", "NOT A REF", "ghcr.io/Org/App"} {
		if got, err := r.For("acme").RewriteRequired(ref); !errors.Is(err, ErrNotCacheable) || got != "" {
			t.Errorf("%s must be refused: %q %v", ref, got, err)
		}
	}
	off := Rewriter{}
	if got, err := off.For("acme").RewriteRequired("gcr.io/x/y:1"); err != nil || got != "gcr.io/x/y:1" {
		t.Errorf("with the cache off the reference is left alone: %q %v", got, err)
	}
}
