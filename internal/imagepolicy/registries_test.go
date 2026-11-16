package imagepolicy

import (
	"errors"
	"testing"
)

// The list of allowed registries decides by the host of the image, in any spelling of it; empty and "*" allow everything.
func TestRegistryAllowed(t *testing.T) {
	list := []string{"docker.io", "GHCR.io", "registry.example.com:5000"}
	for _, c := range []struct {
		name   string
		list   []string
		ref    string
		wantOK bool
	}{
		{"hub default", list, "nginx", true},
		{"hub org", list, "acme/web:1", true},
		{"hub spelling", list, "index.docker.io/library/nginx", true},
		{"listed", list, "ghcr.io/acme/web:1", true},
		{"listed port", list, "registry.example.com:5000/x:1", true},
		{"other port", list, "registry.example.com:5001/x:1", false},
		{"no port", list, "registry.example.com/x:1", false},
		{"unlisted", list, "gcr.io/acme/web", false},
		{"digest", list, "quay.io/x/y@sha256:" + zeros(), false},
		{"empty list", nil, "gcr.io/acme/web", true},
		{"star", []string{"*"}, "gcr.io/acme/web", true},
		{"default port is dropped", []string{"registry.example.com:443"}, "registry.example.com/x", true},
	} {
		err := RegistryAllowed(c.list, c.ref)
		if (err == nil) != c.wantOK {
			t.Errorf("%s: %v", c.name, err)
		}
		if err != nil && !errors.Is(err, ErrRegistryNotAllowed) && c.wantOK == false {
			t.Errorf("%s: the error must say the registry is not allowed: %v", c.name, err)
		}
	}
	// Check applies it in front of the tenant's own list and never to the platform's references
	p := Policy{Registries: list, Allow: []string{"gcr.io"}}
	if err := p.Check("gcr.io/x/y"); !errors.Is(err, ErrRegistryNotAllowed) {
		t.Errorf("the tenant's list cannot widen the laboratory's: %v", err)
	}
	if err := p.Check("platform:tool@sha256:" + zeros()); err != nil {
		t.Errorf("platform images pass: %v", err)
	}
}
