package devicestate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/cybericebox/laboratory/internal/imagecache"
)

// seen is what the fake gate received.
type seen struct {
	mu      sync.Mutex
	headers []http.Header
	methods []string
	paths   []string
	queries []string
}

func (s *seen) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.headers, s.methods, s.paths = append(s.headers, r.Header.Clone()), append(s.methods, r.Method), append(s.paths, r.URL.Path)
		s.queries = append(s.queries, r.URL.RawQuery)
		s.mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	})
}

func (s *seen) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.paths)
}

// shortDir is a temporary directory with a short path: the path of a unix socket is limited to about a hundred bytes.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "cibrelay")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func writeToken(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func handlerFor(t *testing.T, s *seen, allow rule) (http.Handler, *imagecache.TokenFile) {
	t.Helper()
	up := httptest.NewServer(s.handler())
	t.Cleanup(up.Close)
	tf := imagecache.NewTokenFile(writeToken(t, "node-token\n"))
	return relayHandler("http", strings.TrimPrefix(up.URL, "http://"), http.DefaultTransport, tf, allow, nil), tf
}

func do(h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Every request gets the node's token; whatever the client sent as a platform header or as Authorization is gone.
func TestRelaySetsTheNodeTokenAndRemovesWhatTheClientSent(t *testing.T) {
	var s seen
	h, _ := handlerFor(t, &s, pullAllowed)
	rec := do(h, "GET", "/v2/c/acme/docker.io/library/nginx/manifests/sha256:abc", map[string]string{
		"Authorization":    "Basic dDF+YWxpY2U6cHc=",
		"X-Cib-Node-Token": "forged",
		"x-cib-node":       "other-node",
		"X-Cib-Tenant":     "victim",
		"Accept":           "application/vnd.oci.image.manifest.v1+json",
	})
	if rec.Code != 200 || s.count() != 1 {
		t.Fatalf("%d %d", rec.Code, s.count())
	}
	got := s.headers[0]
	if got.Get("X-Cib-Node-Token") != "node-token" {
		t.Errorf("the token is the node's: %q", got.Get("X-Cib-Node-Token"))
	}
	if got.Get("Authorization") != "" || got.Get("X-Cib-Node") != "" || got.Get("X-Cib-Tenant") != "" {
		t.Errorf("client headers must not pass: %v", got)
	}
	if got.Get("Accept") == "" {
		t.Error("ordinary headers pass")
	}
	if got.Get("X-Forwarded-For") != "" {
		t.Errorf("the relay adds no forwarding header: %q", got.Get("X-Forwarded-For"))
	}
}

// The token is read again: a rotated file is picked up, an unreadable one is a 503 and nothing is forwarded.
func TestRelayTokenIsReread(t *testing.T) {
	var s seen
	up := httptest.NewServer(s.handler())
	defer up.Close()
	file := writeToken(t, "one")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tf := &imagecache.TokenFile{Path: file, Now: func() time.Time { return now }}
	h := relayHandler("http", strings.TrimPrefix(up.URL, "http://"), http.DefaultTransport, tf, pullAllowed, nil)
	if rec := do(h, "GET", "/v2/", nil); rec.Code != 200 || s.headers[0].Get("X-Cib-Node-Token") != "one" {
		t.Fatalf("%d", rec.Code)
	}
	if err := os.WriteFile(file, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(imagecache.TokenReread + time.Second)
	if rec := do(h, "GET", "/v2/", nil); rec.Code != 200 || s.headers[1].Get("X-Cib-Node-Token") != "two" {
		t.Fatalf("%d", rec.Code)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	now = now.Add(imagecache.TokenReread + time.Second)
	if rec := do(h, "GET", "/v2/", nil); rec.Code != http.StatusServiceUnavailable || s.count() != 2 {
		t.Fatalf("no token, no request: %d %d", rec.Code, s.count())
	}
}

// The TCP listener is for pulls: every method but GET and HEAD, and every path outside /v2/, /v2/c/** and /v2/lab/**, is refused with 405
// and never reaches the gate.
func TestPullListenerRefusesWhatIsNotAPull(t *testing.T) {
	var s seen
	h, _ := handlerFor(t, &s, pullAllowed)
	for _, ok := range []string{"/v2/", "/v2/c/acme/docker.io/library/nginx/manifests/latest", "/v2/lab/ns/lab/dev/manifests/latest", "/v2/lab/ns/lab/dev/blobs/sha256:abc"} {
		for _, m := range []string{"GET", "HEAD"} {
			if rec := do(h, m, ok, nil); rec.Code != 200 {
				t.Errorf("%s %s: %d", m, ok, rec.Code)
			}
		}
	}
	before := s.count()
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT"} {
		if rec := do(h, m, "/v2/lab/ns/lab/dev/blobs/uploads/", nil); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d", m, rec.Code)
		}
		if rec := do(h, m, "/v2/c/acme/docker.io/library/nginx/manifests/latest", nil); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s on the cache: %d", m, rec.Code)
		}
	}
	for _, p := range []string{
		"/v2/_catalog", "/v2/base/x/manifests/latest", "/v2/platform/x/manifests/latest", "/v2/lab", "/v2/cx/y", "/", "/metrics", "/healthz",
		"/v2/c/../_catalog", "/v2/c/acme/../../_catalog", "/v2//_catalog", "/v2/c/%2e%2e/_catalog", "/v2/lab/%2e%2e/_catalog", "/v2/c/a%2fb/manifests/x", "/v2/c/./x",
	} {
		if rec := do(h, "GET", p, nil); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: %d", p, rec.Code)
		}
	}
	if s.count() != before {
		t.Errorf("a refused request reached the gate: %d new", s.count()-before)
	}
}

