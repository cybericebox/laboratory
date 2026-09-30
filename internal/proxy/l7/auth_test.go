package l7

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var testSecret = []byte("test-session-secret-0123456789abcdef")

func signCookie(t *testing.T, claims jwtClaims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(testSecret)
	if err != nil {
		t.Fatalf("sign cookie: %v", err)
	}
	return token
}

func requestWithCookie(token string) *http.Request {
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "challenge", Value: token})
	return req
}

func hour() jwt.RegisteredClaims {
	return jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}
}

func TestValidateCookie_Valid(t *testing.T) {
	token := signCookie(t, jwtClaims{GroupID: "abc-123", Client: "p-u-9", RegisteredClaims: hour()})
	claims, err := validateCookie(requestWithCookie(token), testSecret, "challenge", time.Now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claims.GroupID != "abc-123" || claims.client() != "p-u-9" {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestValidateCookie_Expired(t *testing.T) {
	token := signCookie(t, jwtClaims{GroupID: "abc-123", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour))}})
	if _, err := validateCookie(requestWithCookie(token), testSecret, "challenge", time.Now); err == nil {
		t.Fatal("expected error for expired cookie")
	}
}

func TestValidateCookie_BadSecret(t *testing.T) {
	token := signCookie(t, jwtClaims{GroupID: "abc-123", RegisteredClaims: hour()})
	if _, err := validateCookie(requestWithCookie(token), []byte("another-secret"), "challenge", time.Now); err == nil {
		t.Fatal("expected error for a cookie signed with another secret")
	}
}

func TestValidateCookie_EmptyGroupID(t *testing.T) {
	token := signCookie(t, jwtClaims{Client: "p-u-9", RegisteredClaims: hour()})
	if _, err := validateCookie(requestWithCookie(token), testSecret, "challenge", time.Now); err == nil {
		t.Fatal("expected error for empty group_id")
	}
}

// The platform's RS256 handoff token is not a session: the cookie takes HMAC only.
func TestValidateCookie_RejectsAnRSAToken(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwtClaims{GroupID: "abc-123", RegisteredClaims: hour()}).SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateCookie(requestWithCookie(token), testSecret, "challenge", time.Now); err == nil {
		t.Fatal("expected error for a non-HMAC cookie")
	}
}

func TestValidateCookie_NoSecretRefusesEverything(t *testing.T) {
	token := signCookie(t, jwtClaims{GroupID: "abc-123", RegisteredClaims: hour()})
	if _, err := validateCookie(requestWithCookie(token), nil, "challenge", time.Now); err == nil {
		t.Fatal("expected error without a secret")
	}
}
