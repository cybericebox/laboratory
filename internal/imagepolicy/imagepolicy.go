// Package imagepolicy decides whether a tenant may use an image: a per-tenant allow list
// and a platform deny list of "registry/repository-prefix" entries, and the refusal of
// references that already point at the platform's image cache.
package imagepolicy

import (
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// Policy is what one tenant's images are checked against.
type Policy struct {
	// Allow: when not empty the image must lie under one of the entries.
	Allow []string
	// Deny: the image must lie under none of the entries (the platform's private repositories).
	Deny []string
	// CachePrefixes are the host:port addresses of the platform registry and image cache. A
	// tenant names the original image; the operator routes it through the cache, so a
	// reference that already points at the cache would reach what it holds for other users.
	CachePrefixes []string
}

// Normalize returns "registry/repository" of an image reference with docker.io defaults
// ("nginx" is "docker.io/library/nginx"); tag and digest are dropped.
func Normalize(ref string) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("invalid image reference %q: %v", ref, err)
	}
	reg := r.Context().RegistryStr()
	if reg == name.DefaultRegistry {
		reg = "docker.io"
	}
	return reg + "/" + r.Context().RepositoryStr(), nil
}

// normalizeEntry turns a policy entry into a lowercase "registry[/path]" without a
// trailing slash or star. A bare Docker Hub path ("library/", "acme") is not valid: write
// docker.io/<path>.
func normalizeEntry(e string) string {
	e = strings.ToLower(strings.TrimSpace(e))
	e = strings.TrimSuffix(e, "*")
	e = strings.TrimRight(e, "/")
	e = strings.TrimPrefix(e, "index.docker.io")
	if strings.HasPrefix(e, "/") {
		e = "docker.io" + e
	}
	return e
}

// under reports whether repo ("registry/path") equals entry or lies below it, on path boundaries.
func under(repo, entry string) bool {
	return entry != "" && (repo == entry || strings.HasPrefix(repo, entry+"/"))
}

// Check returns an error when the tenant may not use the image.
func (p Policy) Check(ref string) error {
	repo, err := Normalize(ref)
	if err != nil {
		return err
	}
	repo = strings.ToLower(repo)
	for _, c := range p.CachePrefixes {
		if c != "" && strings.HasPrefix(repo, strings.ToLower(c)+"/") {
			return fmt.Errorf("image %q points at the platform image cache: name the original registry", ref)
		}
	}
	for _, d := range p.Deny {
		if under(repo, normalizeEntry(d)) {
			return fmt.Errorf("image %q is not available to tenants", ref)
		}
	}
	if len(p.Allow) == 0 {
		return nil
	}
	for _, a := range p.Allow {
		if under(repo, normalizeEntry(a)) {
			return nil
		}
	}
	return fmt.Errorf("image %q is not on the tenant's allow list", ref)
}