// The push rule: writes only under one device's repository; no DELETE, no other repository, no catalog.
func TestPushRule(t *testing.T) {
	var s seen
	h, _ := handlerFor(t, &s, pushAllowed)
	for _, m := range []string{"POST", "PATCH", "PUT", "GET", "HEAD"} {
		for _, p := range []string{"/v2/lab/ns/lab/dev/blobs/uploads/", "/v2/lab/ns/lab/dev/manifests/latest", "/v2/lab/ns/lab/dev/blobs/sha256:abc"} {
			if rec := do(h, m, p, nil); rec.Code != 200 {
				t.Errorf("%s %s: %d", m, p, rec.Code)
			}
		}
	}
	before := s.count()
	for _, p := range []string{"/v2/lab/ns/lab/dev/manifests/latest", "/v2/lab/ns/lab/dev/blobs/sha256:abc"} {
		if rec := do(h, "DELETE", p, nil); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("DELETE %s: %d", p, rec.Code)
		}
	}
	for _, p := range []string{
		"/v2/", "/v2/_catalog", "/v2/c/acme/docker.io/library/nginx/manifests/latest", "/v2/base/blobs/uploads/", "/v2/lab/ns/lab/dev", "/v2/lab/ns/lab/dev/",
		"/v2/lab/ns/lab", "/v2/lab/ns/../other/lab/dev/manifests/x", "/v2/lab//lab/dev/manifests/x",
	} {
		if rec := do(h, "POST", p, nil); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d", p, rec.Code)
		}
	}
	if s.count() != before {
		t.Errorf("a refused request reached the gate")
	}
}

