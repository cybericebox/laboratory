package l7

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const labHost = "web-abc12w.challenges.example.com"

// staticKeys is a KeyLookup with one key.
func staticKeys(tenant, kid string, pub ed25519.PublicKey) KeyLookup {
	return func(t, k string) (ed25519.PublicKey, bool) {
		if t == tenant && k == kid {
			return pub, true
		}
		return nil, false
	}
}

// baseHandoff is a valid handoff of tenant acme for group g1.
func baseHandoff(now time.Time) handoffClaims {
	return handoffClaims{
		GroupID: "g1", Host: labHost, Session: now.Add(24 * time.Hour).Unix(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "acme", Subject: "p-u1", Audience: jwt.ClaimStrings{HandoffAudience},
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
	}
}

func signHandoff(t *testing.T, priv ed25519.PrivateKey, kid string, claims handoffClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

type handoffFixture struct {
	priv    ed25519.PrivateKey
	pub     ed25519.PublicKey
	groups  map[string]string // group id -> owning tenant
	owner   func(string) (string, bool)
	handler *Handler
	backend *httptest.Server
	now     time.Time
}

func newHandoffFixture(t *testing.T) *handoffFixture {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cookies" { // a device that tries to set the proxy's session cookie, and one of its own
			http.SetCookie(w, &http.Cookie{Name: "challenge", Value: "forged", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "app", Value: "1", Path: "/"})
		}
		_, _ = w.Write([]byte("lab"))
	}))
	t.Cleanup(backend.Close)
	f := &handoffFixture{priv: priv, backend: backend, now: time.Now(), groups: map[string]string{"g1": "acme"}}
	f.owner = func(g string) (string, bool) { t, ok := f.groups[g]; return t, ok }
	f.pub = pub
	f.handler = NewHandler(staticKeys("acme", "k1", pub), testSecret, "challenges.example.com", "challenge",
		func(task, groupID string) (string, error) { return backend.URL, nil }).
		WithGroupTenant(func(groupID string) (string, bool) { return f.owner(groupID) })
	f.handler.now = func() time.Time { return f.now }
	return f
}

func (f *handoffFixture) link(t *testing.T, mutate func(*handoffClaims)) string {
	t.Helper()
	claims := baseHandoff(f.now)
	if mutate != nil {
		mutate(&claims)
	}
	return signHandoff(t, f.priv, "k1", claims)
}

func (f *handoffFixture) open(token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "https://"+labHost+AuthPath+"?t="+url.QueryEscape(token), nil)
	req.Host = labHost
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestHandoff_SetsTheProxyCookieAndRedirects(t *testing.T) {
	f := newHandoffFixture(t)
	rec := f.open(f.link(t, nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("code = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("referrer policy = %q", rec.Header().Get("Referrer-Policy"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	c := cookies[0]
	if c.Name != "challenge" || c.Domain != "" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Fatalf("cookie = %+v", c)
	}
	if want := f.now.Add(24 * time.Hour).Unix(); c.Expires.Unix() != want {
		t.Fatalf("expires %d, want the session end %d", c.Expires.Unix(), want)
	}
	claims, err := validateCookie(requestWithCookie(c.Value), testSecret, "challenge", f.handler.now)
	if err != nil || claims.GroupID != "g1" || claims.client() != "p-u1" {
		t.Fatalf("cookie claims = %+v err = %v", claims, err)
	}

	// The cookie then opens the lab; the handoff token is not what is checked.
	req := httptest.NewRequest("GET", "https://"+labHost+"/", nil)
	req.Host = labHost
	req.AddCookie(c)
	next := httptest.NewRecorder()
	f.handler.ServeHTTP(next, req)
	if next.Code != 200 || next.Body.String() != "lab" {
		t.Fatalf("lab request: %d %q", next.Code, next.Body.String())
	}
}

// A saved device link must reach its authored destination after authentication.
func TestHandoff_NextPreservesDestination(t *testing.T) {
	f := newHandoffFixture(t)
	token := f.link(t, nil)
	baseline := f.open(token)
	next := "/panel?x=1#anchor"
	req := httptest.NewRequest(http.MethodGet, "https://"+labHost+AuthPath+"?"+url.Values{"t": {token}, "next": {next}}.Encode(), nil)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != next {
		t.Fatalf("code = %d location = %q, want 303 %q", rec.Code, rec.Header().Get("Location"), next)
	}
	if strings.Contains(rec.Header().Get("Location"), token) || strings.Contains(rec.Body.String(), token) {
		t.Fatal("handoff token leaked in redirect")
	}
	if rec.Header().Get("Set-Cookie") != baseline.Header().Get("Set-Cookie") {
		t.Fatal("destination changed session cookie policy or lifetime")
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("handoff response lost token-leak protections")
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.RequestURI()))
	}))
	defer backend.Close()
	f.handler.resolver = func(task, groupID string) (string, error) { return backend.URL, nil }
	location, err := req.URL.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	// Browsers retain the fragment locally and omit it from the HTTP request.
	location.Fragment, location.RawFragment = "", ""
	follow := httptest.NewRequest(http.MethodGet, location.String(), nil)
	follow.AddCookie(rec.Result().Cookies()[0])
	out := httptest.NewRecorder()
	f.handler.ServeHTTP(out, follow)
	if out.Code != http.StatusOK || out.Body.String() != "/panel?x=1" {
		t.Fatalf("device received %d %q, want /panel?x=1", out.Code, out.Body.String())
	}
}

