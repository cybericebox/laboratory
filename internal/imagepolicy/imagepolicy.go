// Package imagepolicy decides whether a tenant may use an image: a per-tenant allow list
// and a platform deny list of "registry/repository-prefix" entries, and the refusal of
// references that already point at the platform's image cache.
package imagepolicy

import (
	"errors"
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
	// Registries is the list of registries this laboratory pulls from (the chart's registry.allowedRegistries): an image from another
	// registry is refused with ErrRegistryNotAllowed, whatever the tenant's own list says. Empty or containing "*": every registry.
	Registries []string
}

// CanonicalRegistry is a registry host in the form Check compares: lowercase, no trailing dot or default port, Docker Hub as docker.io.
func CanonicalRegistry(host string) string {
	full, _ := canonicalHost(host)
	return full
}

// ReasonRegistryNotAllowed is the stable reason of a refusal for a registry that is not allowed (pkg/agent/client has the same constant).
const ReasonRegistryNotAllowed = "REGISTRY_NOT_ALLOWED"

// ErrRegistryNotAllowed is what Check wraps when the registry of the image is not on the laboratory's list of allowed registries.
var ErrRegistryNotAllowed = errors.New("registry is not allowed")

// AnyRegistry is the entry of Registries that allows every registry.
const AnyRegistry = "*"

// RegistryAllowed says whether the registry of an image reference ("host[:port]" in its canonical form, Docker Hub spelled in any
// way) is on the list. An empty list and "*" allow everything. The platform's own images (platform:..., the node form) are not looked at here.
func RegistryAllowed(registries []string, ref string) error {
	if len(registries) == 0 {
		return nil
	}
	for _, r := range registries {
		if strings.TrimSpace(r) == AnyRegistry {
			return nil
		}
	}
	repo, _, err := normalize(ref)
	if err != nil {
		return err
	}
	host := repo
	if k := strings.Index(repo, "/"); k >= 0 {
		host = repo[:k]
	}
	for _, r := range registries {
		if full, _ := canonicalHost(r); full != "" && full == host {
			return nil
		}
	}
	return fmt.Errorf("%w: image %q comes from %s, which is not on this laboratory's list of allowed registries", ErrRegistryNotAllowed, ref, host)
}

// ServedOnly is the list of allowed registries for a laboratory whose image cache is on: the cache serves only the registries it knows,
// so an image from any other is refused, never pulled directly. An empty list or "*" becomes the served registries; otherwise the
// entries that are served stay, in the order given. The chart refuses a list with a registry the cache does not serve; this is the same
// rule where the agent reads its configuration.
func ServedOnly(allowed, served []string) []string {
	any := len(allowed) == 0
	for _, a := range allowed {
		if strings.TrimSpace(a) == AnyRegistry {
			any = true
		}
	}
	var out []string
	if any {
		for _, s := range served {
			if full, _ := canonicalHost(s); full != "" {
				out = append(out, full)
			}
		}
		return out
	}
	for _, a := range allowed {
		full, _ := canonicalHost(a)
		for _, s := range served {
			if sf, _ := canonicalHost(s); sf == full && full != "" {
				out = append(out, full)
				break
			}
		}
	}
	return out
}

// ErrNotPlatform is what Check wraps for a platform reference (platform:<name>@sha256:<hex>): the laboratory no longer serves images of a
// registry of its own, and nothing sends such a reference any more. The stable reason is ReasonImageNotPlatform.
var ErrNotPlatform = errors.New("platform images are not served: name the image on a public registry")

// platformScheme opened the reference of an image of the platform's own registry.
const platformScheme = "platform:"

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

// Check returns an error when the tenant may not use the image. A platform reference (platform:...) is refused with ErrNotPlatform, so it is
// never read as a Docker Hub image.
func (p Policy) Check(ref string) error {
	if strings.HasPrefix(ref, platformScheme) {
		return fmt.Errorf("%w: %q", ErrNotPlatform, ref)
	}
	repo, bare, err := normalize(ref)
	if err != nil {
		return err
	}
	if why := refusedHost(bare); why != "" {
		return fmt.Errorf("image %q %s: name the original image on a public registry", ref, why)
	}
	if err := RegistryAllowed(p.Registries, ref); err != nil {
		return err
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
