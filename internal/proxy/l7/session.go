package l7

import (
	"errors"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/cybericebox/laboratory/internal/grant"
)

// AuthPath is the path a platform page posts an access grant to: POST https://<device>-<code><kind>.<base>/_auth. The proxy consumes it
// and never forwards it to a lab.
//
// The request is a form post (application/x-www-form-urlencoded, at most authBodyMax bytes) of the fields
//
//	grant  the access grant, a JWS of at most grant.MaxLen bytes (see internal/grant)
//	next   optional: a root-relative path (with a query) to land on; anything else becomes "/"
//
// The form comes from the platform page, so it is cross-site: the proxy requires the Origin header and matches it against the list of
// platform origins, which stops an attacker's page from posting its own grant into a victim's browser (login CSRF). On a web host the
// answer is a 303 to `next` with the grant as a host-only cookie; on a console host it is the console page, with no cookie. A GET (the
// old link with a token in its URL) consumes nothing and gets the "session over" card, so a link of the old shape fails closed.
const AuthPath = "/_auth"

const (
	// authBodyMax bounds the body of the post; the grant field is bounded by grant.MaxLen inside it.
	authBodyMax = 8 << 10
	// nextMax bounds the next field.
	nextMax = 2048
)

// readAuthForm runs the checks every post to AuthPath passes before a grant is looked at: the method, the content type, the size of the
// body and of the fields, and the Origin. It answers a failure itself and reports false. The reason of a refusal that says nothing about
// the grant (the wrong method, the size, the type) is the status; every refusal about the grant or the origin is the one card.
func (h *Handler) readAuthForm(w http.ResponseWriter, r *http.Request) (raw, next string, ok bool) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodPost:
	case http.MethodGet, http.MethodHead:
		// The old link carried the token in its URL; nothing is consumed from a GET.
		h.expired(w, r)
		return "", "", false
	default:
		w.Header().Set("Allow", http.MethodPost)
		fail(w, r, http.StatusMethodNotAllowed, pageGone, "method not allowed")
		return "", "", false
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
		fail(w, r, http.StatusUnsupportedMediaType, pageGone, "unsupported media type")
		return "", "", false
	}
	r.Body = http.MaxBytesReader(w, r.Body, authBodyMax)
	if err := r.ParseForm(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, r, http.StatusRequestEntityTooLarge, pageGone, "request too large")
			return "", "", false
		}
		h.expired(w, r)
		return "", "", false
	}
	// only the body counts: a grant in the query string is a link of the old shape
	raw, next = r.PostForm.Get("grant"), r.PostForm.Get("next")
	if raw == "" || len(raw) > grant.MaxLen || len(next) > nextMax || !h.platformOrigin(r) {
		h.expired(w, r)
		return "", "", false
	}
	return raw, next, true
}

// platformOrigin says whether the Origin of the request is one of the platform's pages (proxy.l7.platformOrigins). A request without an
// Origin is refused, and so is every request while the list is empty.
func (h *Handler) platformOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	return o != "" && h.origins.Ancestor(o) != ancestorNone
}

// authenticate turns a grant posted by the platform page into the cookie of a web host and sends the browser to a safe destination.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request, task string) {
	raw, next, ok := h.readAuthForm(w, r)
	if !ok {
		return
	}
	if !h.acquireVerify(w, r) {
		return
	}
	g, err := h.verifyLogin(raw, task, requestHost(r))
	h.releaseVerify()
	if err != nil {
		h.expired(w, r)
		return
	}
	// Only a grant that verifies is counted: a flood of garbage costs one cheap rejection each and uses up nobody's budget.
	if !h.throttleHandoff(w, r, task, g.Group+"|"+g.client()) {
		return
	}
	h.setGrantCookie(w, raw, g.End(), h.now())
	http.Redirect(w, r, handoffDestination(next), http.StatusSeeOther)
}

// rootRelative says whether s starts with one slash that is not followed by another slash or a backslash (a browser reads both
// as the start of an authority), holds no backslash anywhere and no control character (a browser drops tabs and line breaks
// before it parses, so "/<tab>/host" is "//host").
func rootRelative(s string) bool {
	if len(s) == 0 || s[0] != '/' {
		return false
	}
	if len(s) > 1 && (s[1] == '/' || s[1] == '\\') {
		return false
	}
	return !strings.Contains(s, "\\") && strings.IndexFunc(s, unicode.IsControl) < 0
}

// handoffDestination accepts only an unambiguous root-relative device URL.
// Recheck decoded forms too: encoded separators, controls and dot segments must
// not become an authority or return to the handoff endpoint after normalization.
// Deep or malformed encoding falls back to the root instead of guessing intent.
func handoffDestination(target string) string {
	candidate := target
	for range 4 {
		if !rootRelative(candidate) {
			return "/"
		}
		u, err := url.Parse(candidate)
		if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Opaque != "" ||
			strings.Contains(u.Path, "//") || path.Clean(u.Path) == AuthPath {
			return "/"
		}
		decoded, err := url.PathUnescape(candidate)
		if err != nil {
			return "/"
		}
		if decoded == candidate {
			return target
		}
		candidate = decoded
	}
	return "/"
}

// setGrantCookie sets the cookie of a web session: the grant itself, host-only, until the grant ends.
func (h *Handler) setGrantCookie(w http.ResponseWriter, value string, end, now time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: h.cookieName, Value: value, Path: "/", // no Domain: host-only, the cookie belongs to the one device host that set it
		Expires: end, MaxAge: int(end.Sub(now).Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// requestHost is the host name of the request: lower case, without a port.
func requestHost(r *http.Request) string {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(host, ".")
}
