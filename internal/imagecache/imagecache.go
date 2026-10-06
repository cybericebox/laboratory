// Package imagecache rewrites image references so the nodes pull them through
// the platform's in-cluster pull-through cache (zot with on-demand sync).
//
// The cache is served on every node as localhost:<port> by the node-agent's
// registry forwarder. A reference REG/repo:tag becomes
// localhost:<port>/REG/repo:tag; the cache maps the REG prefix back to the
// upstream registry, so the layout needs no per-image configuration.
package imagecache

import (
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
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
	out := r.Prefix + "/" + reg + "/" + parsed.Context().RepositoryStr()
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
