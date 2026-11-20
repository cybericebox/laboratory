package registrygate

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/cybericebox/laboratory/internal/names"
)

// CachePrefix is the first path segment of the cache: /v2/c/<tenant segment>/<registry>/<repo>/(manifests|blobs)/<ref>.
const CachePrefix = "/v2/c/"

const (
	kindManifests = "manifests"
	kindBlobs     = "blobs"
	kindUploads   = "uploads"
	schemeHTTP    = "http"
)

var (
	reComponent = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	reRegistry  = regexp.MustCompile(`^[a-z0-9]+(?:[.-][a-z0-9]+)*(?::[0-9]{1,5})?$`)
	reDigest    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	reTag       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// objectRef is a parsed request of the cache path.
type objectRef struct {
	Tenant   string // the tenant segment of the path (names.TenantSegment, or "platform"): who the image is pulled for
	Registry string // docker.io
	Repo     string // library/nginx
	Kind     string // manifests or blobs
	Ref      string // a digest, or (manifests only) a tag
}

func (o objectRef) fullRepo() string { return o.Registry + "/" + o.Repo }

// zotRepo is where zot keeps the repository.
func (o objectRef) zotRepo() string { return "c/" + o.fullRepo() }

func (o objectRef) isTag() bool { return !reDigest.MatchString(o.Ref) }

// parseCachePath reads the path of a cache request (already accepted by checkPath). Only the objects a pull needs are accepted: tag
// lists, the catalog, referrers, uploads and everything else are not part of the cache path.
func parseCachePath(path string) (objectRef, bool) {
	rest, ok := strings.CutPrefix(path, CachePrefix)
	if !ok {
		return objectRef{}, false
	}
	segs := strings.Split(rest, "/")
	if len(segs) < 5 { // tenant segment, registry, at least one repository component, kind, ref
		return objectRef{}, false
	}
	o := objectRef{Tenant: segs[0], Registry: segs[1], Kind: segs[len(segs)-2], Ref: segs[len(segs)-1], Repo: strings.Join(segs[2:len(segs)-2], "/")}
	if !names.ValidSegment(o.Tenant) || !reRegistry.MatchString(o.Registry) || len(o.Repo) > 255 {
		return objectRef{}, false
	}
	for _, c := range segs[2 : len(segs)-2] {
		if !reComponent.MatchString(c) {
			return objectRef{}, false
		}
	}
	switch o.Kind {
	case kindManifests:
		if !reDigest.MatchString(o.Ref) && !reTag.MatchString(o.Ref) {
			return objectRef{}, false
		}
	case kindBlobs:
		if !reDigest.MatchString(o.Ref) {
			return objectRef{}, false
		}
	default:
		return objectRef{}, false
	}
	return o, true
}

// oldForm says whether a cache path has no tenant segment: the old form c/<registry>/<repo>/..., whose first component is a registry host
// (a dot or a port) and never a valid segment. It is answered 403, not 404.
func oldForm(path string) bool {
	rest, ok := strings.CutPrefix(path, CachePrefix)
	if !ok {
		return false
	}
	first, _, _ := strings.Cut(rest, "/")
	return !names.ValidSegment(first)
}

// isCache says whether a request belongs to the cache path.
func isCache(r *http.Request) bool { return strings.HasPrefix(r.URL.EscapedPath(), CachePrefix) }
