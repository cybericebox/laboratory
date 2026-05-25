package l7

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func makeToken(t *testing.T, priv ed25519.PrivateKey, groupID string) string {
	t.Helper()
	payload := map[string]interface{}{"group_id": groupID, "exp": time.Now().Add(time.Hour).Unix()}
	raw, _ := json.Marshal(payload)
	sig := ed25519.Sign(priv, raw)
	return base64.StdEncoding.EncodeToString(raw) + "." + base64.StdEncoding.EncodeToString(sig)
}

func TestHandler_StripsCookie(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	var gotCookieHeader string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookieHeader = r.Header.Get("Cookie")
		w.WriteHeader(200)
	}))
	defer backend.Close()

	resolver := func(task, groupID string) (string, error) {
		return backend.URL, nil
	}

	h := NewHandler(pub, "challenges.example.com", "challenge", resolver)

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
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	h := NewHandler(pub, "challenges.example.com", "challenge", nil)

	req := httptest.NewRequest("GET", "http://evil.com/", nil)
	req.Host = "evil.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}