// Forward serves both listeners: the pull rule on TCP, and on the unix socket (mode 0600) the push rule, with the token on each. The
// client dials the socket and keeps the logical host localhost:5035.
func TestForwardServesTheTCPPortAndThePushSocket(t *testing.T) {
	var s seen
	up := httptest.NewServer(s.handler())
	defer up.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := ln.Addr().String()
	_ = ln.Close()
	sock := filepath.Join(shortDir(t), "run", "push.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Forward(ctx, listen, strings.TrimPrefix(up.URL, "http://"), logr.Discard(), WithNodeToken(writeToken(t, "tok")), withClearTokenForTests(), WithPushSocket(sock, -1))
	}()
	waitFor(t, func() bool { _, err := os.Stat(sock); return err == nil })
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("the push socket must be a socket of mode 0600: %v", fi.Mode())
	}
	tcp := &http.Client{Timeout: 5 * time.Second}
	unix := &http.Client{Timeout: 5 * time.Second, Transport: UnixTransport(sock)}
	status := func(c *http.Client, method, url string) int {
		t.Helper()
		var resp *http.Response
		for i := 0; i < 50; i++ {
			req, _ := http.NewRequest(method, url, nil)
			resp, err = c.Do(req)
			if err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if c := status(tcp, "GET", "http://"+listen+"/v2/c/acme/docker.io/library/nginx/manifests/latest"); c != 200 {
		t.Errorf("a pull on the TCP port: %d", c)
	}
	if c := status(tcp, "PUT", "http://"+listen+"/v2/lab/ns/lab/dev/manifests/latest"); c != http.StatusMethodNotAllowed {
		t.Errorf("a push on the TCP port must be refused: %d", c)
	}
	if c := status(unix, "PUT", "http://localhost:5035/v2/lab/ns/lab/dev/manifests/latest"); c != 200 {
		t.Errorf("a push on the socket: %d", c)
	}
	if c := status(unix, "DELETE", "http://localhost:5035/v2/lab/ns/lab/dev/manifests/latest"); c != http.StatusMethodNotAllowed {
		t.Errorf("DELETE on the socket must be refused: %d", c)
	}
	if c := status(unix, "GET", "http://localhost:5035/v2/c/acme/docker.io/library/nginx/manifests/latest"); c != http.StatusMethodNotAllowed {
		t.Errorf("a pull of the cache on the push socket is not what it is for: %d", c)
	}
	s.mu.Lock()
	for _, h := range s.headers {
		if h.Get("X-Cib-Node-Token") != "tok" {
			t.Errorf("every forwarded request carries the token: %v", h)
		}
	}
	s.mu.Unlock()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Forward does not return when its context ends")
	}
	if _, err := os.Stat(sock); err == nil {
		t.Error("the socket is removed when the relay stops")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func withClearTokenForTests() ForwardOption {
	return func(o *forwardOptions) { o.allowClearToken = true }
}

// A token never travels over plain http: without a certificate authority the relay does not start, unless it is the explicit development
// setup, which sends no token at all.
func TestForwardRefusesATokenWithoutTLS(t *testing.T) {
	err := Forward(context.Background(), "127.0.0.1:0", "x:1", logr.Discard(), WithNodeToken(writeToken(t, "tok")))
	if err == nil || !strings.Contains(err.Error(), "clear") {
		t.Fatalf("%v", err)
	}
	if err := Forward(context.Background(), "127.0.0.1:0", "x:1", logr.Discard(), WithNodeToken(writeToken(t, "tok")), WithPlainDevelopment()); err == nil {
		t.Fatal("the development setup sends no token: both options together are refused")
	}
}

// The development setup relays without any token, and every client header is still removed.
func TestForwardPlainDevelopmentSendsNoToken(t *testing.T) {
	var s seen
	up := httptest.NewServer(s.handler())
	defer up.Close()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	listen := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = Forward(ctx, listen, strings.TrimPrefix(up.URL, "http://"), logr.Discard(), WithPlainDevelopment())
	}()
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		req, _ := http.NewRequest("GET", "http://"+listen+"/v2/", nil)
		req.Header.Set("X-Cib-Node-Token", "forged")
		req.Header.Set("Authorization", "Basic x")
		if resp, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if h := s.headers[0]; h.Get("X-Cib-Node-Token") != "" || h.Get("Authorization") != "" {
		t.Fatalf("%v", h)
	}
}

func TestForwardNeedsAToken(t *testing.T) {
	if err := Forward(context.Background(), "127.0.0.1:0", "x:1", logr.Discard()); err == nil {
		t.Fatal("a relay without a token has no identity and must not start")
	}
}

func TestPushSocketRefusesAFileAtItsPath(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x.sock")
	if err := os.WriteFile(f, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenPushSocket(f, -1); err == nil {
		t.Fatal("a regular file at the path is never removed")
	}
	// a socket left by an earlier run is replaced
	sock := filepath.Join(shortDir(t), "old.sock")
	old, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if ul, ok := old.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = old.Close()
	ln, err := listenPushSocket(sock, -1)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
}

// With a certificate authority the relay speaks TLS to the gate and trusts only that authority; a server of another authority is refused.
func TestForwardOverTLS(t *testing.T) {
	var got http.Header
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte("ok"))
	}))
	defer up.Close()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: up.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	target := strings.TrimPrefix(up.URL, "https://")
	o := forwardOptions{tlsCAFile: caFile}
	if err := o.loadTLS(target); err != nil {
		t.Fatal(err)
	}
	tf := imagecache.NewTokenFile(writeToken(t, "tok"))
	h := relayHandler("https", target, o.upstreamTransport(), tf, pullAllowed, nil)
	if rec := do(h, "GET", "/v2/c/acme/ghcr.io/o/app/manifests/sha256:abc", nil); rec.Code != 200 || got.Get("X-Cib-Node-Token") != "tok" {
		t.Fatalf("%d %v", rec.Code, got)
	}
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = http.Header{"Reached": {"yes"}} }))
	other.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	other.StartTLS()
	defer other.Close()
	got = nil
	h2 := relayHandler("https", strings.TrimPrefix(other.URL, "https://"), o.upstreamTransport(), tf, pullAllowed, nil)
	if rec := do(h2, "GET", "/v2/c/a/b/manifests/x", nil); rec.Code != http.StatusBadGateway || got != nil {
		t.Fatalf("an untrusted server must not be reached: %d %v", rec.Code, got)
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

// On the push socket only the query parameters of the snapshot client reach the gate; the pull listener forwards the query as it is.
func TestPushSocketDropsQueryParametersTheClientDoesNotUse(t *testing.T) {
	var s seen
	h, _ := handlerFor(t, &s, pushAllowed)
	up := httptest.NewServer(s.handler())
	defer up.Close()
	tf := imagecache.NewTokenFile(writeToken(t, "tok"))
	h = relayHandler("http", strings.TrimPrefix(up.URL, "http://"), http.DefaultTransport, tf, pushAllowed, pushQuery)
	do(h, "POST", "/v2/lab/ns/lab/dev/blobs/uploads/?mount=sha256%3Aabc&from=c%2Fdocker.io%2Flibrary%2Fapp&sneaky=1&_state=x", nil)
	do(h, "PUT", "/v2/lab/ns/lab/dev/blobs/uploads/id?digest=sha256%3Aabc&other=1", nil)
	q := s.queries
	if len(q) != 2 || strings.Contains(q[0], "sneaky") || strings.Contains(q[0], "_state") || !strings.Contains(q[0], "mount=") || !strings.Contains(q[0], "from=") ||
		strings.Contains(q[1], "other") || !strings.Contains(q[1], "digest=") {
		t.Fatalf("%v", q)
	}
}

// The push socket has a directory of its own (0700) and is created 0600; the upstream transport takes no proxy from the environment and is
// bounded on every wait but the transfer.
func TestPushSocketDirectoryAndUpstreamTransportBounds(t *testing.T) {
	sock := filepath.Join(shortDir(t), "registry-push", "push.sock")
	ln, err := listenPushSocket(sock, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	di, _ := os.Stat(filepath.Dir(sock))
	si, _ := os.Stat(sock)
	if di.Mode().Perm() != 0o700 || si.Mode().Perm() != 0o600 {
		t.Fatalf("dir %v socket %v", di.Mode(), si.Mode())
	}
	var o forwardOptions
	tr, ok := o.upstreamTransport().(*http.Transport)
	if !ok || tr.Proxy != nil || tr.ResponseHeaderTimeout <= 0 || tr.IdleConnTimeout <= 0 || tr.MaxResponseHeaderBytes != maxHeaderBytes || tr.TLSHandshakeTimeout <= 0 {
		t.Fatalf("%+v", tr)
	}
}
