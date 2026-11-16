package registrygate

import (
	"strings"
	"testing"
)

func TestCheckPath(t *testing.T) {
	ok := []string{
		"/v2", "/v2/",
		"/v2/_catalog",
		"/v2/lab/ns/lab1/dev/manifests/sha256:0123456789abcdef",
		"/v2/docker.io/library/nginx/manifests/latest",
		"/v2/lab/a/blobs/uploads/",
		"/v2/lab/a/blobs/uploads/0b0b0b0b-1111-2222-3333-444444444444",
		"/v2/a%2Db/manifests/x", // an encoded hyphen is harmless
	}
	bad := []string{
		"", "/", "/v1/x", "/v2x/a", "/zot/auth/login", "/metrics", "v2/a",
		"/v2/../x", "/v2/a/../b", "/v2/a/./b", "/v2/a//b", "/v2//a", "/v2/a/", "/v2/a/b/blobs/uploads//",
		"/v2/a%2Fb", "/v2/a%2fb", "/v2/a%5Cb", "/v2/a%2e%2e/b", "/v2/%2e%2e/b", "/v2/a%00b", "/v2/a%0ab", "/v2/a%25b",
		"/v2/a%zz", "/v2/a%2", "/v2/a%", `/v2/a\b`, "/v2/a b", "/v2/a\tb", "/v2/a?b", "/v2/a#b", "/v2/a\x7fb",
		"/v2/" + strings.Repeat("a", MaxPathLength),
	}
	for _, p := range ok {
		if err := checkPath(p); err != nil {
			t.Errorf("%q must be accepted: %v", p, err)
		}
	}
	for _, p := range bad {
		if err := checkPath(p); err == nil {
			t.Errorf("%q must be refused", p)
		}
	}
}

// FuzzCheckPath: whatever the input, an accepted path has no dot segment, no empty segment (but the one trailing slash of an upload), no
// encoded slash or backslash and no control character, and is under /v2. The checker never panics.
func FuzzCheckPath(f *testing.F) {
	for _, s := range []string{"/v2/", "/v2/a/b/manifests/x", "/v2/a/../b", "/v2/a%2Fb", "/v2/a/blobs/uploads/", "/v2/%2e%2e/x", "//v2"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if checkPath(p) != nil {
			return
		}
		if p != "/v2" && !strings.HasPrefix(p, "/v2/") {
			t.Fatalf("accepted outside /v2: %q", p)
		}
		low := strings.ToLower(p)
		for _, bad := range []string{"%2f", "%5c", "%00", "%2e", "%25", "\\", "\x00", "//", "/../", "/./"} {
			if strings.Contains(low, bad) {
				t.Fatalf("accepted %q containing %q", p, bad)
			}
		}
		if strings.HasSuffix(p, "/..") || strings.HasSuffix(p, "/.") {
			t.Fatalf("accepted a dot segment at the end: %q", p)
		}
		for i := 0; i < len(p); i++ {
			if p[i] < 0x21 || p[i] == 0x7f {
				t.Fatalf("accepted a control character: %q", p)
			}
		}
	})
}
