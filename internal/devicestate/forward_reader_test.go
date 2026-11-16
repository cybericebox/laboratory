package devicestate

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The reader account is added to the reads of the snapshot repositories and the base repository, and to nothing else.
func TestReaderHandlerAddsTheReaderOnlyWhereItBelongs(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.Method
		_, _ = w.Write([]byte("ok"))
	}))
	defer up.Close()
	h := readerHandler(strings.TrimPrefix(up.URL, "http://"), forwardOptions{readerUser: "reader", readerPassword: "pw"})

	do := func(method, path, auth string) string {
		req := httptest.NewRequest(method, path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || gotPath != path || gotMethod != method {
			t.Fatalf("%s %s relayed as %s %s: %d", method, path, gotMethod, gotPath, rec.Code)
		}
		return gotAuth
	}
	reader := "Basic cmVhZGVyOnB3" // reader:pw
	for _, p := range []string{"/v2/lab/ns/lab/dev/manifests/sha256:abc", "/v2/lab/ns/lab/dev/blobs/sha256:abc", "/v2/base/manifests/latest", "/v2/base/blobs/sha256:x", "/v2/platform/tools/manifests/sha256:abc", "/v2/platform/tools/blobs/sha256:abc"} {
		for _, m := range []string{"GET", "HEAD"} {
			if got := do(m, p, ""); got != reader {
				t.Errorf("%s %s: Authorization = %q, want the reader", m, p, got)
			}
		}
	}
	// the public image cache stays anonymous
	for _, p := range []string{"/v2/docker.io/library/nginx/manifests/1", "/v2/ghcr.io/o/app/blobs/sha256:a", "/v2/", "/v2/_catalog"} {
		if got := do("GET", p, ""); got != "" {
			t.Errorf("%s must not get credentials: %q", p, got)
		}
	}
	// writes never get the reader, and credentials the caller brought are left alone (the snapshot pusher is the writer)
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, p := range []string{"/v2/lab/ns/lab/dev/blobs/uploads/", "/v2/platform/tools/blobs/uploads/", "/v2/platform/tools/manifests/sha256:abc"} {
			if got := do(m, p, ""); got != "" {
				t.Errorf("%s %s must not get the reader: %q", m, p, got)
			}
		}
	}
	if got := do("GET", "/v2/lab/ns/lab/dev/manifests/x", "Basic d3JpdGVyOnc="); got != "Basic d3JpdGVyOnc=" {
		t.Errorf("the caller's own credentials stay: %q", got)
	}
	// a path that only starts like a snapshot repository is not one
	if got := do("GET", "/v2/labx/y/manifests/1", ""); got != "" {
		t.Errorf("labx is not lab: %q", got)
	}
	if got := do("GET", "/v2/platformx/y/manifests/1", ""); got != "" {
		t.Errorf("platformx is not platform: %q", got)
	}
}

// The reader goes only to requests that carry the exact Host the runtime pulls from, and never to paths that climb.
func TestReaderHandlerNeedsTheExactHost(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotAuth = r.Header.Get("Authorization") }))
	defer up.Close()
	h := readerHandler(strings.TrimPrefix(up.URL, "http://"), forwardOptions{readerUser: "reader", readerPassword: "pw", host: "localhost:5035"})
	for host, want := range map[string]bool{"localhost:5035": true, "LOCALHOST:5035": true, "127.0.0.1:5035": false, "localhost:5036": false, "evil.example": false, "localhost": false} {
		gotAuth = ""
		req := httptest.NewRequest("GET", "/v2/lab/ns/lab/dev/manifests/x", nil)
		req.Host = host
		h.ServeHTTP(httptest.NewRecorder(), req)
		if (gotAuth != "") != want {
			t.Errorf("Host %q: reader added = %v, want %v", host, gotAuth != "", want)
		}
	}
	for _, p := range []string{"/v2/lab/../docker.io/x/manifests/1", "/v2/lab//x"} {
		gotAuth = ""
		req := httptest.NewRequest("GET", p, nil)
		req.Host = "localhost:5035"
		h.ServeHTTP(httptest.NewRecorder(), req)
		if gotAuth != "" {
			t.Errorf("%s must not get the reader", p)
		}
	}
}

// With a certificate authority the relay speaks TLS to the registry and trusts only that authority; the pull's own credentials
// reach the registry, and a server of another authority is refused.
func TestForwardOverTLS(t *testing.T) {
	var gotAuth, gotHost string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotHost = r.Header.Get("Authorization"), r.Host
		_, _ = w.Write([]byte("ok"))
	}))
	defer up.Close()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: up.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	target := strings.TrimPrefix(up.URL, "https://")
	// the test server's certificate is for 127.0.0.1 and example.com: the target is dialled by that address
	o := forwardOptions{tlsCAFile: caFile}
	if err := o.loadTLS(target); err != nil {
		t.Fatal(err)
	}
	h := readerHandler(target, o)
	req := httptest.NewRequest("GET", "/v2/c/ghcr.io/o/app/manifests/sha256:abc", nil)
	req.Header.Set("Authorization", "Basic dDF+YWxpY2U6cHc=")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || gotAuth != "Basic dDF+YWxpY2U6cHc=" || gotHost != target {
		t.Fatalf("%d auth %q host %q", rec.Code, gotAuth, gotHost)
	}
	// a relay with no reader adds none, even to a snapshot path
	gotAuth = ""
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v2/lab/a/b/manifests/x", nil))
	if gotAuth != "" {
		t.Fatalf("no reader configured, none added: %q", gotAuth)
	}

	// a server of another authority is refused
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotAuth = "reached" }))
	other.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	other.StartTLS()
	defer other.Close()
	gotAuth = ""
	h2 := readerHandler(strings.TrimPrefix(other.URL, "https://"), o)
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest("GET", "/v2/c/a/b/manifests/x", nil))
	if rec.Code != http.StatusBadGateway || gotAuth != "" {
		t.Fatalf("an untrusted server must not be reached: %d %q", rec.Code, gotAuth)
	}
	if err := (&forwardOptions{tlsCAFile: filepath.Join(t.TempDir(), "none")}).loadTLS("x:1"); err == nil {
		t.Fatal("a missing CA file is an error")
	}
	bad := filepath.Join(t.TempDir(), "bad.crt")
	_ = os.WriteFile(bad, []byte("not pem"), 0o600)
	if err := (&forwardOptions{tlsCAFile: bad}).loadTLS("x:1"); err == nil {
		t.Fatal("a CA file with no certificate is an error")
	}
}

// selfSigned is a certificate for 127.0.0.1 of an authority the test's relay does not trust.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "other"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
