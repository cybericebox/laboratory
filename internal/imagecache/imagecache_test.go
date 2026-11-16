package imagecache

import (
	"errors"
	"testing"
)

func TestRewrite(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035", Registries: DefaultRegistries}
	dg := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct{ in, want string }{
		{"nginx", "localhost:5035/c/docker.io/library/nginx:latest"},
		{"nginx:1.25", "localhost:5035/c/docker.io/library/nginx:1.25"},
		{"library/nginx:1.25", "localhost:5035/c/docker.io/library/nginx:1.25"},
		{"bitnami/redis:7", "localhost:5035/c/docker.io/bitnami/redis:7"},
		{"docker.io/bitnami/redis:7", "localhost:5035/c/docker.io/bitnami/redis:7"},
		{"index.docker.io/library/alpine:3", "localhost:5035/c/docker.io/library/alpine:3"},
		{"ghcr.io/cybericebox/laboratory-node:v1.2.3", "localhost:5035/c/ghcr.io/cybericebox/laboratory-node:v1.2.3"},
		{"quay.io/prometheus/node-exporter:v1", "localhost:5035/c/quay.io/prometheus/node-exporter:v1"},
		{"registry.k8s.io/pause:3.9", "localhost:5035/c/registry.k8s.io/pause:3.9"},
		{"nginx@" + dg, "localhost:5035/c/docker.io/library/nginx@" + dg},
		{"ghcr.io/o/app:1.0@" + dg, "localhost:5035/c/ghcr.io/o/app@" + dg},
		// Not served by the cache: left alone.
		{"registry.example.com/team/app:1", "registry.example.com/team/app:1"},
		{"localhost:5035/lab/ns/lab/web@" + dg, "localhost:5035/lab/ns/lab/web@" + dg},
		{"localhost:5035/c/docker.io/library/nginx:1", "localhost:5035/c/docker.io/library/nginx:1"},
		{"10.0.0.5:5000/app:1", "10.0.0.5:5000/app:1"},
		// Malformed or empty: left alone.
		{"", ""},
		{"NOT A REF!!", "NOT A REF!!"},
	}
	for _, c := range cases {
		if got := r.Rewrite(c.in); got != c.want {
			t.Errorf("Rewrite(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRewriteDisabled(t *testing.T) {
	if got := (Rewriter{}).Rewrite("nginx"); got != "nginx" {
		t.Fatalf("empty rewriter must not touch refs, got %q", got)
	}
	if got := (Rewriter{Prefix: "localhost:5035"}).Rewrite("nginx"); got != "nginx" {
		t.Fatalf("no registries, no rewrite, got %q", got)
	}
}

func TestExtraRegistry(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035", Registries: append([]string{"registry.example.com"}, DefaultRegistries...)}
	if got := r.Rewrite("registry.example.com:8443/team/app:1"); got != "registry.example.com:8443/team/app:1" {
		// A registry with a port is a different registry name from the configured one.
		t.Fatalf("got %q", got)
	}
	if got := r.Rewrite("registry.example.com/team/app:1"); got != "localhost:5035/c/registry.example.com/team/app:1" {
		t.Fatalf("got %q", got)
	}
}

func TestRepoOf(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035"}
	for in, want := range map[string]string{
		"localhost:5035/c/docker.io/library/nginx:1.25": "c/docker.io/library/nginx",
		"localhost:5035/c/ghcr.io/o/app@sha256:abc":     "c/ghcr.io/o/app",
		"localhost:5035/c/docker.io/library/nginx":      "c/docker.io/library/nginx",
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
	if got := r.RewritePinned("nginx:1.25", dg); got != "localhost:5035/c/docker.io/library/nginx@"+dg {
		t.Fatal(got)
	}
	if got := r.RewritePinned("ghcr.io/o/app:v1", dg); got != "localhost:5035/c/ghcr.io/o/app@"+dg {
		t.Fatal(got)
	}
	if got := r.RewritePinned("registry.example.com/app:v1", dg); got != "registry.example.com/app:v1" {
		t.Fatalf("a registry the cache does not serve is never pinned or rewritten: %s", got)
	}
	if got := r.RewritePinned("nginx:1.25", ""); got != "localhost:5035/c/docker.io/library/nginx:1.25" {
		t.Fatalf("no digest, plain rewrite: %s", got)
	}
}

// A platform image arrives from the agent in node form (localhost:<port>/platform/<name>@sha256:...): the cache leaves it alone, pinned or
// not, because it already points at the registry and its first path element is not an upstream registry.
func TestPlatformImagesAreNeverRewritten(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035", Registries: DefaultRegistries}
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	ref := "localhost:5035/platform/tools@" + digest
	if got := r.Rewrite(ref); got != ref {
		t.Errorf("Rewrite = %q", got)
	}
	if got := r.RewritePinned(ref, ""); got != ref {
		t.Errorf("RewritePinned with no digest = %q", got)
	}
	if got := r.RewritePinned(ref, digest); got != ref {
		t.Errorf("RewritePinned = %q", got)
	}
	if DigestOf(ref) != digest {
		t.Errorf("the digest of a platform image is read from the reference: %q", DigestOf(ref))
	}
}

// With the cache on an image that cannot go through it is refused, never pulled directly.
func TestRewriteRequiredRefusesWhatTheCacheCannotServe(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035", Registries: []string{"docker.io", "ghcr.io"}}
	for ref, want := range map[string]string{
		"nginx":                              "localhost:5035/c/docker.io/library/nginx:latest",
		"ghcr.io/org/app:1":                  "localhost:5035/c/ghcr.io/org/app:1",
		"localhost:5035/lab/snapshots/x:tag": "localhost:5035/lab/snapshots/x:tag", // already the cache
	} {
		got, err := r.RewriteRequired(ref)
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", ref, got, err, want)
		}
	}
	for _, ref := range []string{"gcr.io/x/y:1", "registry.example.com:5000/a/b", "NOT A REF", "ghcr.io/Org/App"} {
		if got, err := r.RewriteRequired(ref); !errors.Is(err, ErrNotCacheable) || got != "" {
			t.Errorf("%s must be refused: %q %v", ref, got, err)
		}
	}
	off := Rewriter{}
	if got, err := off.RewriteRequired("gcr.io/x/y:1"); err != nil || got != "gcr.io/x/y:1" {
		t.Errorf("with the cache off the reference is left alone: %q %v", got, err)
	}
}
