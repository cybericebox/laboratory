// Package imagepolicy decides whether a tenant may use an image: a per-tenant allow list
// and a platform deny list of "registry/repository-prefix" entries, and the refusal of
// references that already point at the platform's image cache.
package imagepolicy

import (
	"fmt"
	"net"
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

// dockerHubHosts are the spellings of Docker Hub a reference may use; all of them are docker.io.
var dockerHubHosts = map[string]bool{
	"docker.io": true, "index.docker.io": true, "registry-1.docker.io": true, "registry.hub.docker.com": true,
}

// canonicalHost lowercases a registry host, drops a trailing dot and the default port (:443, :80) and maps the
// Docker Hub spellings to docker.io. The second result is the host without any port.
func canonicalHost(h string) (full, bare string) {
	h = strings.ToLower(strings.TrimSpace(h))
	host, port := h, ""
	if strings.HasPrefix(h, "[") {
		if end := strings.Index(h, "]"); end > 0 {
			host = h[:end+1]
			if rest := h[end+1:]; strings.HasPrefix(rest, ":") {
				port = rest[1:]
			}
		}
	} else if k := strings.LastIndex(h, ":"); k >= 0 {
		host, port = h[:k], h[k+1:]
	}
	host = strings.TrimRight(host, ".")
	if dockerHubHosts[host] {
		host = "docker.io"
	}
	if port == "443" || port == "80" {
		port = ""
	}
	if port != "" {
		return host + ":" + port, host
	}
	return host, host
}

// numericLabel reports whether a host label is a number in any of the spellings an address parser accepts
// (decimal, 0x hex, leading-zero octal), so "127.1", "2130706433" and "0x7f.1" count as IP literals.
func numericLabel(l string) bool {
	if l == "" {
		return false
	}
	if strings.HasPrefix(l, "0x") {
		return true
	}
	for _, r := range l {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// refusedHost explains why a registry host may not be named by a tenant, or returns "". A tenant names the original
// registry on the public internet: no loopback or other IP literal (in any spelling), no localhost, no host without a
// dot (a name of the cluster or the node's network).
func refusedHost(bare string) string {
	switch {
	case bare == "":
		return "has no registry host"
	case strings.HasPrefix(bare, "["):
		return "names an IP address"
	case net.ParseIP(bare) != nil:
		return "names an IP address"
	case bare == "localhost" || strings.HasSuffix(bare, ".localhost"):
		return "names localhost"
	case !strings.Contains(bare, "."):
		return "names a host without a domain"
	}
	labels := strings.Split(bare, ".")
	if numericLabel(labels[len(labels)-1]) {
		return "names an IP address"
	}
	return ""
}

// cleanRef refuses spellings that make two references to one repository compare as different strings.
func cleanRef(ref string) error {
	if ref == "" || len(ref) > 255 {
		return fmt.Errorf("invalid image reference")
	}
	for _, r := range ref {
		if r <= ' ' || r >= 0x7f || r == '\\' {
			return fmt.Errorf("invalid image reference: only printable ASCII without spaces")
		}
	}
	name := ref
	if k := strings.Index(name, "@"); k >= 0 {
		name = name[:k]
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("invalid image reference: empty, \".\" and \"..\" path components are refused")
		}
	}
	return nil
}

// Normalize returns "registry/repository" of an image reference in its canonical form: lowercase host without a
// trailing dot or default port, the Docker Hub spellings as docker.io, and docker.io defaults ("nginx" is
// "docker.io/library/nginx"); tag and digest are dropped. A reference with empty, "." or ".." path components or
// characters outside printable ASCII is refused.
func Normalize(ref string) (string, error) {
	repo, _, err := normalize(ref)
	return repo, err
}

// normalize also returns the host with no port at all.
func normalize(ref string) (repo, bareHost string, err error) {
	if err := cleanRef(ref); err != nil {
		return "", "", fmt.Errorf("%q: %v", ref, err)
	}
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", "", fmt.Errorf("invalid image reference %q: %v", ref, err)
	}
	reg := r.Context().RegistryStr()
	if reg == name.DefaultRegistry {
		reg = "docker.io"
	}
	full, bare := canonicalHost(reg)
	path := strings.ToLower(r.Context().RepositoryStr())
	return full + "/" + path, bare, nil
}

// normalizeEntry turns a policy entry into a lowercase "registry[/path]" without a
// trailing slash or star. A bare Docker Hub path ("library/", "acme") is not valid: write
// docker.io/<path>.
func normalizeEntry(e string) string {
	e = strings.ToLower(strings.TrimSpace(e))
	e = strings.TrimSuffix(e, "*")
	e = strings.TrimRight(e, "/")
	if e == "" {
		return ""
	}
	host, rest := e, ""
	if k := strings.Index(e, "/"); k >= 0 {
		host, rest = e[:k], e[k:]
	}
	full, _ := canonicalHost(host)
	if full == "" {
		full = "docker.io"
	}
	return full + rest
}

// under reports whether repo ("registry/path") equals entry or lies below it, on path boundaries.
func under(repo, entry string) bool {
	return entry != "" && (repo == entry || strings.HasPrefix(repo, entry+"/"))
}

// Check returns an error when the tenant may not use the image.
func (p Policy) Check(ref string) error {
	repo, bare, err := normalize(ref)
	if err != nil {
		return err
	}
	if why := refusedHost(bare); why != "" {
		return fmt.Errorf("image %q %s: name the original image on a public registry", ref, why)
	}
	// The platform's own registry and cache, in any spelling: by host, whatever the port.
	for _, c := range p.CachePrefixes {
		if c == "" {
			continue
		}
		if _, cb := canonicalHost(c); cb != "" && cb == bare {
			return fmt.Errorf("image %q points at the platform image cache: name the original registry", ref)
		}
	}
	// The deny list is matched on the host without a port, so another port of a denied registry is denied too.
	for _, d := range p.Deny {
		e := normalizeEntry(d)
		if under(repo, e) || under(stripPort(repo), stripPort(e)) {
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

// stripPort removes the port of the host of "host[:port]/path".
func stripPort(repoOrEntry string) string {
	host, rest := repoOrEntry, ""
	if k := strings.Index(repoOrEntry, "/"); k >= 0 {
		host, rest = repoOrEntry[:k], repoOrEntry[k:]
	}
	_, bare := canonicalHost(host)
	return bare + rest
}
