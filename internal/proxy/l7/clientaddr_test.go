package l7

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// No address of a client reaches a task device, whatever the client sends and whether or not the proxy would add its own: every
// header that carries one is dropped, X-Forwarded-For is not added from the peer address, X-Forwarded-Proto stays. Also for a request
// that asks for an upgrade (WebSocket).
func TestHandler_NoClientAddressReachesTheDevice(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	var got http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(200)
	}))
	defer backend.Close()
	h := NewHandler(staticKeys("acme", "k1", pub), testSecret, "challenges.example.com", "challenge",
		func(task, groupID string) (string, error) { return backend.URL, nil })

	for name, upgrade := range map[string]bool{"plain": false, "upgrade": true} {
		t.Run(name, func(t *testing.T) {
			got = nil
			req := httptest.NewRequest("GET", "http://mytask.challenges.example.com/path", nil)
			req.Host = "mytask.challenges.example.com"
			req.RemoteAddr = "198.51.100.7:4242"
			req.AddCookie(&http.Cookie{Name: "challenge", Value: signCookie(t, jwtClaims{GroupID: "grp1", Client: "p-u1", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})})
			for _, hdr := range clientAddressHeaders {
				req.Header.Set(hdr, "203.0.113.9")
			}
			req.Header.Set("X-Forwarded-For", "203.0.113.9, 203.0.113.10")
			if upgrade {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 || got == nil {
				t.Fatalf("the request must reach the device: %d", rec.Code)
			}
			for _, hdr := range append([]string{"X-Real-IP"}, clientAddressHeaders...) {
				if v := got.Values(hdr); len(v) != 0 {
					t.Errorf("%s reached the device: %v", hdr, v)
				}
			}
			if got.Get("X-Forwarded-Proto") != "https" {
				t.Errorf("X-Forwarded-Proto = %q", got.Get("X-Forwarded-Proto"))
			}
		})
	}
}
