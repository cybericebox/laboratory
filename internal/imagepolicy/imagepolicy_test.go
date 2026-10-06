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

// The spellings the re-audit found (R-3) all fail or are caught by the same rule as the plain one.
func TestCheckRefusesAlternateSpellings(t *testing.T) {
	deny := Policy{Deny: []string{"ghcr.io/platform", "docker.io/acme"}, CachePrefixes: []string{"localhost:5035", "zot.laboratory.svc:5000"}}
	for _, ref := range []string{
		"ghcr.io:443/platform/x", "ghcr.io./platform/x", "GHCR.IO/platform/x", "ghcr.io:8443/platform/x", "ghcr.io//platform/x",
		"ghcr.io/other/../platform/x", "ghcr.io/./platform/x", "registry-1.docker.io/acme/x", "index.docker.io/acme/x",
		"docker.io/acme/x", "acme/x",
		"127.0.0.1:5035/lab/ns/lab/dev@sha256:" + zeros(), "127.0.0.1:5035/ghcr.io/platform/x", "LOCALHOST:5035/lab/x", "localhost/x/y",
		"127.1/x/y", "0x7f.1/x/y", "[::1]:5035/x/y", "10.0.0.1/x", "registry:5000/x", "zot.laboratory.svc:5000/x",
		"zot.laboratory.svc/x", "foo.localhost/x", "ghcr.io/ white/x",
	} {
		if err := deny.Check(ref); err == nil {
			t.Errorf("%q must be refused", ref)
		}
	}
	for _, ref := range []string{"ghcr.io/platformer/x", "docker.io/library/nginx", "nginx:1.27", "quay.io/a/b@sha256:" + zeros(), "ghcr.io:443/other/x"} {
		if err := deny.Check(ref); err != nil {
			t.Errorf("%q must pass: %v", ref, err)
		}
	}
}

func TestAllowEntriesAreCanonical(t *testing.T) {
	p := Policy{Allow: []string{"GHCR.io./Acme/", "index.docker.io/library"}}
	for ref, ok := range map[string]bool{"ghcr.io/acme/web": true, "ghcr.io:443/acme/web": true, "nginx": true, "ghcr.io:5000/acme/web": false} {
		if err := p.Check(ref); (err == nil) != ok {
			t.Errorf("%q: %v", ref, err)
		}
	}
}
