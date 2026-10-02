package l7

import (
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AuthPath is the path of the handoff link the platform gives the participant:
// https://<device>-<code>.<base>/_auth?t=<jwt>. The proxy consumes it and never
// forwards it to a lab.
const AuthPath = "/_auth"

// handoff turns a platform link into the proxy's own cookie and sends the
// browser to the site root, so the token leaves the URL.
func (h *Handler) handoff(w http.ResponseWriter, r *http.Request, host string) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	raw := r.URL.Query().Get("t")
	if raw == "" || len(h.secret) == 0 {
		h.expired(w, r)
		return
	}
	now := h.now()
	claims, err := verifyHandoff(raw, h.keys, h.groupTenant, host, h.now, h.handoffMax)
	if err != nil {
		h.expired(w, r)
		return
	}
	// The session ends at the link's sess, but not later than sessionMax from now; it is valid for sessionIdle,
	// and renewals (renewSession) slide that until the absolute end.
	abs := time.Unix(claims.Session, 0)
	if limit := now.Add(h.sessionMax); h.sessionMax > 0 && abs.After(limit) {
		abs = limit
	}
	value, end, err := h.sessionCookie(jwtClaims{
		GroupID: claims.GroupID, Client: claims.client(), Tenant: claims.Issuer, Abs: abs.Unix(),
		RegisteredClaims: jwt.RegisteredClaims{IssuedAt: jwt.NewNumericDate(now)},
	}, now)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.setSessionCookie(w, value, end, now)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// sessionCookie signs the cookie of c, valid for sessionIdle from now and not past c.Abs.
func (h *Handler) sessionCookie(c jwtClaims, now time.Time) (string, time.Time, error) {
	end := now.Add(h.sessionIdle)
	if abs := time.Unix(c.Abs, 0); h.sessionIdle <= 0 || end.After(abs) {
		end = abs
	}
	c.ExpiresAt = jwt.NewNumericDate(end)
	value, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(h.secret)
	return value, end, err
}

func (h *Handler) setSessionCookie(w http.ResponseWriter, value string, end, now time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: h.cookieName, Value: value, Domain: h.baseDomain, Path: "/",
		Expires: end, MaxAge: int(end.Sub(now).Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// renewSession slides the session of a valid cookie: when less than sessionRenew of it remains and the absolute end
// allows a later expiry, a fresh cookie goes out with the response. An active user costs one renewal per
// sessionRenew at most, and a cookie without an absolute end (older) is left alone.
func (h *Handler) renewSession(w http.ResponseWriter, c jwtClaims) {
	if c.Abs == 0 || c.ExpiresAt == nil || h.sessionRenew <= 0 {
		return
	}
	now := h.now()
	if c.ExpiresAt.Sub(now) >= h.sessionRenew {
		return
	}
	value, end, err := h.sessionCookie(c, now)
	if err != nil || !end.After(c.ExpiresAt.Time) {
		return
	}
	h.setSessionCookie(w, value, end, now)
}