func TestHandoff_NextStaysOnDeviceOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, next, want string
	}{
		{"default", "", "/"},
		{"root", "/", "/"},
		{"path query fragment", "/panel?x=1&y=2#anchor", "/panel?x=1&y=2#anchor"},
		{"encoded path", "/panel%20name?x=%26#part", "/panel%20name?x=%26#part"},
		{"external query value", "/panel?return=https%3A%2F%2Fevil.example%2F", "/panel?return=https%3A%2F%2Fevil.example%2F"},
		{"absolute", "https://evil.example/panel", "/"},
		{"same origin absolute", "https://" + labHost + "/panel", "/"},
		{"scheme relative", "//evil.example", "/"},
		{"triple slash", "///evil.example", "/"},
		{"dot normalized authority", "/a/..//evil.example/path", "/"},
		{"relative", "panel", "/"},
		{"leading whitespace", " /panel", "/"},
		{"backslash authority", `/\evil.example`, "/"},
		{"path backslash", `/panel\docs`, "/"},
		{"query backslash", `/panel?x=\evil`, "/"},
		{"CRLF", "/panel\r\nLocation: https://evil.example", "/"},
		{"tab authority", "/\t/evil.example", "/"},
		{"control", "/panel\x00", "/"},
		{"DEL", "/panel\x7f", "/"},
		{"encoded slash", "/%2fevil.example", "/"},
		{"encoded authority", "%2f%2fevil.example", "/"},
		{"encoded backslash", "/%5cevil.example", "/"},
		{"encoded CRLF", "/panel?x=%0d%0aLocation:https://evil.example", "/"},
		{"encoded tab", "/%09/evil.example", "/"},
		{"encoded DEL", "/panel#%7f", "/"},
		{"double encoded slash", "/%252fevil.example", "/"},
		{"double encoded backslash", "/%255cevil.example", "/"},
		{"double encoded control", "/panel?x=%250a", "/"},
		{"excess nested encoding", "/%252525252fexample", "/"},
		{"malformed escape", "/panel%zz", "/"},
		{"malformed query escape", "/panel?x=%zz", "/"},
		{"auth loop", "/_auth?t=other", "/"},
		{"encoded auth loop", "/%5fauth?t=other", "/"},
		{"dot normalized auth loop", "/panel/../_auth?t=other", "/"},
		{"encoded dot auth loop", "/panel/%2e%2e/_auth?t=other", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHandoffFixture(t)
			token := f.link(t, nil)
			req := httptest.NewRequest(http.MethodGet, "https://"+labHost+AuthPath+"?"+url.Values{"t": {token}, "next": {tc.next}}.Encode(), nil)
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != tc.want {
				t.Fatalf("code = %d location = %q, want 303 %q", rec.Code, rec.Header().Get("Location"), tc.want)
			}
			location, err := req.URL.Parse(rec.Header().Get("Location"))
			if err != nil || location.Scheme != "https" || location.Host != labHost || location.User != nil {
				t.Fatalf("redirect changed device origin: %v (%v)", location, err)
			}
			if strings.Contains(location.String(), token) || len(rec.Result().Cookies()) != 1 {
				t.Fatal("handoff leaked token or failed to issue its session cookie")
			}
		})
	}
}

