package l7

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// challengeClaims mirrors spec §3: { user_id, group_id, exp }. user_id is
// preserved for downstream audit (access logs, abuse correlation) but routing
// uses only group_id — moving a user between groups is by design impossible.
type challengeClaims struct {
	UserID  string `json:"user_id"`
	GroupID string `json:"group_id"`
	Exp     int64  `json:"exp"`
}

// validateCookie reads the challenge cookie, verifies the Ed25519 signature,
// checks expiry, and returns the parsed claims.
// Cookie format: base64(json_payload).base64(ed25519_signature)
func validateCookie(r *http.Request, pubKey ed25519.PublicKey, cookieName string) (challengeClaims, error) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return challengeClaims{}, fmt.Errorf("no %s cookie", cookieName)
	}

	parts := strings.SplitN(cookie.Value, ".", 2)
	if len(parts) != 2 {
		return challengeClaims{}, fmt.Errorf("malformed cookie")
	}
	rawJSON, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return challengeClaims{}, fmt.Errorf("decode payload: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return challengeClaims{}, fmt.Errorf("decode signature: %w", err)
	}
	if !ed25519.Verify(pubKey, rawJSON, sig) {
		return challengeClaims{}, fmt.Errorf("invalid signature")
	}
	var claims challengeClaims
	if err := json.Unmarshal(rawJSON, &claims); err != nil {
		return challengeClaims{}, fmt.Errorf("unmarshal claims: %w", err)
	}
	if time.Now().Unix() >= claims.Exp {
		return challengeClaims{}, fmt.Errorf("token expired")
	}
	if claims.GroupID == "" {
		return challengeClaims{}, fmt.Errorf("empty group_id")
	}
	return claims, nil
}
