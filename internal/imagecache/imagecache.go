// Package imagecache rewrites image references so the nodes pull them through
// the platform's in-cluster image cache (the registry gate in front of zot).
//
// The cache is served on every node as localhost:<port> by the node-agent's
// registry forwarder. A reference REG/repo:tag of a tenant becomes
// localhost:<port>/c/<tenant>/REG/repo:tag; the gate takes the tenant from the
// path (names.TenantSegment), checks the pull against the schedule and maps the
// REG segment back to the upstream registry of its list (allowed registries),
// so the layout needs no per-image configuration. The c/ segment keeps the
// cache apart from the snapshots, which the gate does not check against an
// upstream. The internal tree of the cache has no tenant: c/REG/repo (RepoOf).
package imagecache

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/cybericebox/laboratory/internal/names"
)

// CacheSegment is the first path segment of the cache under the registry's address.
const CacheSegment = "c"

// HeaderGateCache is the response header with which the registry gate says where an answer on the cache path came from:
// GateCacheHit when the object was served from the cache's own storage (after the gate proved the pull). The prewarm asks for it to know
// that a blob is really stored and not only proved.
const (
	HeaderGateCache = "Registry-Gate-Cache"
	GateCacheHit    = "hit"
)

// DefaultRegistries are the upstream registries the chart configures by default.
var DefaultRegistries = []string{"docker.io", "ghcr.io", "quay.io", "registry.k8s.io"}

// Rewriter rewrites image references for one cache.
type Rewriter struct {
	// Prefix is host:port of the cache as the nodes see it, e.g. "localhost:5035".
	Prefix string
	// Registries are the upstream registries the cache serves (docker.io, ghcr.io, ...).
	Registries []string
}

// ErrNotCacheable is what RewriteRequired wraps when the cache is on and the image cannot go through it: a reference that does not
// parse, or a registry the cache does not serve. The cache being on, such an image is refused, never pulled directly: a direct pull
// would bypass the gate, the credentials rule and the allowed list at once.
var ErrNotCacheable = errors.New("the image cannot be pulled through the image cache")

// Enabled says whether images are pulled through a cache at all.
func (r Rewriter) Enabled() bool { return r.Prefix != "" }

// Scoped is a Rewriter bound to one tenant segment: every reference it writes carries that segment.
type Scoped struct {
	r       Rewriter
	segment string
}

// For binds the rewriter to a tenant: the segment is names.TenantSegment(tenant) (the name itself when it is safe, a hashed form
// otherwise).
func (r Rewriter) For(tenant string) Scoped {
	return Scoped{r: r, segment: names.TenantSegment(tenant)}
}

// ForPlatform binds the rewriter to the platform's own components (the VPN and gateway pods of every group): the segment is
// names.SegmentPlatform.
func (r Rewriter) ForPlatform() Scoped { return Scoped{r: r, segment: names.SegmentPlatform} }

// RewriteFor is Rewrite for the pods of a tenant.
func (r Rewriter) RewriteFor(tenant, ref string) string { return r.For(tenant).Rewrite(ref) }

// RewriteRequired is Rewrite for a cache that is on: the reference to pull through the cache, or an error wrapping ErrNotCacheable when
// the reference is malformed or its registry is not served. A reference that already points at the cache (a snapshot) passes
// unchanged. With the cache off (no prefix) the reference is returned as is.
func (s Scoped) RewriteRequired(ref string) (string, error) {
	r := s.r
	if r.Prefix == "" || ref == "" {
		return ref, nil
	}
	if prefixed, err := s.checkCachePrefixed(ref); prefixed || err != nil {
		return ref, err
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("%w: %q is not a valid image reference", ErrNotCacheable, ref)
	}
	reg := parsed.Context().RegistryStr()
	if reg == name.DefaultRegistry {
		reg = "docker.io"
	}
	if !r.serves(reg) {
		return "", fmt.Errorf("%w: the registry %s of %q is not served by the cache", ErrNotCacheable, reg, ref)
	}
	return s.Rewrite(ref), nil
}