func TestHandoff_NextDoesNotBypassTokenVerification(t *testing.T) {
	for _, invalid := range []string{"missing", "bad", "expired"} {
		t.Run(invalid, func(t *testing.T) {
			f := newHandoffFixture(t)
			token := ""
			switch invalid {
			case "bad":
				token = "bad-token"
			case "expired":
				token = f.link(t, nil)
				f.now = f.now.Add(2 * time.Minute)
			}
			baseline := f.open(token)
			req := httptest.NewRequest(http.MethodGet, "https://"+labHost+AuthPath+"?"+url.Values{"t": {token}, "next": {"/panel?x=1#anchor"}}.Encode(), nil)
			req.AddCookie(&http.Cookie{Name: "challenge", Value: "existing-session"})
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || rec.Header().Get("Location") != "" || len(rec.Result().Cookies()) != 0 {
				t.Fatalf("code = %d location = %q cookies = %v", rec.Code, rec.Header().Get("Location"), rec.Result().Cookies())
			}
			if rec.Body.String() != baseline.Body.String() || rec.Header().Get("Set-Cookie") != baseline.Header().Get("Set-Cookie") {
				t.Fatal("next changed invalid handoff behavior")
			}
		})
	}
}

func TestHandoff_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*handoffClaims)
	}{
		{"expired link", func(c *handoffClaims) {
			c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-10 * time.Minute))
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-8 * time.Minute))
		}},
		{"another device host", func(c *handoffClaims) { c.Host = "db-abc123" }},
		{"session already over", func(c *handoffClaims) { c.Session = time.Now().Add(-time.Hour).Unix() }},
		{"no client", func(c *handoffClaims) { c.Subject = "" }},
		{"another audience", func(c *handoffClaims) { c.Audience = jwt.ClaimStrings{"somewhere-else"} }},
		{"no audience", func(c *handoffClaims) { c.Audience = nil }},
		{"no not-before", func(c *handoffClaims) { c.NotBefore = nil }},
		{"not valid yet", func(c *handoffClaims) { c.NotBefore = jwt.NewNumericDate(time.Now().Add(10 * time.Minute)) }},
		{"no issuer", func(c *handoffClaims) { c.Issuer = "" }},
		{"an unknown tenant", func(c *handoffClaims) { c.Issuer = "mallory" }},
		{"an issuer that is no name", func(c *handoffClaims) { c.Issuer = "../acme" }},
		{"a group that does not exist", func(c *handoffClaims) { c.GroupID = "nope" }},
		{"link over the lifetime cap", func(c *handoffClaims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(6 * time.Minute)) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newHandoffFixture(t)
			rec := f.open(f.link(t, c.mutate))
			if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
				t.Fatalf("code = %d cookies = %v", rec.Code, rec.Result().Cookies())
			}
		})
	}
}

func TestHandoff_TamperedSignature(t *testing.T) {
	f := newHandoffFixture(t)
	token := f.link(t, nil)
	parts := strings.Split(token, ".")
	parts[1] = parts[1][:len(parts[1])-2] + "AA"
	if rec := f.open(strings.Join(parts, ".")); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("tampered payload: %d", rec.Code)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	signed := signHandoff(t, other, "k1", baseHandoff(f.now))
	if rec := f.open(signed); rec.Code != http.StatusUnauthorized {
		t.Fatalf("foreign key: %d", rec.Code)
	}
}

// The handoff is stateless: the same link opens again until its exp.
func TestHandoff_IsStatelessAndOpensAgainUntilExp(t *testing.T) {
	f := newHandoffFixture(t)
	token := f.link(t, nil)
	for i := 0; i < 2; i++ {
		if rec := f.open(token); rec.Code != http.StatusSeeOther {
			t.Fatalf("open %d: %d", i, rec.Code)
		}
	}
	f.now = f.now.Add(2 * time.Minute)
	if rec := f.open(token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after exp: %d", rec.Code)
	}
}

// Only EdDSA counts: a token signed with RSA, HMAC (any secret) or "none" is refused.
func TestHandoff_RefusesOtherAlgorithms(t *testing.T) {
	f := newHandoffFixture(t)
	claims := baseHandoff(f.now)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	rs, _ := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(rsaKey)
	hs, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(testSecret)
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	for name, token := range map[string]string{"RS256": rs, "HS256": hs, "none": none} {
		if rec := f.open(token); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
			t.Fatalf("%s: %d", name, rec.Code)
		}
	}
}

func TestExpiredCard_NoCookieShowsTheCardWithoutRedirect(t *testing.T) {
	f := newHandoffFixture(t)
	for _, tc := range []struct{ lang, first, hint, other string }{
		{"", "Сесію завершено", "Щоб продовжити, відкрийте лабораторію за посиланням у завданні.", "Session ended"},
		{"uk-UA,uk;q=0.9", "Сесію завершено", "Щоб продовжити, відкрийте лабораторію за посиланням у завданні.", "Session ended"},
		{"en-US,en;q=0.9", "Session ended", "To continue, open the lab from the link in the task.", "Сесію завершено"},
	} {
		req := httptest.NewRequest("GET", "https://"+labHost+"/", nil)
		req.Host = labHost
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		if tc.lang != "" {
			req.Header.Set("Accept-Language", tc.lang)
		}
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		body := rec.Body.String()
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("Location") != "" {
			t.Fatalf("%q: code = %d location = %q", tc.lang, rec.Code, rec.Header().Get("Location"))
		}
		if !strings.Contains(body, tc.first) || !strings.Contains(body, tc.hint) || strings.Contains(body, tc.other) {
			t.Fatalf("%q: card = %s", tc.lang, body)
		}
	}
}

