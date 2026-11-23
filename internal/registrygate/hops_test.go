package registrygate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/netguard"
	"github.com/cybericebox/laboratory/internal/netguard/netguardtest"
)

// redirector is a registry (plain http, on the loopback) that answers every request with a 307 to loc, and a target that counts its hits.
func redirector(t *testing.T, loc func(target *httptest.Server) string) (reg Registry, target *httptest.Server, hits *atomic.Int64) {
	t.Helper()
	hits = &atomic.Int64{}
	target = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("secret of the inside"))
	}))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", loc(target))
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	return Registry{Name: "up", URL: mustURL(t, srv.URL)}, target, hits
}

// A compromised upstream cannot make the fill path request an internal endpoint, by any spelling of its address.
func TestFetchDoesNotFollowARedirectToAnInternalEndpoint(t *testing.T) {
	for name, loc := range map[string]func(*httptest.Server) string{
		"IP literal":    func(s *httptest.Server) string { return s.URL + "/x" },
		"default port":  func(*httptest.Server) string { return "https://127.0.0.1/x" },
		"decimal":       func(*httptest.Server) string { return "https://2130706433/x" },
		"IPv6":          func(*httptest.Server) string { return "https://[::1]/x" },
		"metadata":      func(*httptest.Server) string { return "https://169.254.169.254/latest/meta-data" },
		"a port":        func(s *httptest.Server) string { return "https://cdn.example.com:8443/x" },
		"userinfo":      func(*httptest.Server) string { return "https://u:p@cdn.example.com/x" },
		"plain http":    func(*httptest.Server) string { return "http://cdn.example.com/x" },
		"relative path": func(*httptest.Server) string { return "//127.0.0.1/x" },
	} {
		reg, _, hits := redirector(t, loc)
		resp, out, err := NewUpstream(2*time.Second).Fetch(context.Background(), reg, Object{Repo: "o/r", Kind: kindBlobs, Ref: "sha256:00"}, Creds{"alice", "pw"}, "k")
		if resp != nil || out == Allowed || err == nil || hits.Load() != 0 {
			t.Errorf("%s: resp %v, outcome %v, err %v, hits %d", name, resp != nil, out, err, hits.Load())
		}
	}
}

// A name that resolves to the loopback is refused when the connection is made (DNS rebinding), on the fill path (a manifest proof does not follow a redirect at all).
func TestARedirectToANameThatResolvesToTheLoopbackIsRefusedAtDialTime(t *testing.T) {
	reg, _, hits := redirector(t, func(*httptest.Server) string { return "https://rebind.test/x" })
	u := NewUpstream(2 * time.Second)
	u.HTTP.Transport = netguard.NewTransport(2*time.Second, netguardtest.DNSServer(t))

	resp, out, err := u.Fetch(context.Background(), reg, Object{Repo: "o/r", Kind: kindBlobs, Ref: "sha256:00"}, Creds{}, "")
	if resp != nil || out == Allowed || err == nil || !strings.Contains(err.Error(), "internal address") {
		t.Fatalf("fill path: %v %v %v", resp != nil, out, err)
	}
	if hits.Load() != 0 {
		t.Fatalf("the target was reached: %d", hits.Load())
	}
}

// A registry that redirects to its own origin is still followed (a registry of the cluster, over plain http).
func TestFetchFollowsARedirectToTheRegistrysOwnOrigin(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/o/r/blobs/sha256:00" {
			http.Redirect(w, r, srv.URL+"/storage/blob", http.StatusTemporaryRedirect)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("a redirect carries no Authorization")
		}
		_, _ = w.Write([]byte("blob"))
	}))
	defer srv.Close()
	resp, out, err := NewUpstream(2*time.Second).Fetch(context.Background(), Registry{Name: "up", URL: mustURL(t, srv.URL)}, Object{Repo: "o/r", Kind: kindBlobs, Ref: "sha256:00"}, Creds{}, "")
	if err != nil || out != Allowed || resp == nil {
		t.Fatalf("%v %v", out, err)
	}
	_ = resp.Body.Close()
}
