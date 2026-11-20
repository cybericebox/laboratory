package l7

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cybericebox/laboratory/internal/grant"
	"github.com/cybericebox/laboratory/internal/names"
)

// The proxy has no credential of its own. What a browser holds for a device host is an access grant: a JWS the tenant's platform signed
// for exactly that host (internal/grant). It arrives once, in a POST of the form the platform page submits (see session.go), and on a web
// host it is then kept as the host-only cookie the proxy reads on every request. Nothing is signed by the proxy; a key it could lose does
// not exist.

// KeyLookup finds the access public key a tenant registered under a key id.
type KeyLookup = grant.KeyLookup

// GroupTenant says which tenant a LabGroup (by the id the platform gave it) belongs to.
type GroupTenant func(groupID string) (tenant string, ok bool)

// sessionGrant is a verified grant together with the raw token it came from (the console forwards the raw token to the device access
// service, which verifies it again).
type sessionGrant struct {
	grant.Claims
	raw string
}

// client is the LabGroupClient of the grant (sub).
func (g sessionGrant) client() string { return g.Subject }

var validTaskRE = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{0,62}$`)

// kindOfHost is the grant kind a request host can be the host of: a web host (no kind letter, or the web letter) takes a web grant, a
// console host a console grant. Another kind (vnc, rdp, a reserved letter) takes none.
func kindOfHost(task string) (string, bool) {
	switch _, _, kind := names.ParseAccessHost(task); kind {
	case "", names.AccessKindHTTP:
		return grant.KindWeb, true
	case names.AccessKindTerminal:
		return grant.KindConsole, true
	}
	return "", false
}

// verifyGrant checks a grant for the host of this request: the signature against the key registered for (iss, kid), aud, iat/nbf/exp and the
// lifetime bound, and that it was issued for exactly this host (the whole host name) and for the kind the host is. host is the full host name
// of the request, without a port, lower case.
func (h *Handler) verifyGrant(raw, task, host string) (sessionGrant, error) {
	kind, ok := kindOfHost(task)
	if !ok {
		return sessionGrant{}, fmt.Errorf("%w: not a host a grant opens", grant.ErrInvalid)
	}
	claims, err := grant.Verify(raw, grant.Options{
		Keys: h.keys, Now: h.now, MaxTTL: h.grantMax, Audience: grant.AudienceProxy, Kind: kind, Host: host,
	})
	if err != nil {
		return sessionGrant{}, err
	}
	return sessionGrant{Claims: claims, raw: raw}, nil
}

// verifyLogin is verifyGrant for a grant that is being posted: it also requires that the LabGroup belongs to the tenant that signed it (a
// tenant cannot reach another's labs). A request that carries the grant afterwards (the cookie, the console socket) is checked against the group
// by the handler on every request, so the cache never holds an ownership.
func (h *Handler) verifyLogin(raw, task, host string) (sessionGrant, error) {
	g, err := h.verifyGrant(raw, task, host)
	if err != nil {
		return sessionGrant{}, err
	}
	if h.groupTenant == nil {
		return sessionGrant{}, fmt.Errorf("%w: no way to tell the tenant of a group", grant.ErrInvalid)
	}
	if owner, ok := h.groupTenant(g.Group); !ok || owner != g.Issuer {
		return sessionGrant{}, fmt.Errorf("%w: the group does not belong to the issuer", grant.ErrInvalid)
	}
	return g, nil
}

// grantCacheTTL is how long a verified cookie is trusted before it is verified again, and grantCacheMax the entries kept. An Ed25519
// check on every request would use much of a replica's CPU at the measured request rate, the HS256 check it replaced cost almost
// nothing. The price is that a key a tenant removed stops working after at most this long.
const (
	grantCacheTTL = 30 * time.Second
	grantCacheMax = 10000
)

// grantCache remembers verified web grants by the hash of (host, raw grant), for min(grantCacheTTL, time left of the grant).
type grantCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]cachedGrant
}

type cachedGrant struct {
	claims  sessionGrant
	expires time.Time
}

func newGrantCache() *grantCache { return &grantCache{entries: map[[sha256.Size]byte]cachedGrant{}} }

func grantCacheKey(host, raw string) [sha256.Size]byte {
	return sha256.Sum256([]byte(host + "\x00" + raw))
}

func (c *grantCache) get(key [sha256.Size]byte, now time.Time) (sessionGrant, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return sessionGrant{}, false
	}
	if !now.Before(e.expires) {
		delete(c.entries, key)
		return sessionGrant{}, false
	}
	return e.claims, true
}

// put stores a verified grant. A full cache drops what is expired first and, when still full, stores nothing: the bound holds and the
// request just pays for a verification.
func (c *grantCache) put(key [sha256.Size]byte, g sessionGrant, now time.Time) {
	expires := now.Add(grantCacheTTL)
	if end := g.End(); end.Before(expires) {
		expires = end
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= grantCacheMax {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= grantCacheMax {
			return
		}
	}
	c.entries[key] = cachedGrant{claims: g, expires: expires}
}

// cookieGrant reads the grant cookie of a request on a web host and verifies it for that host, through the cache. The group and policy
// checks of the request still run after it.
func (h *Handler) cookieGrant(r *http.Request, task string) (sessionGrant, error) {
	cookie, err := r.Cookie(h.cookieName)
	if err != nil || cookie.Value == "" {
		return sessionGrant{}, fmt.Errorf("no %s cookie", h.cookieName)
	}
	host := requestHost(r)
	now := h.now()
	key := grantCacheKey(host, cookie.Value)
	if g, ok := h.grants.get(key, now); ok {
		return g, nil
	}
	g, err := h.verifyGrant(cookie.Value, task, host)
	if err != nil {
		return sessionGrant{}, err
	}
	if g.Kind != grant.KindWeb {
		return sessionGrant{}, fmt.Errorf("%w: not a web grant", grant.ErrInvalid)
	}
	h.grants.put(key, g, now)
	return g, nil
}

func getTaskName(r *http.Request, baseDomain string) (string, error) {
	host := r.Host
	if colonIdx := strings.IndexByte(host, ':'); colonIdx >= 0 {
		host = host[:colonIdx]
	}
	suffix := "." + baseDomain
	if !strings.HasSuffix(host, suffix) {
		return "", fmt.Errorf("invalid host")
	}
	task := strings.TrimSuffix(host, suffix)
	if !validTaskRE.MatchString(task) {
		return "", fmt.Errorf("invalid task: %s", task)
	}
	return task, nil
}
