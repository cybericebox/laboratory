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

// maxHandoffLifetime bounds exp - iat of a handoff token: it is a one-click
// link, never a session.
const maxHandoffLifetime = 5 * time.Minute

// jwtClaims is the proxy's own session cookie (HS256). It carries only what the
// operator knows: the LabGroup and a LabGroupClient of that group.
type jwtClaims struct {
	GroupID string `json:"group_id"`
	// Client is the LabGroupClient of the group the session acts as: the same
	// object that is the participant's VPN peer.
	Client string `json:"client,omitempty"`
	jwt.RegisteredClaims
}

func (c jwtClaims) client() string { return c.Client }

// handoffClaims is the token in the /_auth link, signed by the platform.
// exp is the lifetime of the link (about a minute, five at most); Session is the end of the
// session the proxy cookie gets.
type handoffClaims struct {
	GroupID string `json:"group_id"`
	Client  string `json:"client"`
	// Host is the device host label (<device>-<code>) the link was issued for.
	Host string `json:"host"`
	// Session is the unix time the proxy cookie expires.
	Session int64 `json:"sess"`
	jwt.RegisteredClaims
}

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
	return claims, nil
}

// verifyHandoff checks the platform's signature offline, the link lifetime and
// that the link was issued for this device host.
func verifyHandoff(raw string, key ed25519.PublicKey, host string, now func() time.Time) (handoffClaims, error) {
	var claims handoffClaims
	token, err := jwt.ParseWithClaims(
		raw, &claims, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return key, nil
		},
		jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithTimeFunc(now), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return handoffClaims{}, fmt.Errorf("invalid handoff: %w", err)
	}
	if !token.Valid {
		return handoffClaims{}, fmt.Errorf("invalid handoff")
	}
	if claims.GroupID == "" || claims.Client == "" || claims.Host == "" {
		return handoffClaims{}, fmt.Errorf("incomplete handoff")
	}
	if claims.IssuedAt == nil || claims.ExpiresAt.Sub(claims.IssuedAt.Time) > maxHandoffLifetime {
		return handoffClaims{}, fmt.Errorf("handoff lives too long")
	}
	if claims.Host != host {
		return handoffClaims{}, fmt.Errorf("handoff is for another host")
	}
	if !now().Before(time.Unix(claims.Session, 0)) {
		return handoffClaims{}, fmt.Errorf("session already over")
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
