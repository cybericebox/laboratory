package imagecache

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTokenFileIsReread(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tf := &TokenFile{Path: path, Now: func() time.Time { return now }}
	if got, err := tf.Token(); err != nil || got != "one" {
		t.Fatalf("%q %v", got, err)
	}
	if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(TokenReread - time.Second)
	if got, _ := tf.Token(); got != "one" {
		t.Fatalf("read again too early: %q", got)
	}
	now = now.Add(2 * time.Second)
	if got, _ := tf.Token(); got != "two" {
		t.Fatalf("the rotated token is not picked up: %q", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	now = now.Add(TokenReread)
	if _, err := tf.Token(); err == nil {
		t.Fatal("a missing token file is an error, never an empty token")
	}
}

func TestRoundTripperSetsTheTokenAndStripsClientHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("X-Cib-Node-Token", "forged")
	req.Header.Set("x-cib-other", "1")
	resp, err := NewTokenFile(path).RoundTripper(nil).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got.Get(HeaderNodeToken) != "tok" || got.Get("X-Cib-Other") != "" {
		t.Fatalf("%v", got)
	}
	if req.Header.Get("X-Cib-Node-Token") != "forged" {
		t.Fatal("the caller's request must not be changed")
	}
}
