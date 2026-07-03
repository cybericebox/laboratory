package l7

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func makeToken(t *testing.T, priv *rsa.PrivateKey, groupID string) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwtClaims{
		GroupID: groupID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString(priv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return token
}

func TestHandler_StripsCookie(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)

	var gotCookieHeader string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookieHeader = r.Header.Get("Cookie")
		w.WriteHeader(200)
	}))
	defer backend.Close()

	resolver := func(task, groupID string) (string, error) {
		return backend.URL, nil
	}

	h := NewHandler(func() *rsa.PublicKey { return &priv.PublicKey }, "challenges.example.com", "challenge", resolver)

	req := httptest.NewRequest("GET", "http://mytask.challenges.example.com/path", nil)
	req.Host = "mytask.challenges.example.com"
	req.AddCookie(&http.Cookie{Name: "challenge", Value: makeToken(t, priv, "grp1")})
	req.AddCookie(&http.Cookie{Name: "other", Value: "keep"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if strings.Contains(gotCookieHeader, "challenge=") {
		t.Fatalf("challenge cookie was NOT stripped: %s", gotCookieHeader)
	}
	if !strings.Contains(gotCookieHeader, "other=keep") {
		t.Fatalf("other cookie was stripped: %s", gotCookieHeader)
	}
}

func TestHandler_InvalidHost(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	h := NewHandler(func() *rsa.PublicKey { return &priv.PublicKey }, "challenges.example.com", "challenge", nil)

	req := httptest.NewRequest("GET", "http://evil.com/", nil)
	req.Host = "evil.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}
