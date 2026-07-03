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

func signToken(t *testing.T, priv *rsa.PrivateKey, claims jwtClaims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(priv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return token
}

func requestWithCookie(token string) *http.Request {
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "challenge", Value: token})
	return req
}

func TestValidateCookie_Valid(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	token := signToken(t, priv, jwtClaims{
		UserID:  "u-9",
		GroupID: "abc-123",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	claims, err := validateCookie(requestWithCookie(token), func() *rsa.PublicKey { return &priv.PublicKey }, "challenge")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claims.GroupID != "abc-123" {
		t.Fatalf("expected GroupID abc-123, got %s", claims.GroupID)
	}
	if claims.UserID != "u-9" {
		t.Fatalf("expected UserID u-9, got %s", claims.UserID)
	}
}

func TestValidateCookie_Expired(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	token := signToken(t, priv, jwtClaims{
		GroupID: "abc-123",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
	})
	if _, err := validateCookie(requestWithCookie(token), func() *rsa.PublicKey { return &priv.PublicKey }, "challenge"); err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestValidateCookie_BadSig(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	token := signToken(t, other, jwtClaims{
		GroupID: "abc-123",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	if _, err := validateCookie(requestWithCookie(token), func() *rsa.PublicKey { return &priv.PublicKey }, "challenge"); err == nil {
		t.Fatal("expected error for bad signature")
	}
}

func TestValidateCookie_EmptyGroupID(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	token := signToken(t, priv, jwtClaims{
		UserID: "u-9",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	if _, err := validateCookie(requestWithCookie(token), func() *rsa.PublicKey { return &priv.PublicKey }, "challenge"); err == nil {
		t.Fatal("expected error for empty group_id")
	}
}

func TestValidateCookie_WrongAlg(t *testing.T) {
	// HS256 token signed with a shared secret must be rejected: only RSA allowed.
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	claims := jwtClaims{
		GroupID: "abc-123",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("secret"))
	if err != nil {
		t.Fatalf("sign HS256 token: %v", err)
	}
	if _, err := validateCookie(requestWithCookie(token), func() *rsa.PublicKey { return &priv.PublicKey }, "challenge"); err == nil {
		t.Fatal("expected error for non-RSA signing method")
	}
}