// hasPrefixFold is strings.HasPrefix without regard to case: a registry host is not case sensitive, and "LOCALHOST:5035/c/x/..." must not
// slip past a comparison made on the lower case form.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// checkCachePrefixed decides what a reference that already names the cache (its address, in any case) may be. Written straight into a Device
// or Lab it would otherwise reach another tenant's cache path or the platform's: only the cache path of this scope's own tenant segment
// passes, and a snapshot (lab/..., which the gate judges by its own rule). Anything else under the prefix is refused with ErrNotCacheable.
// prefixed says the reference names the cache at all.
func (s Scoped) checkCachePrefixed(ref string) (prefixed bool, err error) {
	if !hasPrefixFold(ref, s.r.Prefix+"/") {
		return false, nil
	}
	rest := ref[len(s.r.Prefix)+1:]
	switch {
	case strings.HasPrefix(rest, "lab/"):
		return true, nil
	case strings.HasPrefix(rest, CacheSegment+"/"):
		if seg, _, ok := strings.Cut(rest[len(CacheSegment)+1:], "/"); ok && seg == s.segment {
			return true, nil
		}
		return true, fmt.Errorf("%w: %q names the cache path of another tenant", ErrNotCacheable, ref)
	}
	return true, fmt.Errorf("%w: %q names the cache but not a cache path of this tenant or a snapshot", ErrNotCacheable, ref)
}

// Rewrite returns the reference to pull through the cache, <prefix>/c/<segment>/<registry>/<repo>. It returns ref
// unchanged when the rewriter is empty, the reference is malformed, it already
// points at the cache, or its registry is not one the cache serves (a private
// registry, or a snapshot image that is already local).
//
// A reference without a registry is docker.io; one without a repository
// namespace gets "library/"; one without a tag gets ":latest" (Kubernetes
// treats the two alike). A reference with a digest keeps only the digest.
func (s Scoped) Rewrite(ref string) string {
	r := s.r
	if r.Prefix == "" || len(r.Registries) == 0 || ref == "" {
		return ref
	}
	if hasPrefixFold(ref, r.Prefix+"/") {
		return ref // already the cache's (checkCachePrefixed says whether it is this tenant's to name)
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return ref
	}
	reg := parsed.Context().RegistryStr()
	if reg == name.DefaultRegistry {
		reg = "docker.io"
	}
	if !r.serves(reg) {
		return ref
	}
	out := r.Prefix + "/" + CacheSegment + "/" + s.segment + "/" + reg + "/" + parsed.Context().RepositoryStr()
	switch p := parsed.(type) {
	case name.Digest:
		return out + "@" + p.DigestStr()
	case name.Tag:
		return out + ":" + p.TagStr()
	}
	return ref
}

func (r Rewriter) serves(reg string) bool {
	for _, s := range r.Registries {
		if s == reg {
			return true
		}
	}
	return false
}

// RepoOf returns the internal repository path of a reference that points at the cache (prefix stripped, tag or digest removed), ""
// if it does not. The tenant segment of a cache path is not part of it: <prefix>/c/<segment>/<registry>/<repo> gives
// c/<registry>/<repo>, the path of the shared tree, which is also what the snapshot paths (lab/...) keep as they are.
func (r Rewriter) RepoOf(ref string) string {
	rest, ok := strings.CutPrefix(ref, r.Prefix+"/")
	if !ok || r.Prefix == "" {
		return ""
	}
	if i := strings.Index(rest, "@"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		rest = rest[:i]
	}
	if tail, ok := strings.CutPrefix(rest, CacheSegment+"/"); ok {
		if seg, repo, found := strings.Cut(tail, "/"); found && names.ValidSegment(seg) {
			return CacheSegment + "/" + repo
		}
	}
	return rest
}
