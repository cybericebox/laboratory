package l7

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const labHost = "web-abc123.challenges.example.com"

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
		GroupID: "g1", Host: "web-abc123", Session: now.Add(24 * time.Hour).Unix(),
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
	if c.Name != "challenge" || c.Domain != "challenges.example.com" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
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
	for _, tc := range []struct{ lang, first string }{
		{"", "Сесія завершилася."},
		{"uk-UA,uk;q=0.9", "Сесія завершилася."},
		{"en-US,en;q=0.9", "The session has ended."},
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
		if !strings.Contains(body, tc.first) || !strings.Contains(body, "Відкрийте лабораторію ще раз за посиланням із завдання.") || !strings.Contains(body, "Open the lab again") {
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
	if out.Code != http.StatusUnauthorized || !strings.Contains(out.Body.String(), "Сесія завершилася.") {
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
	if get() != http.StatusForbidden {
		t.Fatal("a session of a group that changed owner must stop")
	}
	delete(f.groups, "g1")
	if get() != http.StatusForbidden {
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
