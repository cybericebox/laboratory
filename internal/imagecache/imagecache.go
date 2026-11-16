// Package imagecache rewrites image references so the nodes pull them through
// the platform's in-cluster image cache (the registry gate in front of zot).
//
// The cache is served on every node as localhost:<port> by the node-agent's
// registry forwarder. A reference REG/repo:tag becomes
// localhost:<port>/c/REG/repo:tag; the gate maps the REG segment back to the
// upstream registry of its list (allowed registries), so the layout needs no
// per-image configuration. The c/ segment keeps the cache apart from the
// snapshots and platform images, which the gate does not check against an
// upstream.
package imagecache

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
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

// RewriteRequired is Rewrite for a cache that is on: the reference to pull through the cache, or an error wrapping ErrNotCacheable when
// the reference is malformed or its registry is not served. A reference that already points at the cache (a snapshot, a platform image)
// passes unchanged. With the cache off (no prefix) the reference is returned as is.
func (r Rewriter) RewriteRequired(ref string) (string, error) {
	if r.Prefix == "" || ref == "" || strings.HasPrefix(ref, r.Prefix+"/") {
		return ref, nil
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
	return r.Rewrite(ref), nil
}

// Rewrite returns the reference to pull through the cache. It returns ref
// unchanged when the rewriter is empty, the reference is malformed, it already
// points at the cache, or its registry is not one the cache serves (a private
// registry, or a snapshot image that is already local).
//
// A reference without a registry is docker.io; one without a repository
// namespace gets "library/"; one without a tag gets ":latest" (Kubernetes
// treats the two alike). A reference with a digest keeps only the digest.
func (r Rewriter) Rewrite(ref string) string {
	if r.Prefix == "" || len(r.Registries) == 0 || ref == "" {
		return ref
	}
	if strings.HasPrefix(ref, r.Prefix+"/") {
		return ref
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
	out := r.Prefix + "/" + CacheSegment + "/" + reg + "/" + parsed.Context().RepositoryStr()
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

// RepoOf returns the cache repository path of a reference that points at the
// cache (prefix stripped, tag or digest removed), "" if it does not.
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
	return rest
}
