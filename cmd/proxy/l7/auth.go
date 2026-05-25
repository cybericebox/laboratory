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

type challengeClaims struct {
	GroupID string `json:"group_id"`
	Exp     int64  `json:"exp"`
}

// validateCookie reads the challenge cookie, verifies the Ed25519 signature,
// checks expiry, and returns the group_id.
// Cookie format: base64(json_payload).base64(ed25519_signature)
func validateCookie(r *http.Request, pubKey ed25519.PublicKey, cookieName string) (groupID string, err error) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return "", fmt.Errorf("no %s cookie", cookieName)
	}

	parts := strings.SplitN(cookie.Value, ".", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("malformed cookie")
	}
	rawJSON, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("decode payload: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode signature: %w", err)
	}
	if !ed25519.Verify(pubKey, rawJSON, sig) {
		return "", fmt.Errorf("invalid signature")
	}
	var claims challengeClaims
	if err := json.Unmarshal(rawJSON, &claims); err != nil {
		return "", fmt.Errorf("unmarshal claims: %w", err)
	}
	if time.Now().Unix() > claims.Exp {
		return "", fmt.Errorf("token expired")
	}
	if claims.GroupID == "" {
		return "", fmt.Errorf("empty group_id")
	}
	return claims.GroupID, nil
}
