package registrygate

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newGate(t *testing.T, zot http.Handler) (*Gate, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(zot)
	t.Cleanup(srv.Close)
	g, err := New(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return g, srv
}

func TestGatePassesRequestsToZotUnchanged(t *testing.T) {
	var seen *http.Request
	var body string
	g, _ := newGate(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Docker-Content-Digest", "sha256:abc")
		w.Header().Set("Location", "http://"+r.Host+"/v2/x/blobs/uploads/1")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("zot says"))
	}))
	req := httptest.NewRequest(http.MethodPost, "http://localhost:5035/v2/lab/a/blobs/uploads/?mount=sha256:1&from=b", strings.NewReader("payload"))
	req.Header.Set("Authorization", "Basic dXNlcjpwdw==")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted || w.Body.String() != "zot says" || w.Header().Get("Docker-Content-Digest") != "sha256:abc" {
		t.Fatalf("response %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if seen.URL.Path != "/v2/lab/a/blobs/uploads/" || seen.URL.RawQuery != "mount=sha256:1&from=b" || body != "payload" {
		t.Fatalf("request changed on the way: %s?%s %q", seen.URL.Path, seen.URL.RawQuery, body)
	}
	if seen.Header.Get("Authorization") != "Basic dXNlcjpwdw==" {
		t.Fatalf("the pass-through paths keep the client's credentials for zot: %q", seen.Header.Get("Authorization"))
	}
	if seen.Host != "localhost:5035" || !strings.HasPrefix(w.Header().Get("Location"), "http://localhost:5035/") {
		t.Fatalf("the client's own registry name must survive: host %q location %q", seen.Host, w.Header().Get("Location"))
	}
}

func TestGateRefusesWhatZotMustNeverSee(t *testing.T) {
	hit := false
	g, _ := newGate(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	for _, target := range []string{"/v2/a/../b", "/v2/a%2Fb", "/metrics", "/zot/auth/login", "/v2/a//b"} {
		req := httptest.NewRequest(http.MethodGet, "http://x"+target, nil)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
	if hit {
		t.Fatal("a refused request reached zot")
	}
}

func TestGateAnswers502WhenZotIsDownAndReadyzFollows(t *testing.T) {
	g, srv := newGate(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	h := g.HealthHandler()
	probe := func(path string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w.Code
	}
	if probe("/healthz") != http.StatusOK || probe("/readyz") != http.StatusOK {
		t.Fatal("zot is up: both probes must pass")
	}
	srv.Close()
	if probe("/healthz") != http.StatusServiceUnavailable {
		t.Fatal("liveness includes zot's /v2/: with zot gone it fails")
	}
	if probe("/readyz") != http.StatusServiceUnavailable {
		t.Fatal("readiness must follow zot")
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://x/v2/", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("zot down: %d", w.Code)
	}
	if err := g.Ready(context.Background()); err == nil {
		t.Fatal("Ready must fail")
	}
}

func TestNewRejectsABadAddress(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("an empty zot address must be refused")
	}
}

// The cache path carries the pull's credentials: with CacheTLSOnly a plain connection is refused before anything is read.
func TestCachePathNeedsTLSWhenAsked(t *testing.T) {
	g, _ := newGate(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	served := false
	g.Cache = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true })
	g.CacheTLSOnly = true
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://x/v2/c/docker.io/library/nginx/manifests/latest", nil))
	if w.Code != http.StatusForbidden || served {
		t.Fatalf("plain: %d served %v", w.Code, served)
	}
	req := httptest.NewRequest(http.MethodGet, "https://x/v2/c/docker.io/library/nginx/manifests/latest", nil)
	req.TLS = &tls.ConnectionState{}
	g.ServeHTTP(httptest.NewRecorder(), req)
	if !served {
		t.Fatal("over TLS the cache path is served")
	}
	// the rest of the registry is not affected
	w = httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://x/v2/lab/a/manifests/x", nil))
	if w.Code == http.StatusForbidden {
		t.Fatal("the pass-through is served on the plain port")
	}
}
