package registrygate

import (
	"strings"
	"testing"
)

func TestParseCachePath(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	ok := map[string]objectRef{
		"/v2/c/docker.io/library/nginx/manifests/" + d:      {"docker.io", "library/nginx", "manifests", d},
		"/v2/c/ghcr.io/org/sub/img/blobs/" + d:              {"ghcr.io", "org/sub/img", "blobs", d},
		"/v2/c/registry.k8s.io/pause/manifests/3.10":        {"registry.k8s.io", "pause", "manifests", "3.10"},
		"/v2/c/localhost:5000/team/img/manifests/v1.0_rc-1": {"localhost:5000", "team/img", "manifests", "v1.0_rc-1"},
	}
	for p, want := range ok {
		got, good := parseCachePath(p)
		if !good || got != want {
			t.Errorf("%s: %+v %v, want %+v", p, got, good, want)
		}
	}
	bad := []string{
		"/v2/c/", "/v2/c/docker.io", "/v2/c/docker.io/nginx", "/v2/c/docker.io/nginx/manifests", "/v2/c/docker.io/nginx/tags/list",
		"/v2/c/docker.io/nginx/blobs/uploads/", "/v2/c/docker.io/_catalog", "/v2/c/docker.io/nginx/referrers/" + d,
		"/v2/c/docker.io/nginx/blobs/latest",         // a blob is a digest
		"/v2/c/docker.io/nginx/blobs/sha256:abc",     // a short digest
		"/v2/c/docker.io/Nginx/manifests/" + d,       // upper case in a repository name
		"/v2/c/docker.io/ngi nx/manifests/" + d,      // not the OCI grammar
		"/v2/c/docker.io/-nginx/manifests/" + d,      // a component does not start with a separator
		"/v2/c/DOCKER.io/nginx/manifests/" + d,       //
		"/v2/c/docker.io/nginx/manifests/-bad",       // a tag does not start with -
		"/v2/c/docker.io/nginx/manifests/sha512:abc", // not sha256
		"/v2/c/doc ker.io/nginx/manifests/latest",
		"/v2/c/docker.io/" + strings.Repeat("a/", 200) + "x/manifests/latest", // too long
	}
	for _, p := range bad {
		if _, good := parseCachePath(p); good {
			t.Errorf("%s must be refused", p)
		}
	}
}

func TestSplitUser(t *testing.T) {
	for in, want := range map[string][3]string{"t1~alice": {"t1", "alice", "ok"}, "acme-co~bob~x": {"acme-co", "bob~x", "ok"}} {
		tenant, user, ok := splitUser(in)
		if !ok || tenant != want[0] || user != want[1] {
			t.Errorf("%q: %q %q %v", in, tenant, user, ok)
		}
	}
	for _, in := range []string{"alice", "~alice", "t1~", "T1~alice", "t 1~alice", "-t~a", strings.Repeat("a", 64) + "~u"} {
		if _, _, ok := splitUser(in); ok {
			t.Errorf("%q must be refused", in)
		}
	}
}

func FuzzParseCachePath(f *testing.F) {
	d := "sha256:" + strings.Repeat("a", 64)
	for _, s := range []string{"/v2/c/docker.io/library/nginx/manifests/" + d, "/v2/c/a/b/blobs/" + d, "/v2/c/", "/v2/c/a/b/c/d/e"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		o, ok := parseCachePath(p)
		if !ok {
			return
		}
		// what is accepted rebuilds into the same path, and holds nothing a path can hide
		if CachePrefix+o.Registry+"/"+o.Repo+"/"+o.Kind+"/"+o.Ref != p {
			t.Fatalf("%q parsed to %+v", p, o)
		}
		if strings.Contains(p, "..") || strings.Contains(o.Repo, "//") || strings.HasPrefix(o.Repo, "/") || strings.ContainsAny(p, "%\\ ?#") {
			t.Fatalf("accepted %q", p)
		}
	})
}
