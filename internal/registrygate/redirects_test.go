package registrygate

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// redirectRig is an upstream registry (plain http on the loopback) that answers every manifest HEAD with a 307 to loc, and a target that
// every https or http host name of the test resolves to (the dial is redirected, so the names are fixed and nothing leaves the loopback).
type redirectRig struct {
	t      *testing.T
	reg    *httptest.Server
	target http.HandlerFunc

	mu       sync.Mutex
	loc      string
	regAuth  []string
	hits     []seen
	up       *Upstream
	registry Registry
}

type seen struct{ Host, Path, Method, Auth, Accept string }

func newRedirectRig(t *testing.T, patterns []string) *redirectRig {
	t.Helper()
	r := &redirectRig{t: t}
	r.reg = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.regAuth = append(r.regAuth, req.Header.Get("Authorization"))
		loc := r.loc
		r.mu.Unlock()
		if req.Header.Get("Authorization") == "" {
			w.Header().Set("Www-Authenticate", `Basic realm="up"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(r.reg.Close)
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits = append(r.hits, seen{req.Host, req.URL.Path, req.Method, req.Header.Get("Authorization"), req.Header.Get("Accept")})
		r.mu.Unlock()
		r.target(w, req)
	})
	tlsSrv, plain := httptest.NewTLSServer(h), httptest.NewServer(h)
	t.Cleanup(tlsSrv.Close)
	t.Cleanup(plain.Close)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // the test target's own certificate, loopback only
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			switch {
			case strings.HasSuffix(addr, ":443"), strings.HasSuffix(addr, ":8443"):
				return d.DialContext(ctx, network, tlsSrv.Listener.Addr().String())
			case strings.HasSuffix(addr, ":80"):
				return d.DialContext(ctx, network, plain.Listener.Addr().String())
			}
			return d.DialContext(ctx, network, addr)
		},
	}
	r.up = NewUpstream(5 * time.Second)
	r.up.HTTP.Transport = tr
	reg := Registry{Name: "k8s.test", Redirects: patterns}
	reg.URL = mustURL(t, r.reg.URL)
	r.registry = reg
	return r
}

func (r *redirectRig) proof(loc string, kind string) (Outcome, http.Header, error) {
	r.mu.Lock()
	r.loc = loc
	r.mu.Unlock()
	return r.up.Proof(context.Background(), r.registry, Object{Repo: "pause", Kind: kind, Ref: "3.10", Accept: "application/vnd.oci.image.index.v1+json"},
		Creds{"alice", "secret"}, "h")
}

func (r *redirectRig) seen() []seen {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]seen(nil), r.hits...)
}

func ok200(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Docker-Content-Digest", "sha256:ab")
	w.WriteHeader(200)
}

func TestRedirectAllowedTargetIsFollowedAndJudgedAsDirect(t *testing.T) {
	r := newRedirectRig(t, []string{"*.pkg.test"})
	r.target = ok200
	o, hdr, err := r.proof("https://eu-docker.pkg.test/v2/k8s/pause/manifests/3.10?x=1", kindManifests)
	if o != Allowed || err != nil || hdr.Get("Docker-Content-Digest") != "sha256:ab" {
		t.Fatalf("%v %v %v", o, err, hdr)
	}
	hits := r.seen()
	if len(hits) != 1 || hits[0].Method != http.MethodHead || hits[0].Accept != "application/vnd.oci.image.index.v1+json" || hits[0].Host != "eu-docker.pkg.test" {
		t.Fatalf("the target gets the same method and Accept: %+v", hits)
	}
	// The registry itself did get the credentials (Basic after its challenge); the target must not.
	if hits[0].Auth != "" {
		t.Fatalf("credentials reached the redirect target: %q", hits[0].Auth)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.regAuth) != 2 || !strings.HasPrefix(r.regAuth[1], "Basic ") {
		t.Fatalf("the registry was asked with the credentials: %v", r.regAuth)
	}
}

// A refusal from the redirect target says nothing about the caller's credentials (none were sent), and a ref of the caller's choice could
// make a CDN answer 403 for everybody sharing the key: after a redirect only a 200 or a 404 is an answer, as for the data transfers (classify).
func TestRedirectFinalAnswerIsJudgedLikeADirectOne(t *testing.T) {
	for code, want := range map[int]Outcome{200: Allowed, 401: Unreachable, 403: Unreachable, 404: NotFound, 500: Unreachable, 429: Unreachable, 204: Unreachable} {
		r := newRedirectRig(t, []string{"*.pkg.test"})
		r.target = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
		if o, _, _ := r.proof("https://a.pkg.test/v2/x/manifests/1", kindManifests); o != want {
			t.Errorf("%d: %v, want %v", code, o, want)
		}
	}
}

func TestRedirectsStayInconclusiveWhenNotAllowed(t *testing.T) {
	cases := map[string]string{
		"a host outside the patterns":              "https://other.example.test/v2/x/manifests/1",
		"the apex of a wildcard":                   "https://pkg.test/v2/x/manifests/1",
		"a look-alike suffix":                      "https://evilpkg.test/v2/x/manifests/1",
		"a pattern host as a subdomain of another": "https://a.pkg.test.evil.test/v2/x/manifests/1",
		"plain http":                               "http://a.pkg.test/v2/x/manifests/1",
		"a port":                                   "https://a.pkg.test:8443/v2/x/manifests/1",
		"the default port written out":             "https://a.pkg.test:443/v2/x/manifests/1",
		"userinfo":                                 "https://user:pw@a.pkg.test/v2/x/manifests/1",
		"an IP literal":                            "https://127.0.0.1/v2/x/manifests/1",
		"an IPv6 literal":                          "https://[::1]/v2/x/manifests/1",
		"a trailing dot":                           "https://a.pkg.test./v2/x/manifests/1",
		"a relative Location (the registry's own host)": "/v2/x/manifests/1",
		"no Location":    "",
		"another scheme": "ftp://a.pkg.test/x",
	}
	for name, loc := range cases {
		r := newRedirectRig(t, []string{"*.pkg.test"})
		r.target = ok200
		if o, _, _ := r.proof(loc, kindManifests); o != Unreachable {
			t.Errorf("%s: %v, want inconclusive", name, o)
		}
		if n := len(r.seen()); n != 0 {
			t.Errorf("%s: %d requests reached the target", name, n)
		}
	}
}

func TestRegistryWithoutRedirectsAndBlobsStayAsToday(t *testing.T) {
	r := newRedirectRig(t, nil)
	r.target = ok200
	if o, _, _ := r.proof("https://a.pkg.test/v2/x/manifests/1", kindManifests); o != Unreachable || len(r.seen()) != 0 {
		t.Fatalf("a registry not in the list: %v %v", o, r.seen())
	}
	r = newRedirectRig(t, []string{"*.pkg.test"})
	r.target = ok200
	if o, _, _ := r.proof("https://a.pkg.test/v2/x/blobs/1", kindBlobs); o != Unreachable || len(r.seen()) != 0 {
		t.Fatalf("the list is for manifests only: %v %v", o, r.seen())
	}
}

func TestRedirectExactHostPattern(t *testing.T) {
	r := newRedirectRig(t, []string{"cdn.example.test"})
	r.target = ok200
	if o, _, _ := r.proof("https://cdn.example.test/m", kindManifests); o != Allowed {
		t.Fatalf("exact: %v", o)
	}
	if o, _, _ := r.proof("https://x.cdn.example.test/m", kindManifests); o != Unreachable {
		t.Fatalf("an exact pattern has no subdomains: %v", o)
	}
}

func TestRedirectChainLimitLoopAndSecondHop(t *testing.T) {
	// A relative Location on the second hop is resolved against the target and checked again.
	r := newRedirectRig(t, []string{"*.pkg.test"})
	r.target = func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/start" {
			w.Header().Set("Location", "/final")
			w.WriteHeader(http.StatusFound)
			return
		}
		ok200(w, req)
	}
	if o, _, _ := r.proof("https://a.pkg.test/start", kindManifests); o != Allowed || len(r.seen()) != 2 || r.seen()[1].Path != "/final" || r.seen()[1].Host != "a.pkg.test" {
		t.Fatalf("relative second hop: %v %+v", o, r.seen())
	}

	// A second hop that leaves the patterns is not followed.
	r = newRedirectRig(t, []string{"*.pkg.test"})
	r.target = func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Location", "https://evil.example.test/steal")
		w.WriteHeader(http.StatusMovedPermanently)
	}
	if o, _, _ := r.proof("https://a.pkg.test/start", kindManifests); o != Unreachable || len(r.seen()) != 1 {
		t.Fatalf("second hop leaves the patterns: %v %+v", o, r.seen())
	}

	// A loop stops at the limit.
	r = newRedirectRig(t, []string{"*.pkg.test"})
	r.target = func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Location", "https://a.pkg.test/loop")
		w.WriteHeader(http.StatusSeeOther)
	}
	if o, _, _ := r.proof("https://a.pkg.test/loop", kindManifests); o != Unreachable || len(r.seen()) != maxProofRedirects {
		t.Fatalf("loop: %v %d requests", o, len(r.seen()))
	}

	// Exactly the limit is followed, one more is not.
	for _, depth := range []int{maxProofRedirects, maxProofRedirects + 1} {
		r = newRedirectRig(t, []string{"*.pkg.test"})
		r.target = func(w http.ResponseWriter, req *http.Request) {
			n := 0
			for _, c := range req.URL.Path {
				if c == 'x' {
					n++
				}
			}
			if n+1 >= depth {
				ok200(w, req)
				return
			}
			w.Header().Set("Location", "https://a.pkg.test"+req.URL.Path+"x")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}
		o, _, _ := r.proof("https://a.pkg.test/", kindManifests)
		want := Allowed
		if depth > maxProofRedirects {
			want = Unreachable
		}
		if o != want {
			t.Errorf("%d requests deep: %v, want %v", depth, o, want)
		}
	}
}

func TestRedirectChainSharesOneDeadline(t *testing.T) {
	r := newRedirectRig(t, []string{"*.pkg.test"})
	r.target = func(w http.ResponseWriter, req *http.Request) { time.Sleep(600 * time.Millisecond); ok200(w, req) }
	r.up.Timeout = 150 * time.Millisecond
	if o, _, err := r.proof("https://a.pkg.test/slow", kindManifests); o != Unreachable || err == nil {
		t.Fatalf("%v %v", o, err)
	}
}

func TestParseRedirectPatterns(t *testing.T) {
	good := [][]string{{"*.pkg.dev"}, {"cdn.example.com", "*.Example.org"}, {"*.co.uk-owner.test"}, nil}
	for _, g := range good {
		if _, err := ParseRedirectPatterns(g); err != nil {
			t.Errorf("%v refused: %v", g, err)
		}
	}
	if got, _ := ParseRedirectPatterns([]string{" *.PKG.dev "}); len(got) != 1 || got[0] != "*.pkg.dev" {
		t.Errorf("normalized: %v", got)
	}
	bad := []string{"", " ", "*", "*.", "**", "*.*.pkg.dev", "a.*.pkg.dev", "pkg.*", "*pkg.dev", "*.com", "*.co.uk", "com", "dev", "*.dev",
		"*.localhost", "localhost", "https://a.pkg.dev", "a.pkg.dev:443", "a.pkg.dev/x", "u@a.pkg.dev", "127.0.0.1", "*.127.0.0.1", "::1", "[::1]",
		"a..pkg.dev", ".pkg.dev", "a.pkg.dev.", "-a.pkg.dev", "a_b.pkg.dev", "a b.pkg.dev",
		// a wildcard whose subdomains are themselves public suffixes (EC2 host names: anyone's instance), and numeric last labels (IPv4 forms)
		"*.compute.amazonaws.com", "*.1", "0x7f.1", "a.0x7f.1", "*.0x7f.1"}
	for _, b := range bad {
		if _, err := ParseRedirectPatterns([]string{b}); err == nil {
			t.Errorf("%q accepted", b)
		}
		if _, err := ParseRedirectPatterns([]string{"*.pkg.dev", b}); err == nil {
			t.Errorf("a list with %q accepted", b)
		}
	}
}

func TestParseRegistriesCarriesAndChecksRedirects(t *testing.T) {
	list, err := ParseRegistries([]byte(`[{"name":"registry.k8s.io","url":"https://registry.k8s.io","redirects":["*.pkg.dev"]},{"name":"ghcr.io","url":"https://ghcr.io"}]`))
	if err != nil || len(list) != 2 || len(list[0].Redirects) != 1 || list[0].Redirects[0] != "*.pkg.dev" || len(list[1].Redirects) != 0 {
		t.Fatalf("%+v %v", list, err)
	}
	for _, bad := range []string{`["*"]`, `["*.com"]`, `["a.pkg.dev:8443"]`, `[""]`, `"*.pkg.dev"`} {
		if _, err := ParseRegistries([]byte(`[{"name":"registry.k8s.io","url":"https://registry.k8s.io","redirects":` + bad + `}]`)); err == nil {
			t.Errorf("redirects %s accepted: the gate must refuse a list it cannot enforce", bad)
		}
	}
}
