package registrygate

import (
	"strings"
	"testing"
)

func TestParseCachePath(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	ok := map[string]objectRef{
		"/v2/c/acme/docker.io/library/nginx/manifests/" + d:      {"acme", "docker.io", "library/nginx", "manifests", d},
		"/v2/c/t-0123456789ab/ghcr.io/org/sub/img/blobs/" + d:    {"t-0123456789ab", "ghcr.io", "org/sub/img", "blobs", d},
		"/v2/c/platform/registry.k8s.io/pause/manifests/3.10":    {"platform", "registry.k8s.io", "pause", "manifests", "3.10"},
		"/v2/c/default/localhost:5000/team/img/manifests/v1.0_r": {"default", "localhost:5000", "team/img", "manifests", "v1.0_r"},
	}
	for p, want := range ok {
		got, good := parseCachePath(p)
		if !good || got != want {
			t.Errorf("%s: %+v %v, want %+v", p, got, good, want)
		}
	}
	bad := []string{
		"/v2/c/", "/v2/c/acme", "/v2/c/acme/docker.io", "/v2/c/acme/docker.io/nginx", "/v2/c/acme/docker.io/nginx/manifests", "/v2/c/acme/docker.io/nginx/tags/list",
		"/v2/c/acme/docker.io/nginx/blobs/uploads/", "/v2/c/acme/docker.io/_catalog", "/v2/c/acme/docker.io/nginx/referrers/" + d,
		"/v2/c/acme/docker.io/nginx/blobs/latest",         // a blob is a digest
		"/v2/c/acme/docker.io/nginx/blobs/sha256:abc",     // a short digest
		"/v2/c/acme/docker.io/Nginx/manifests/" + d,       // upper case in a repository name
		"/v2/c/acme/docker.io/ngi nx/manifests/" + d,      // not the OCI grammar
		"/v2/c/acme/docker.io/-nginx/manifests/" + d,      // a component does not start with a separator
		"/v2/c/acme/DOCKER.io/nginx/manifests/" + d,       //
		"/v2/c/acme/docker.io/nginx/manifests/-bad",       // a tag does not start with -
		"/v2/c/acme/docker.io/nginx/manifests/sha512:abc", // not sha256
		"/v2/c/acme/doc ker.io/nginx/manifests/latest",
		"/v2/c/acme/docker.io/" + strings.Repeat("a/", 200) + "x/manifests/latest", // too long
		"/v2/c/docker.io/library/nginx/manifests/" + d,                             // the old form: no tenant segment, a host in its place
		"/v2/c/Acme/docker.io/nginx/manifests/latest",                              // a segment is lower case
		"/v2/c/a.b/docker.io/nginx/manifests/latest",                               // a segment has no dot
		"/v2/c/-a/docker.io/nginx/manifests/latest",                                // nor a leading hyphen
		"/v2/c/localhost:5000/team/img/manifests/latest",                           // nor a port
	}
	for _, p := range bad {
		if _, good := parseCachePath(p); good {
			t.Errorf("%s must be refused", p)
		}
	}
}

// A path in the old form is told apart from other malformed paths: it is a 403, not a 404.
func TestOldFormIsRecognised(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	for _, p := range []string{"/v2/c/docker.io/library/nginx/manifests/" + d, "/v2/c/localhost:5000/team/img/blobs/" + d, "/v2/c/ghcr.io/a/b/manifests/x"} {
		if !oldForm(p) {
			t.Errorf("%s is the old form", p)
		}
	}
	for _, p := range []string{"/v2/c/acme/docker.io/nginx/manifests/x", "/v2/c/acme/docker.io", "/v2/lab/a/b/c/manifests/latest"} {
		if oldForm(p) {
			t.Errorf("%s is not the old form", p)
		}
	}
}

func FuzzParseCachePath(f *testing.F) {
	d := "sha256:" + strings.Repeat("a", 64)
	for _, s := range []string{"/v2/c/acme/docker.io/library/nginx/manifests/" + d, "/v2/c/t/a/b/blobs/" + d, "/v2/c/", "/v2/c/a/b/c/d/e"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		o, ok := parseCachePath(p)
		if !ok {
			return
		}
		// what is accepted rebuilds into the same path, and holds nothing a path can hide
		if CachePrefix+o.Tenant+"/"+o.Registry+"/"+o.Repo+"/"+o.Kind+"/"+o.Ref != p {
			t.Fatalf("%q parsed to %+v", p, o)
		}
		if strings.Contains(p, "..") || strings.Contains(o.Repo, "//") || strings.HasPrefix(o.Repo, "/") || strings.ContainsAny(p, "%\\ ?#") {
			t.Fatalf("accepted %q", p)
		}
	})
}
