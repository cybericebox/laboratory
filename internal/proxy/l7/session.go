package l7

import (
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AuthPath is the path of the handoff link the platform gives the participant:
// https://<device>-<labid>.<base>/_auth?t=<jwt>. The proxy consumes it and never
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
	claims, err := verifyHandoff(raw, h.key(), host, h.now)
	if err != nil {
		h.expired(w, r)
		return
	}
	end := time.Unix(claims.Session, 0)
	value, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwtClaims{
		GroupID: claims.GroupID, Client: claims.Client,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(end),
		},
	}).SignedString(h.secret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: h.cookieName, Value: value, Domain: h.baseDomain, Path: "/",
		Expires: end, MaxAge: int(end.Sub(now).Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
