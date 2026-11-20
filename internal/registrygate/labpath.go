package registrygate

import (
	"net/url"
	"strings"
)

// labPath is a request on a snapshot repository: /v2/lab/<namespace>/<lab>/<device>/<kind>/..., or one of the shared repositories.
type labPath struct {
	Repo string // "lab/<ns>/<lab>/<dev>"
	NS   string
	Lab  string
	Dev  string
	// Kind is "manifests", "blobs" or "uploads" (a blob upload: POST .../blobs/uploads/, PATCH/PUT/GET/DELETE .../blobs/uploads/<id>).
	Kind string
	Ref  string // a tag or digest (manifests, blobs) or the upload session id (uploads)
}

// parseLabPath reads the path (already accepted by checkPath). ok is false for anything that is not a snapshot repository object.
func parseLabPath(escaped string) (labPath, bool) {
	rest, ok := strings.CutPrefix(escaped, "/v2/lab/")
	if !ok {
		return labPath{}, false
	}
	seg := strings.Split(rest, "/")
	if len(seg) < 4 {
		return labPath{}, false
	}
	p := labPath{NS: seg[0], Lab: seg[1], Dev: seg[2]}
	p.Repo = "lab/" + p.NS + "/" + p.Lab + "/" + p.Dev
	switch {
	case len(seg) == 5 && seg[3] == "manifests":
		p.Kind, p.Ref = "manifests", seg[4]
	case len(seg) == 5 && seg[3] == "blobs" && seg[4] != "uploads":
		p.Kind, p.Ref = "blobs", seg[4]
	case len(seg) == 6 && seg[3] == "blobs" && seg[4] == "uploads" && seg[5] == "":
		p.Kind = "uploads" // the path ended in a slash: the start of an upload
	case len(seg) == 5 && seg[3] == "blobs" && seg[4] == "uploads":
		p.Kind = "uploads"
	case len(seg) == 6 && seg[3] == "blobs" && seg[4] == "uploads" && seg[5] != "":
		p.Kind, p.Ref = "uploads", seg[5]
	default:
		return labPath{}, false // tags/list, referrers and the rest are the operator's
	}
	for _, s := range []string{p.NS, p.Lab, p.Dev} {
		if s == "" {
			return labPath{}, false
		}
	}
	return p, true
}

// mountFromAllowed says whether a blob mount may take its blob from repository from: the shared base image, or a cache repository
// (the way a first snapshot reuses the layers of its base image). Another device's snapshot repository is never a source.
func mountFromAllowed(from string) bool {
	from, _ = url.PathUnescape(from)
	return from == "base" || strings.HasPrefix(from, "c/")
}

// isSharedRead says whether the path is a read of the shared "base" repository (the operator's).
func isBasePath(escaped string) bool {
	return strings.HasPrefix(escaped, "/v2/base/")
}
