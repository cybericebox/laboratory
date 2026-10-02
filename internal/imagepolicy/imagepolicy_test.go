package imagepolicy

import "testing"

func TestCheck(t *testing.T) {
	cases := []struct {
		name string
		p    Policy
		ref  string
		ok   bool
	}{
		{"open", Policy{}, "ghcr.io/anyone/anything:1", true},
		{"hub default", Policy{Allow: []string{"docker.io/library"}}, "nginx", true},
		{"hub default tag", Policy{Allow: []string{"docker.io/library/"}}, "nginx:1.27", true},
		{"hub user not library", Policy{Allow: []string{"docker.io/library"}}, "acme/web", false},
		{"registry entry", Policy{Allow: []string{"quay.io"}}, "quay.io/x/y@sha256:" + zeros(), true},
		{"prefix entry", Policy{Allow: []string{"ghcr.io/acme/"}}, "ghcr.io/acme/web:1", true},
		{"star entry", Policy{Allow: []string{"ghcr.io/acme/*"}}, "ghcr.io/acme/web:1", true},
		{"other org", Policy{Allow: []string{"ghcr.io/acme/"}}, "ghcr.io/other/web:1", false},
		{"sibling prefix is not under", Policy{Allow: []string{"ghcr.io/acme"}}, "ghcr.io/acme-private/web", false},
		{"case", Policy{Allow: []string{"GHCR.io/Acme"}}, "ghcr.io/acme/web", true},
		{"deny wins", Policy{Allow: []string{"ghcr.io/"}, Deny: []string{"ghcr.io/platform/"}}, "ghcr.io/platform/private:1", false},
		{"deny only", Policy{Deny: []string{"ghcr.io/platform"}}, "ghcr.io/platform/x/y", false},
		{"deny leaves the rest", Policy{Deny: []string{"ghcr.io/platform"}}, "ghcr.io/platformer/x", true},
		{"cache prefix", Policy{CachePrefixes: []string{"localhost:5035"}}, "localhost:5035/ghcr.io/platform/private:1", false},
		{"cache prefix with allow", Policy{CachePrefixes: []string{"localhost:5035"}, Allow: []string{"localhost:5035"}}, "localhost:5035/ghcr.io/x/y", false},
		{"bad ref", Policy{}, "NOT A REF", false},
	}
	for _, c := range cases {
		if err := c.p.Check(c.ref); (err == nil) != c.ok {
			t.Errorf("%s: %q: err=%v want ok=%v", c.name, c.ref, err, c.ok)
		}
	}
}

func zeros() string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}