func TestExpiredCard_ExpiredCookie(t *testing.T) {
	f := newHandoffFixture(t)
	rec := f.open(f.link(t, nil))
	c := rec.Result().Cookies()[0]
	f.now = f.now.Add(25 * time.Hour)
	req := httptest.NewRequest("GET", "https://"+labHost+"/", nil)
	req.Host = labHost
	req.AddCookie(c)
	req.Header.Set("Accept", "text/html")
	out := httptest.NewRecorder()
	f.handler.ServeHTTP(out, req)
	if out.Code != http.StatusUnauthorized || !strings.Contains(out.Body.String(), "Сесію завершено") {
		t.Fatalf("expired cookie: %d %s", out.Code, out.Body.String())
	}
}

// The keys of the issuing tenant, by key id: a rotation overlaps (both keys work), a removed key stops,
// an unknown kid is refused.
func TestHandoff_KeysByTenantAndKeyID(t *testing.T) {
	f := newHandoffFixture(t)
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
	keys := map[string]ed25519.PublicKey{"acme/k1": f.pub, "acme/k2": pub2}
	f.handler.keys = func(tenant, kid string) (ed25519.PublicKey, bool) { k, ok := keys[tenant+"/"+kid]; return k, ok }
	open := func(priv ed25519.PrivateKey, kid string) int {
		return f.open(signHandoff(t, priv, kid, baseHandoff(f.now))).Code
	}
	if open(f.priv, "k1") != http.StatusSeeOther || open(priv2, "k2") != http.StatusSeeOther {
		t.Fatal("during a rotation both keys work")
	}
	if open(f.priv, "k2") != http.StatusUnauthorized {
		t.Fatal("a key under another id's name is refused")
	}
	if open(f.priv, "ghost") != http.StatusUnauthorized {
		t.Fatal("an unknown kid is refused")
	}
	// A token with no kid at all.
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, baseHandoff(f.now))
	noKid, _ := tok.SignedString(f.priv)
	if rec := f.open(noKid); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no kid: %d", rec.Code)
	}
	delete(keys, "acme/k1") // the old key is removed
	if open(f.priv, "k1") != http.StatusUnauthorized || open(priv2, "k2") != http.StatusSeeOther {
		t.Fatal("after the removal only the new key works")
	}
}

// CRUCIAL: a tenant cannot open a group of another tenant, with a perfectly valid signature of its own.
func TestHandoff_RefusesAGroupOfAnotherTenant(t *testing.T) {
	f := newHandoffFixture(t)
	f.groups["theirs"] = "mallory"
	rec := f.open(f.link(t, func(c *handoffClaims) { c.GroupID = "theirs" }))
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("cross-tenant handoff: %d", rec.Code)
	}
	// The other tenant's own key does not help either: it is not issuer acme's key.
	pubM, privM, _ := ed25519.GenerateKey(rand.Reader)
	f.handler.keys = func(tenant, kid string) (ed25519.PublicKey, bool) {
		switch tenant + "/" + kid {
		case "acme/k1":
			return f.pub, true
		case "mallory/k1":
			return pubM, true
		}
		return nil, false
	}
	claims := baseHandoff(f.now)
	claims.Issuer, claims.GroupID = "mallory", "g1" // g1 is acme's
	if rec := f.open(signHandoff(t, privM, "k1", claims)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("mallory signing for acme's group: %d", rec.Code)
	}
	claims.GroupID = "theirs"
	if rec := f.open(signHandoff(t, privM, "k1", claims)); rec.Code != http.StatusSeeOther {
		t.Fatalf("mallory for its own group: %d", rec.Code)
	}
}

