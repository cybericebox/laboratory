package l7

import (
	"crypto/rsa"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

type jwtClaims struct {
	GroupID string `json:"group_id"`
	// Client is the LabGroupClient of the group this token acts as: the same
	// object that is the participant's VPN peer. The proxy knows nothing else
	// about who the holder is.
	Client  string `json:"client,omitempty"`
	Version int    `json:"ver,omitempty"`
	// UserID and RegisteredClaims.Subject belong to version 2 tokens (a user id,
	// the client being "p-<user id>"); they are only read for compatibility.
	UserID string `json:"user_id,omitempty"`
	jwt.RegisteredClaims
}

// client is the LabGroupClient the token acts as; empty for a legacy token.
func (c jwtClaims) client() string {
	if c.Client != "" {
		return c.Client
	}
	if c.Subject != "" {
		return ClientName(c.Subject)
	}
	if c.UserID != "" {
		return ClientName(c.UserID)
	}
	return ""
}

var validTaskRE = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{0,62}$`)

func validateCookie(r *http.Request, key func() *rsa.PublicKey, cookieName string) (jwtClaims, error) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return jwtClaims{}, fmt.Errorf("no %s cookie", cookieName)
	}

	pubKey := key()

	var claims jwtClaims
	token, err := jwt.ParseWithClaims(
		cookie.Value, &claims, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return pubKey, nil
		},
	)
	if err != nil {
		return jwtClaims{}, fmt.Errorf("invalid token: %w", err)
	}
	if !token.Valid {
		return jwtClaims{}, fmt.Errorf("invalid token")
	}
	if claims.GroupID == "" {
		return jwtClaims{}, fmt.Errorf("empty group_id")
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
