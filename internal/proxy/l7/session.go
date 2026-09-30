package l7

import (
	"container/list"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AuthPath is the path of the handoff link the platform gives the participant:
// https://<device>-<labid>.<base>/_auth?t=<jwt>. The proxy consumes it and never
// forwards it to a lab.
const AuthPath = "/_auth"

// replayCache remembers handoff jti values until their exp, best effort per
// replica: a link is meant to work once.
type replayCache struct {
	mu    sync.Mutex
	max   int
	order *list.List // of *replayEntry, oldest first
	byID  map[string]*list.Element
}

type replayEntry struct {
	id      string
	expires time.Time
}

func newReplayCache(max int) *replayCache {
	return &replayCache{max: max, order: list.New(), byID: map[string]*list.Element{}}
}

// firstUse reports whether id was not seen yet and remembers it until expires.
func (c *replayCache) firstUse(id string, expires, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for e := c.order.Front(); e != nil; {
		entry := e.Value.(*replayEntry)
		if entry.expires.After(now) {
			break
		}
		next := e.Next()
		c.order.Remove(e)
		delete(c.byID, entry.id)
		e = next
	}
	if _, seen := c.byID[id]; seen {
		return false
	}
	for c.order.Len() >= c.max {
		oldest := c.order.Front()
		c.order.Remove(oldest)
		delete(c.byID, oldest.Value.(*replayEntry).id)
	}
	c.byID[id] = c.order.PushBack(&replayEntry{id: id, expires: expires})
	return true
}

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
	if !h.replays.firstUse(claims.ID, claims.ExpiresAt.Time, now) {
		h.expired(w, r)
		return
	}
	end := time.Unix(claims.Session, 0)
	value, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwtClaims{
		GroupID: claims.GroupID, Client: claims.Client, Version: SessionVersion,
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