// A session cookie dies with the group's ownership: the group now belongs to someone else.
func TestSession_StopsWhenTheGroupChangesOwner(t *testing.T) {
	f := newHandoffFixture(t)
	c := f.open(f.link(t, nil)).Result().Cookies()[0]
	get := func() int {
		req := httptest.NewRequest("GET", "https://"+labHost+"/", nil)
		req.Host = labHost
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if get() != 200 {
		t.Fatal("the session opens the lab")
	}
	f.groups["g1"] = "mallory"
	if get() != http.StatusNotFound {
		t.Fatal("a session of a group that changed owner must stop")
	}
	delete(f.groups, "g1")
	if get() != http.StatusNotFound {
		t.Fatal("and so must a session of a group that is gone")
	}
}

// The idle lifetime is the cookie's: a link asking for more still gets only idle at first.
func TestHandoff_CookieLivesTheIdleTTL(t *testing.T) {
	f := newHandoffFixture(t)
	f.handler.WithLimits(DefaultHandoffLifetime, 2*time.Hour, 30*time.Minute, 168*time.Hour)
	cookies := f.open(f.link(t, nil)).Result().Cookies()
	if len(cookies) != 1 || cookies[0].Expires.Unix() != f.now.Add(2*time.Hour).Unix() {
		t.Fatalf("cookies = %v", cookies)
	}
}

// Never beyond the link's own session end, nor the absolute maximum.
func TestHandoff_SessionNeverPassesTheLinkOrTheMax(t *testing.T) {
	f := newHandoffFixture(t)
	f.handler.WithLimits(DefaultHandoffLifetime, 24*time.Hour, time.Hour, 168*time.Hour)
	short := f.link(t, func(c *handoffClaims) { c.Session = f.now.Add(3 * time.Hour).Unix() })
	if c := f.open(short).Result().Cookies(); len(c) != 1 || c[0].Expires.Unix() != f.now.Add(3*time.Hour).Unix() {
		t.Fatalf("the link ends in 3h: %v", c)
	}
	f.handler.WithLimits(DefaultHandoffLifetime, 24*time.Hour, time.Hour, 10*time.Hour)
	long := f.link(t, func(c *handoffClaims) { c.Session = f.now.Add(1000 * time.Hour).Unix() })
	c := f.open(long).Result().Cookies()
	if len(c) != 1 || c[0].Expires.Unix() != f.now.Add(10*time.Hour).Unix() {
		t.Fatalf("the maximum is 10h: %v", c)
	}
}

// Sliding: the cookie is renewed only when less than renewBefore remains, once per window, up to the absolute end.
func TestSession_SlidesWithActivityAndEndsAtTheMax(t *testing.T) {
	f := newHandoffFixture(t)
	f.handler.WithLimits(DefaultHandoffLifetime, 10*time.Hour, 2*time.Hour, 24*time.Hour)
	c := f.open(f.link(t, func(c *handoffClaims) { c.Session = f.now.Add(1000 * time.Hour).Unix() })).Result().Cookies()[0]
	get := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "https://"+labHost+"/", nil)
		req.Host = labHost
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		return rec
	}
	start := f.now
	if rec := get(c); rec.Code != 200 || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("9h-plus remain: no renewal, got %d %v", rec.Code, rec.Result().Cookies())
	}
	if c.Domain != "" {
		t.Fatalf("the session cookie is host-only: %+v", c)
	}
	// 9h later 1h remains (< 2h): renewed to now+10h
	f.now = start.Add(9 * time.Hour)
	rec := get(c)
	renewed := rec.Result().Cookies()
	if rec.Code != 200 || len(renewed) != 1 || renewed[0].Expires.Unix() != f.now.Add(10*time.Hour).Unix() {
		t.Fatalf("renewal: %d %v", rec.Code, renewed)
	}
	// the renewed cookie is not renewed again at once
	if rec := get(renewed[0]); len(rec.Result().Cookies()) != 0 {
		t.Fatal("one renewal per window")
	}
	// near the absolute end (24h from the start) the expiry is cut to it, and then nothing later is possible
	f.now = start.Add(18 * time.Hour)
	last := get(renewed[0]).Result().Cookies()
	if len(last) == 1 && last[0].Domain != "" {
		t.Fatalf("a renewed cookie stays host-only: %+v", last[0])
	}
	if len(last) != 1 || last[0].Expires.Unix() != start.Add(24*time.Hour).Unix() {
		t.Fatalf("cut to the absolute end: %v", last)
	}
	f.now = start.Add(23 * time.Hour)
	if rec := get(last[0]); rec.Code != 200 || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("at the absolute end there is nothing to renew: %d %v", rec.Code, rec.Result().Cookies())
	}
	f.now = start.Add(25 * time.Hour)
	if rec := get(last[0]); rec.Code == 200 {
		t.Fatal("past the absolute end the session is over")
	}
}

