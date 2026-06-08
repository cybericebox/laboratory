package l7

import (
	"crypto/rsa"
	"fmt"
	"net/http"

	"github.com/golang-jwt/jwt/v5"
)

type jwtClaims struct {
	UserID  string `json:"user_id"`
	GroupID string `json:"group_id"`
	jwt.RegisteredClaims
}

func validateCookie(r *http.Request, key func() *rsa.PublicKey, cookieName string) (jwtClaims, error) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return jwtClaims{}, fmt.Errorf("no %s cookie", cookieName)
	}

	pubKey := key()

	var claims jwtClaims
	token, err := jwt.ParseWithClaims(cookie.Value, &claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return pubKey, nil
	})
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
