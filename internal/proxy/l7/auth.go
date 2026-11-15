package l7

import (
	"crypto/ed25519"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultHandoffLifetime bounds exp - iat of a handoff token: it is a one-click
// link, never a session. Mirrors the chart (proxy.l7.accessTokenMaxTTL): a link is replayable for its lifetime, so it is short.
const DefaultHandoffLifetime = 60 * time.Second

// The proxy's own session is sliding: the cookie expires DefaultSessionIdleTTL after the last request, it is
// re-issued only when less than DefaultSessionRenewBefore of it remains (so an active user costs about one
// Set-Cookie per hour), and it never lives past DefaultSessionMaxTTL from the handoff or past the link's sess.
// Mirror the chart (proxy.l7.sessionIdleTTL, sessionRenewBefore, sessionMaxTTL).
const (
	DefaultSessionIdleTTL     = 24 * time.Hour
	DefaultSessionRenewBefore = time.Hour
	DefaultSessionMaxTTL      = 168 * time.Hour
)

// jwtClaims is the proxy's own session cookie (HS256). It carries only what the
// operator knows: the LabGroup and a LabGroupClient of that group.
type jwtClaims struct {
	GroupID string `json:"group_id"`
	// Client is the LabGroupClient of the group the session acts as: the same
	// object that is the participant's VPN peer.
	Client string `json:"client,omitempty"`
	// Tenant is the tenant that issued the handoff; it must stay the owner of the group.
	Tenant string `json:"tenant,omitempty"`
	// Abs is the unix time the session ends at the latest, whatever renewals do (the handoff's start plus the
	// session maximum, never past the link's sess). Absent in older cookies: they are not renewed.
	Abs int64 `json:"abs,omitempty"`
	// Host is the full host name (lower case, no port) of the one device access host the session was opened for. The request must be for
	// exactly this host, renewals included; a cookie without it is refused, so what leaks opens at most one device.
	Host string `json:"host,omitempty"`
	// Use is "console" for a console session token (the token a console page holds, see terminal_console.go) and empty for the cookie of a
	// web session: neither is accepted in the other's place.
	Use string `json:"use,omitempty"`
	jwt.RegisteredClaims
}

func (c jwtClaims) client() string { return c.Client }

// HandoffAudience is the aud a handoff token must carry.
const HandoffAudience = "laboratory-proxy"

// handoffClaims is the token in the /_auth link, signed by a tenant's platform. The standard claims
// are: iss = the tenant (its name), the header kid = the id of the access key that signed it, aud =
// HandoffAudience, sub = the LabGroupClient the session acts as, and iat, nbf and exp (the lifetime
// of the link: about a minute, five at most). The rest is what the operator knows.
type handoffClaims struct {
	GroupID string `json:"group_id"`
	// Host is the FULL host name of the device access host (<device>-<code><letter>.<base domain>) the link was issued for; the proxy requires
	// it to equal the Host of the request, so a link opens one device and nothing else.
	Host string `json:"host"`
	// Session is the unix time the proxy cookie expires.
	Session int64 `json:"sess"`
	jwt.RegisteredClaims
}

// client is the LabGroupClient of the token (sub).
func (c handoffClaims) client() string { return c.Subject }

// KeyLookup finds the access public key a tenant registered under a key id.
type KeyLookup func(tenant, keyID string) (ed25519.PublicKey, bool)

// GroupTenant says which tenant a LabGroup (by the id the platform gave it) belongs to.
type GroupTenant func(groupID string) (tenant string, ok bool)

var validTaskRE = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{0,62}$`)

// validateCookie verifies the proxy's own session cookie.
func validateCookie(r *http.Request, secret []byte, cookieName string, now func() time.Time) (jwtClaims, error) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return jwtClaims{}, fmt.Errorf("no %s cookie", cookieName)
	}
	if len(secret) == 0 {
		return jwtClaims{}, fmt.Errorf("no session secret")
	}
	var claims jwtClaims
	token, err := jwt.ParseWithClaims(
		cookie.Value, &claims, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return secret, nil
		},
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithTimeFunc(now), jwt.WithExpirationRequired(),
	)
	if err != nil {
		return jwtClaims{}, fmt.Errorf("invalid session: %w", err)
	}
	if !token.Valid {
		return jwtClaims{}, fmt.Errorf("invalid session")
	}
	if claims.GroupID == "" {
		return jwtClaims{}, fmt.Errorf("empty group_id")
	}
	if claims.Use != "" {
		return jwtClaims{}, fmt.Errorf("not a web session")
	}
	return claims, nil
}

var tenantNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// accessKeyFunc finds the key a tenant's token is signed with: EdDSA only, the issuer (iss) names the tenant and the header kid the key
// of that tenant (see KeyLookup). The claims of the token must be a handoffClaims.
func accessKeyFunc(keys KeyLookup) jwt.Keyfunc {
	return func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		var issuer string
		switch c := t.Claims.(type) {
		case *handoffClaims:
			issuer = c.Issuer
		}
		if kid == "" || !tenantNameRE.MatchString(issuer) || len(issuer) > 63 {
			return nil, fmt.Errorf("no issuer or key id")
		}
		key, ok := keys(issuer, kid)
		if !ok {
			return nil, fmt.Errorf("unknown issuer or key id")
		}
		return key, nil
	}
}

// verifyHandoff checks a tenant's handoff link: the signature against the key registered for
// (iss, kid), aud, iat/nbf/exp and the link lifetime, that it was issued for exactly this device host (the whole host name), and
// that the LabGroup belongs to the tenant that signed it (a tenant cannot reach another's labs). host is the full host name of the request, without a port, in lower case.
func verifyHandoff(raw string, keys KeyLookup, groupTenant GroupTenant, host string, now func() time.Time, maxLifetime time.Duration) (handoffClaims, error) {
	var claims handoffClaims
	token, err := jwt.ParseWithClaims(
		raw, &claims, accessKeyFunc(keys),
		jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithTimeFunc(now), jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
		jwt.WithAudience(HandoffAudience), jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return handoffClaims{}, fmt.Errorf("invalid handoff: %w", err)
	}
	if !token.Valid {
		return handoffClaims{}, fmt.Errorf("invalid handoff")
	}
	if claims.GroupID == "" || claims.Subject == "" || claims.Host == "" {
		return handoffClaims{}, fmt.Errorf("incomplete handoff")
	}
	if claims.IssuedAt == nil || claims.NotBefore == nil || claims.ExpiresAt == nil {
		return handoffClaims{}, fmt.Errorf("handoff needs iat, nbf and exp")
	}
	if claims.ExpiresAt.Sub(claims.IssuedAt.Time) > maxLifetime {
		return handoffClaims{}, fmt.Errorf("handoff lives too long")
	}
	if claims.Host != host {
		return handoffClaims{}, fmt.Errorf("handoff is for another host")
	}
	if !now().Before(time.Unix(claims.Session, 0)) {
		return handoffClaims{}, fmt.Errorf("session already over")
	}
	if groupTenant == nil {
		return handoffClaims{}, fmt.Errorf("no way to tell the tenant of a group")
	}
	if owner, ok := groupTenant(claims.GroupID); !ok || owner != claims.Issuer {
		return handoffClaims{}, fmt.Errorf("the group does not belong to the issuer")
	}
	return claims, nil
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