// Idle: no request for idle TTL ends it.
func TestSession_EndsAfterTheIdleTTL(t *testing.T) {
	f := newHandoffFixture(t)
	f.handler.WithLimits(DefaultHandoffLifetime, time.Hour, 10*time.Minute, 168*time.Hour)
	c := f.open(f.link(t, nil)).Result().Cookies()[0]
	f.now = f.now.Add(2 * time.Hour)
	req := httptest.NewRequest("GET", "https://"+labHost+"/", nil)
	req.Host = labHost
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatal("an idle session must end")
	}
}

// The token lifetime limit is the configured one.
func TestHandoff_TokenLifetimeLimitIsConfigured(t *testing.T) {
	f := newHandoffFixture(t)
	long := f.link(t, func(c *handoffClaims) { c.ExpiresAt = jwt.NewNumericDate(f.now.Add(45 * time.Second)) })
	if rec := f.open(long); rec.Code != http.StatusSeeOther {
		t.Fatalf("45s under the default 60s: %d", rec.Code)
	}
	over := f.link(t, func(c *handoffClaims) { c.ExpiresAt = jwt.NewNumericDate(f.now.Add(2 * time.Minute)) })
	if rec := f.open(over); rec.Code == http.StatusSeeOther {
		t.Fatal("2m is over the default 60s limit and must be refused")
	}
	f.handler.WithLimits(30*time.Second, DefaultSessionIdleTTL, DefaultSessionRenewBefore, DefaultSessionMaxTTL)
	if rec := f.open(long); rec.Code == http.StatusSeeOther {
		t.Fatal("45s over a 30s limit must be refused")
	}
}

// A device cannot set, replace or clear the proxy's session cookie; its own cookies pass.
func TestDeviceCannotSetTheSessionCookie(t *testing.T) {
	f := newHandoffFixture(t)
	c := f.open(f.link(t, nil)).Result().Cookies()[0]
	req := httptest.NewRequest("GET", "https://"+labHost+"/cookies", nil)
	req.Host = labHost
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var names []string
	for _, sc := range rec.Result().Cookies() {
		names = append(names, sc.Name+"="+sc.Value)
	}
	if len(names) != 1 || names[0] != "app=1" {
		t.Fatalf("the response cookies are %v: the session cookie must be stripped, the device's own kept", names)
	}
}

// A handoff link opens ONE device: its host claim is the whole host name, and the same group's other hosts refuse it.
func TestHandoff_IsBoundToTheFullHostName(t *testing.T) {
	f := newHandoffFixture(t)
	open := func(host, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "https://"+host+AuthPath+"?t="+url.QueryEscape(token), nil)
		req.Host = host
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		return rec
	}
	link := f.link(t, nil) // host = web-abc12w.challenges.example.com
	for _, host := range []string{"web-abc12t.challenges.example.com", "other-xyz12w.challenges.example.com", "web-abc12w.other.example.com"} {
		if rec := open(host, link); rec.Code == http.StatusSeeOther || len(rec.Result().Cookies()) != 0 {
			t.Errorf("a link for %s opened on %s: %d %v", labHost, host, rec.Code, rec.Result().Cookies())
		}
	}
	// only the first label is no longer enough
	if rec := open(labHost, f.link(t, func(c *handoffClaims) { c.Host = "web-abc12w" })); rec.Code != http.StatusUnauthorized {
		t.Errorf("a link that names the first label only: %d", rec.Code)
	}
	// the right host with a port is still the same host
	if rec := open(labHost+":8443", link); rec.Code != http.StatusSeeOther {
		t.Errorf("the same host with a port: %d", rec.Code)
	}
}

