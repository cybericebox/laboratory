package nodeagent

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The start-up check speaks to the gate as the relay does: TLS trusting the gate's authority only, and the node-agent's own token.
func TestCheckRegistrySpeaksTLSWithTheNodeToken(t *testing.T) {
	var token string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.Header.Get("X-Cib-Node-Token")
		w.WriteHeader(http.StatusUnauthorized) // an answer that is not a server error: it is there
	}))
	defer srv.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	tok := filepath.Join(dir, "token")
	if err := os.WriteFile(tok, []byte("node-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(srv.URL, "https://")
	if err := checkRegistry(t.Context(), addr, ca, tok); err != nil || token != "node-token" {
		t.Fatalf("%v %q", err, token)
	}
	if err := checkRegistry(t.Context(), addr, filepath.Join(dir, "none"), tok); err == nil {
		t.Fatal("a missing CA file is a failed check")
	}
	if err := checkRegistry(t.Context(), addr, ca, filepath.Join(dir, "none")); err == nil {
		t.Fatal("a missing token file is a failed check")
	}
}
