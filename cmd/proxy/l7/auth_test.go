package l7

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidateCookie_Valid(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	payload := map[string]interface{}{"group_id": "abc-123", "exp": time.Now().Add(time.Hour).Unix()}
	raw, _ := json.Marshal(payload)
	sig := ed25519.Sign(priv, raw)
	token := base64.StdEncoding.EncodeToString(raw) + "." + base64.StdEncoding.EncodeToString(sig)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "challenge", Value: token})
	groupID, err := validateCookie(req, pub, "challenge")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if groupID != "abc-123" {
		t.Fatalf("expected abc-123, got %s", groupID)
	}
}

func TestValidateCookie_Expired(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	payload := map[string]interface{}{"group_id": "abc-123", "exp": time.Now().Add(-time.Hour).Unix()}
	raw, _ := json.Marshal(payload)
	sig := ed25519.Sign(priv, raw)
	token := base64.StdEncoding.EncodeToString(raw) + "." + base64.StdEncoding.EncodeToString(sig)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "challenge", Value: token})
	_, err := validateCookie(req, pub, "challenge")
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestValidateCookie_BadSig(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, priv2, _ := ed25519.GenerateKey(rand.Reader)
	payload := map[string]interface{}{"group_id": "abc-123", "exp": time.Now().Add(time.Hour).Unix()}
	raw, _ := json.Marshal(payload)
	sig := ed25519.Sign(priv2, raw)
	token := base64.StdEncoding.EncodeToString(raw) + "." + base64.StdEncoding.EncodeToString(sig)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "challenge", Value: token})
	_, err := validateCookie(req, pub, "challenge")
	if err == nil {
		t.Fatal("expected error for bad signature")
	}
}