// Sessions of two groups live side by side in one browser, one cookie per device host, and each opens only its own host.
func TestSession_CookiesOfTwoGroupsCoexistPerHost(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	backends := map[string]*httptest.Server{}
	for _, g := range []string{"g1", "g2"} {
		b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("lab-" + g)) }))
		t.Cleanup(b.Close)
		backends[g] = b
	}
	now := time.Now()
	h := NewHandler(staticKeys("acme", "k1", pub), testSecret, "challenges.example.com", "challenge",
		func(task, groupID string) (string, error) { return backends[groupID].URL, nil }).
		WithGroupTenant(func(string) (string, bool) { return "acme", true })
	h.now = func() time.Time { return now }

	hostA, hostB := "web-aaa11w.challenges.example.com", "web-bbb22w.challenges.example.com"
	jar, _ := cookiejar.New(nil)
	login := func(host, group string) {
		claims := baseHandoff(now)
		claims.Host, claims.GroupID = host, group
		req := httptest.NewRequest("GET", "https://"+host+AuthPath+"?t="+url.QueryEscape(signHandoff(t, priv, "k1", claims)), nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("handoff on %s: %d", host, rec.Code)
		}
		u, _ := url.Parse("https://" + host + "/")
		jar.SetCookies(u, rec.Result().Cookies())
	}
	login(hostA, "g1")
	login(hostB, "g2") // the second login must not replace the first

	get := func(host string) string {
		u, _ := url.Parse("https://" + host + "/")
		req := httptest.NewRequest("GET", u.String(), nil)
		req.Host = host
		for _, c := range jar.Cookies(u) { // what a browser sends to this host
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", host, rec.Code)
		}
		return rec.Body.String()
	}
	if got := get(hostA); got != "lab-g1" {
		t.Fatalf("host A serves group 1 with its own cookie, got %q", got)
	}
	if got := get(hostB); got != "lab-g2" {
		t.Fatalf("host B serves group 2 with its own cookie, got %q", got)
	}
	for _, host := range []string{hostA, hostB} {
		u, _ := url.Parse("https://" + host + "/")
		if n := len(jar.Cookies(u)); n != 1 {
			t.Fatalf("the browser holds one cookie for %s, got %d", host, n)
		}
	}
	// a host the browser never logged in to gets no cookie, and so no session
	other := "web-ccc33w.challenges.example.com"
	req := httptest.NewRequest("GET", "https://"+other+"/", nil)
	req.Host = other
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a host with no cookie: %d", rec.Code)
	}
	u, _ := url.Parse("https://" + other + "/")
	if len(jar.Cookies(u)) != 0 {
		t.Fatal("a cookie of another device host leaked to a third host")
	}
}

// The session names the one host it was opened for: what leaks opens at most that device. A cookie of host A sent to host B (the same
// group, a real session) is refused, and so is one without a host (issued before the binding).
func TestSession_IsBoundToItsHost(t *testing.T) {
	f := newHandoffFixture(t)
	f.handler.WithLimits(DefaultHandoffLifetime, 10*time.Hour, 2*time.Hour, 24*time.Hour)
	c := f.open(f.link(t, func(c *handoffClaims) { c.Session = f.now.Add(1000 * time.Hour).Unix() })).Result().Cookies()[0]
	claims, err := validateCookie(requestWithCookie(c.Value), testSecret, "challenge", f.handler.now)
	if err != nil || claims.Host != labHost {
		t.Fatalf("the handoff binds the session to the host: %+v %v", claims, err)
	}
	get := func(host string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "https://"+host+"/", nil)
		req.Host = host
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := get(labHost+":8443", c); rec.Code != 200 {
		t.Fatalf("its own host (with a port): %d", rec.Code)
	}
	for _, other := range []string{"web-abc12t.challenges.example.com", "web-zzz99w.challenges.example.com"} {
		if rec := get(other, c); rec.Code != http.StatusUnauthorized {
			t.Errorf("the cookie of %s opened %s: %d", labHost, other, rec.Code)
		}
	}
	// renewal keeps the host, and a renewed cookie is still refused elsewhere
	f.now = f.now.Add(9 * time.Hour)
	rec := get(labHost, c)
	renewed := rec.Result().Cookies()
	if rec.Code != 200 || len(renewed) != 1 {
		t.Fatalf("renewal: %d %v", rec.Code, renewed)
	}
	if cl, err := validateCookie(requestWithCookie(renewed[0].Value), testSecret, "challenge", f.handler.now); err != nil || cl.Host != labHost {
		t.Fatalf("the renewed session names the same host: %+v %v", cl, err)
	}
	if rec := get("web-zzz99w.challenges.example.com", renewed[0]); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("a renewed cookie on another host: %d %v", rec.Code, rec.Result().Cookies())
	}
	// a session cookie that names no host (the old kind) is no session
	old := jwtClaims{GroupID: "g1", Client: "p-u1", Tenant: "acme", Abs: f.now.Add(time.Hour).Unix(),
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(f.now.Add(time.Hour))}}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, old).SignedString(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if rec := get(labHost, &http.Cookie{Name: "challenge", Value: tok}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a cookie without a host: %d", rec.Code)
	}
}

