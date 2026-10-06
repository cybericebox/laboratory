package imagecache

import "testing"

func TestRewrite(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035", Registries: DefaultRegistries}
	dg := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct{ in, want string }{
		{"nginx", "localhost:5035/docker.io/library/nginx:latest"},
		{"nginx:1.25", "localhost:5035/docker.io/library/nginx:1.25"},
		{"library/nginx:1.25", "localhost:5035/docker.io/library/nginx:1.25"},
		{"bitnami/redis:7", "localhost:5035/docker.io/bitnami/redis:7"},
		{"docker.io/bitnami/redis:7", "localhost:5035/docker.io/bitnami/redis:7"},
		{"index.docker.io/library/alpine:3", "localhost:5035/docker.io/library/alpine:3"},
		{"ghcr.io/cybericebox/laboratory-node:v1.2.3", "localhost:5035/ghcr.io/cybericebox/laboratory-node:v1.2.3"},
		{"quay.io/prometheus/node-exporter:v1", "localhost:5035/quay.io/prometheus/node-exporter:v1"},
		{"registry.k8s.io/pause:3.9", "localhost:5035/registry.k8s.io/pause:3.9"},
		{"nginx@" + dg, "localhost:5035/docker.io/library/nginx@" + dg},
		{"ghcr.io/o/app:1.0@" + dg, "localhost:5035/ghcr.io/o/app@" + dg},
		// Not served by the cache: left alone.
		{"registry.example.com/team/app:1", "registry.example.com/team/app:1"},
		{"localhost:5035/lab/ns/lab/web@" + dg, "localhost:5035/lab/ns/lab/web@" + dg},
		{"localhost:5035/docker.io/library/nginx:1", "localhost:5035/docker.io/library/nginx:1"},
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
	if got := r.Rewrite("registry.example.com/team/app:1"); got != "localhost:5035/registry.example.com/team/app:1" {
		t.Fatalf("got %q", got)
	}
}

func TestRepoOf(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035"}
	for in, want := range map[string]string{
		"localhost:5035/docker.io/library/nginx:1.25": "docker.io/library/nginx",
		"localhost:5035/ghcr.io/o/app@sha256:abc":     "ghcr.io/o/app",
		"localhost:5035/docker.io/library/nginx":      "docker.io/library/nginx",
		"docker.io/library/nginx:1":                   "",
	} {
		if got := r.RepoOf(in); got != want {
			t.Errorf("RepoOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRewritePinned(t *testing.T) {
	r := Rewriter{Prefix: "localhost:5035", Registries: DefaultRegistries}
	dg := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got := r.RewritePinned("nginx:1.25", dg); got != "localhost:5035/docker.io/library/nginx@"+dg {
		t.Fatal(got)
	}
	if got := r.RewritePinned("ghcr.io/o/app:v1", dg); got != "localhost:5035/ghcr.io/o/app@"+dg {
		t.Fatal(got)
	}
	if got := r.RewritePinned("registry.example.com/app:v1", dg); got != "registry.example.com/app:v1" {
		t.Fatalf("a registry the cache does not serve is never pinned or rewritten: %s", got)
	}
	if got := r.RewritePinned("nginx:1.25", ""); got != "localhost:5035/docker.io/library/nginx:1.25" {
		t.Fatalf("no digest, plain rewrite: %s", got)
	}
}
