package registrygate

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// countingReviewer counts the TokenReview calls and answers like testReviewer, or fails.
type countingReviewer struct {
	calls int
	fail  bool
}

func (c *countingReviewer) Review(ctx context.Context, token string, aud []string) (Review, error) {
	c.calls++
	if c.fail {
		return Review{}, errors.New("the API server is down")
	}
	return testReviewer{}.Review(ctx, token, aud)
}

func authedGate(t *testing.T) (*Gate, *Schedule, *countingReviewer) {
	t.Helper()
	g, _ := newGate(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	sch := NewSchedule()
	sch.Confirm()
	rev := &countingReviewer{}
	g.Auth = &Authenticator{Reviewer: rev, Accounts: testAccounts, Schedule: sch, Precheck: anyShape}
	return g, sch, rev
}

func do(g http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://x"+target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	return w
}

func TestGateRefusesWhatZotMustNeverSee(t *testing.T) {
	hit := false
	g, _ := newGate(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	for _, target := range []string{"/v2/a/../b", "/v2/a%2Fb", "/metrics", "/zot/auth/login", "/v2/a//b"} {
		w := do(g, http.MethodGet, target, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
	if hit {
		t.Fatal("a refused request reached zot")
	}
}

func TestGateAnswersHealthAndReadyzFollowsZot(t *testing.T) {
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
	if err := g.Ready(context.Background()); err == nil {
		t.Fatal("Ready must fail")
	}
}

func TestNewRejectsABadAddress(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("an empty zot address must be refused")
	}
}

// The token is in every request: with TLSOnly a plain connection is refused before anything is read.
func TestGateNeedsTLSWhenAsked(t *testing.T) {
	g, _, rev := authedGate(t)
	g.TLSOnly = true
	w := do(g, http.MethodGet, "/v2/", map[string]string{HeaderNodeToken: "agent-token"})
	if w.Code != http.StatusForbidden || rev.calls != 0 {
		t.Fatalf("plain: %d (token reviews %d)", w.Code, rev.calls)
	}
	req := httptest.NewRequest(http.MethodGet, "https://x/v2/", nil)
	req.Header.Set(HeaderNodeToken, "agent-token")
	req.TLS = &tls.ConnectionState{}
	w = httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("over TLS: %d", w.Code)
	}
}

// 2.4.3 row "GET /v2/": any accepted principal, always 200, and no challenge.
func TestV2RootIsAnsweredForAnAcceptedPrincipalOnly(t *testing.T) {
	g, _, _ := authedGate(t)
	for _, tok := range []string{"agent-token", "operator-token"} {
		if w := do(g, http.MethodGet, "/v2/", map[string]string{HeaderNodeToken: tok}); w.Code != http.StatusOK || w.Header().Get("Www-Authenticate") != "" {
			t.Errorf("%s: %d %v", tok, w.Code, w.Header())
		}
	}
	if w := do(g, http.MethodGet, "/v2/", nil); w.Code != http.StatusUnauthorized || w.Header().Get("Www-Authenticate") != "" {
		t.Errorf("no token: %d (no Basic challenge may be sent)", w.Code)
	}
	if w := do(g, http.MethodGet, "/v2/", map[string]string{HeaderNodeToken: "garbage"}); w.Code != http.StatusUnauthorized {
		t.Errorf("a token the API server refuses: %d", w.Code)
	}
}

// The token of another ServiceAccount is refused, whatever it is valid for.
func TestTheTokenOfAnotherServiceAccountIsRefused(t *testing.T) {
	g, _, _ := authedGate(t)
	for _, target := range []string{"/v2/", "/v2/c/acme/docker.io/library/nginx/manifests/latest", "/v2/lab/ns/lab/dev/manifests/latest"} {
		if w := do(g, http.MethodGet, target, map[string]string{HeaderNodeToken: "other-token"}); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
}

// An account that is not set is not accepted at all (an empty name must never match).
func TestAnUnsetAccountMatchesNobody(t *testing.T) {
	g, _, _ := authedGate(t)
	g.Auth.Accounts.Operator = Account{}
	if w := do(g, http.MethodGet, "/v2/", map[string]string{HeaderNodeToken: "operator-token"}); w.Code != http.StatusForbidden {
		t.Fatalf("operator not configured: %d", w.Code)
	}
}

// Headers a caller sends that the platform defines are never passed on, and Authorization of a node is never read.
func TestSpoofedPlatformHeadersAreStrippedBeforeZot(t *testing.T) {
	var seen http.Header
	zotSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer zotSrv.Close()
	g, _, _ := authedGate(t)
	zu := mustURL(t, zotSrv.URL)
	lab := NewLabGate(zu, "gate", "pw")
	lab.Schedule = g.Auth.Schedule
	g.Lab = lab
	req := httptest.NewRequest(http.MethodGet, "http://x/v2/lab/ns/lab/dev/manifests/latest", nil)
	req.Header.Set(HeaderNodeToken, "operator-token")
	req.Header.Set("X-Cib-Node", "evil")
	req.Header.Set("x-cib-tenant", "evil")
	req.Header.Set("Authorization", "Basic ZXZpbDpldmls")
	g.ServeHTTP(httptest.NewRecorder(), req)
	if seen == nil {
		t.Fatal("the request did not reach zot")
	}
	for k := range seen {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), HeaderPrefix) {
			t.Errorf("zot saw %s", k)
		}
	}
	if u, p, ok := parseBasic(seen.Get("Authorization")); !ok || u != "gate" || p != "pw" {
		t.Errorf("zot must see the gate's own account only: %q", seen.Get("Authorization"))
	}
}

func parseBasic(h string) (string, string, bool) {
	r := &http.Request{Header: http.Header{"Authorization": {h}}}
	return r.BasicAuth()
}

// The token review is cached; a token not in the cache needs the API server, and an unreachable API server is a 503, not a refusal.
func TestTokenReviewIsCachedAndAnOutageIsA503(t *testing.T) {
	g, _, rev := authedGate(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	g.Auth.Now = func() time.Time { return now }
	hdr := map[string]string{HeaderNodeToken: "agent-token"}
	for i := 0; i < 3; i++ {
		if w := do(g, http.MethodGet, "/v2/", hdr); w.Code != http.StatusOK {
			t.Fatal(w.Code)
		}
	}
	if rev.calls != 1 {
		t.Fatalf("one review for three requests, got %d", rev.calls)
	}
	rev.fail = true
	if w := do(g, http.MethodGet, "/v2/", hdr); w.Code != http.StatusOK {
		t.Fatalf("a cached token keeps working while the API server is down: %d", w.Code)
	}
	now = now.Add(6 * time.Minute) // past the cache time
	w := do(g, http.MethodGet, "/v2/", hdr)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "30" {
		t.Fatalf("an uncached token with the API server down: %d %v", w.Code, w.Header())
	}
	w = do(g, http.MethodGet, "/v2/", map[string]string{HeaderNodeToken: "never-seen"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a token that is not cached gets 503: %d", w.Code)
	}
}

func TestAnythingElseIsDenied(t *testing.T) {
	g, _, _ := authedGate(t)
	for _, target := range []string{"/v2/other/manifests/x", "/v2/platform/x/manifests/latest", "/v2/zot/x"} {
		if w := do(g, http.MethodGet, target, map[string]string{HeaderNodeToken: "operator-token"}); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
	if w := do(g, http.MethodPost, "/v2/", map[string]string{HeaderNodeToken: "operator-token"}); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /v2/: %d", w.Code)
	}
}