func (f *handoffFixture) openFrom(addr, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "https://"+labHost+AuthPath+"?t="+url.QueryEscape(token), nil)
	req.Host, req.RemoteAddr = labHost, addr
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// A garbage flood from one address is stopped before any signature is verified, and does not touch another address' valid link.
func TestHandoff_GarbageFloodIsThrottledBeforeVerification(t *testing.T) {
	f := newHandoffFixture(t)
	f.handler.WithAuthRateLimit(1, 3)
	limited := 0
	for i := 0; i < 100; i++ {
		if f.openFrom("198.51.100.7:4000", "garbage").Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited < 100-preAuthBurst-10 {
		t.Fatalf("only %d of 100 garbage attempts were throttled", limited)
	}
	if rec := f.openFrom("203.0.113.9:5000", f.link(t, nil)); rec.Code != http.StatusSeeOther {
		t.Fatalf("a valid link of another address after the flood: %d", rec.Code)
	}
	if rec := f.openFrom("198.51.100.7:4000", f.link(t, nil)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the flooding address is still over its bucket: %d", rec.Code)
	}
}

// Valid links of one session from one address spend the (lab, address, session) bucket, and only that one.
func TestHandoff_ValidLinkFloodIsLimitedPerSession(t *testing.T) {
	f := newHandoffFixture(t)
	f.handler.WithAuthRateLimit(0.001, 3)
	good := f.link(t, nil)
	limited := false
	for i := 0; i < 10; i++ {
		if f.openFrom("198.51.100.7:4000", good).Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("a flood of valid links of one session must be limited")
	}
	if rec := f.openFrom("203.0.113.10:5000", good); rec.Code != http.StatusSeeOther {
		t.Fatalf("another address: %d", rec.Code)
	}
}

// Whatever cookies an attacker sends, the limiter tables do not grow with them and never pass their cap.
func TestHandoff_AttackerChosenValuesDoNotGrowTheTables(t *testing.T) {
	f := newHandoffFixture(t)
	f.handler.WithAuthRateLimit(1, 3)
	for i := 0; i < 500; i++ {
		req := httptest.NewRequest("GET", "https://"+labHost+AuthPath+"?t=garbage", nil)
		req.Host, req.RemoteAddr = labHost, "198.51.100.7:4000"
		req.AddCookie(&http.Cookie{Name: "challenge", Value: fmt.Sprintf("c%d", i)})
		f.handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	if n := len(f.handler.preAuthLimit.keys); n != 1 {
		t.Fatalf("pre-verification buckets = %d, one per address", n)
	}
	if n := len(f.handler.authLimit.keys); n != 0 {
		t.Fatalf("post-verification buckets = %d: garbage must not create any", n)
	}
	// The cap holds against attacker-chosen keys.
	set := newBucketSet(1, 3, 50)
	for i := 0; i < 5000; i++ {
		set.allow(fmt.Sprintf("k%d", i))
	}
	if len(set.keys) > 50 {
		t.Fatalf("table = %d, cap 50", len(set.keys))
	}
}

// Garbage from a thousand addresses is not a lever on everybody: there is no bucket shared by all addresses before verification.
func TestHandoff_GarbageFromManyAddressesDoesNotBlockAFreshOne(t *testing.T) {
	f := newHandoffFixture(t)
	f.handler.WithAuthRateLimit(1, 3)
	for i := 0; i < 1000; i++ {
		for j := 0; j < 25; j++ { // each address spends its own whole budget
			if i < 40 {
				f.openFrom(fmt.Sprintf("10.%d.%d.%d:1", i/250, i%250, 1), "garbage")
			}
		}
		f.openFrom(fmt.Sprintf("10.9.%d.%d:1", i/250, i%250), "garbage")
	}
	if rec := f.openFrom("203.0.113.77:5000", f.link(t, nil)); rec.Code != http.StatusSeeOther {
		t.Fatalf("a valid link from a fresh address after the flood: %d", rec.Code)
	}
}

// One IPv6 network (a /64) is one client: rotating the host part does not give a fresh bucket; another /64 has its own.
func TestHandoff_IPv6ClientsAreKeyedByTheirSlash64(t *testing.T) {
	f := newHandoffFixture(t)
	f.handler.WithAuthRateLimit(1, 3)
	limited := false
	for i := 0; i < 60; i++ {
		if f.openFrom(fmt.Sprintf("[2001:db8:1:2::%x]:1", i+1), "garbage").Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("addresses of one /64 must share a bucket")
	}
	if f.openFrom("[2001:db8:1:2:ffff::9]:1", "garbage").Code != http.StatusTooManyRequests {
		t.Fatal("any address of the same /64 is over the same bucket")
	}
	if rec := f.openFrom("[2001:db8:1:3::1]:1", f.link(t, nil)); rec.Code != http.StatusSeeOther {
		t.Fatalf("another /64: %d", rec.Code)
	}
	if got := preAuthKeyOf("203.0.113.5:80"); got != "203.0.113.5" {
		t.Fatalf("IPv4 key = %q", got)
	}
}

func preAuthKeyOf(addr string) string {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = addr
	return preAuthClientIP(r)
}
