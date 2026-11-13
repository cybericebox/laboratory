// Package imagepolicy decides whether a tenant may use an image: a per-tenant allow list
// and a platform deny list of "registry/repository-prefix" entries, and the refusal of
// references that already point at the platform's image cache.
package imagepolicy

import (
	"errors"
	"fmt"
	"net"
	"regexp"
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
	// PlatformOnly: the only images are those of the platform registry, pinned by digest (see PlatformRef). Everything else is
	// refused with ErrNotPlatform.
	PlatformOnly bool
	// PlatformPrefix is host:port of the platform registry as the nodes see it (names.RegistryNodePrefix). It is where Map sends a
	// platform reference, and the only host a node-form platform reference may name.
	PlatformPrefix string
}

// ErrNotPlatform is what Check wraps when PlatformOnly is on and the image is not a platform image by digest.
var ErrNotPlatform = errors.New("image is not a platform image")

// PlatformScheme opens a platform reference: platform:<name>@sha256:<hex>.
const PlatformScheme = "platform:"

var (
	platformNameRe   = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	platformDigestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

const maxPlatformNameLen = 64

// PlatformRef is an image of the platform registry: repository platform/<Name>, pinned by Digest ("sha256:<hex>").
type PlatformRef struct{ Name, Digest string }

func newPlatformRef(name, digest string) (PlatformRef, bool) {
	if len(name) > maxPlatformNameLen || !platformNameRe.MatchString(name) || !platformDigestRe.MatchString(digest) {
		return PlatformRef{}, false
	}
	return PlatformRef{Name: name, Digest: digest}, true
}

// ParsePlatform reads the logical form platform:<name>@sha256:<hex> that the platform sends. A tag is not a platform reference:
// the image is always pinned.
func ParsePlatform(ref string) (PlatformRef, bool) {
	rest, ok := strings.CutPrefix(ref, PlatformScheme)
	if !ok {
		return PlatformRef{}, false
	}
	name, digest, ok := strings.Cut(rest, "@")
	if !ok {
		return PlatformRef{}, false
	}
	return newPlatformRef(name, digest)
}

// IsPlatformScheme says the reference starts with "platform:", valid or not. Such a reference is never read as a Docker Hub image.
func IsPlatformScheme(ref string) bool { return strings.HasPrefix(ref, PlatformScheme) }

// Node returns the reference the node's runtime pulls: <prefix>/platform/<name>@<digest>.
func (r PlatformRef) Node(prefix string) string {
	return prefix + "/platform/" + r.Name + "@" + r.Digest
}

// ParseNode reads <prefix>/platform/<name>@sha256:<hex> (the prefix as the nodes see the registry, case and default ports as written).
func ParseNode(ref, prefix string) (PlatformRef, bool) {
	if prefix == "" {
		return PlatformRef{}, false
	}
	rest, ok := strings.CutPrefix(ref, prefix+"/platform/")
	if !ok {
		return PlatformRef{}, false
	}
	name, digest, ok := strings.Cut(rest, "@")
	if !ok {
		return PlatformRef{}, false
	}
	return newPlatformRef(name, digest)
}

// IsPlatform says the reference names an image of the platform registry by digest, in either form.
func (p Policy) IsPlatform(ref string) bool {
	if _, ok := ParsePlatform(ref); ok {
		return true
	}
	_, ok := ParseNode(ref, p.PlatformPrefix)
	return ok
}

// Map returns the reference to run for ref: a platform reference becomes its node form, anything else is returned as it is. Check
// the reference first; Map does no admission.
func (p Policy) Map(ref string) string {
	if r, ok := ParsePlatform(ref); ok && p.PlatformPrefix != "" {
		return r.Node(p.PlatformPrefix)
	}
	return ref
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
//
// A platform reference (platform:<name>@sha256:<hex>) is the platform's own image, picked by the platform, and passes. With PlatformOnly
// nothing else does, but the same image in node form (<PlatformPrefix>/platform/<name>@sha256:<hex>): exactly the repositories platform/**,
// by digest, never lab/**, base/** or the cache.
func (p Policy) Check(ref string) error {
	if IsPlatformScheme(ref) {
		if _, ok := ParsePlatform(ref); !ok {
			return fmt.Errorf("%w: %q must be platform:<name>@sha256:<64 hex>", ErrNotPlatform, ref)
		}
		return nil
	}
	if p.PlatformOnly {
		if _, ok := ParseNode(ref, p.PlatformPrefix); ok {
			return nil
		}
		return fmt.Errorf("%w: image %q must be platform:<name>@sha256:<64 hex>", ErrNotPlatform, ref)
	}
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
