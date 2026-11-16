package registrygate

import (
	"net/http"
	"regexp"
	"strings"
)

// CachePrefix is the first path segment of the cache: /v2/c/<registry>/<repo>/(manifests|blobs)/<ref>.
const CachePrefix = "/v2/c/"

const (
	kindManifests = "manifests"
	kindBlobs     = "blobs"
	schemeHTTP    = "http"
)

var (
	reComponent = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	reRegistry  = regexp.MustCompile(`^[a-z0-9]+(?:[.-][a-z0-9]+)*(?::[0-9]{1,5})?$`)
	reDigest    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	reTag       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	reTenant    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// objectRef is a parsed request of the cache path.
type objectRef struct {
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
	if len(segs) < 4 { // registry, at least one repository component, kind, ref
		return objectRef{}, false
	}
	o := objectRef{Registry: segs[0], Kind: segs[len(segs)-2], Ref: segs[len(segs)-1], Repo: strings.Join(segs[1:len(segs)-2], "/")}
	if !reRegistry.MatchString(o.Registry) || len(o.Repo) > 255 {
		return objectRef{}, false
	}
	for _, c := range segs[1 : len(segs)-2] {
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

// splitUser separates the tenant the operator wrote into the username: "<tenant>~<upstream username>".
func splitUser(u string) (tenant, user string, ok bool) {
	tenant, user, found := strings.Cut(u, "~")
	if !found || user == "" || !reTenant.MatchString(tenant) {
		return "", "", false
	}
	return tenant, user, true
}

// isCache says whether a request belongs to the cache path.
func isCache(r *http.Request) bool { return strings.HasPrefix(r.URL.EscapedPath(), CachePrefix) }
