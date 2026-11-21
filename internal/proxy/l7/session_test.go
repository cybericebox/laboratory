package l7

import (
	"crypto/ed25519"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/cybericebox/laboratory/internal/grant"
	"github.com/cybericebox/laboratory/internal/grant/granttest"
)

const platformOrigin = "https://app.example.com"

type grantFixture struct {
	key     granttest.Key
	groups  map[string]string // group id -> owning tenant
	handler *Handler
	backend *httptest.Server
	now     time.Time
}

func newGrantFixture(t *testing.T) *grantFixture {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cookies" { // a device that tries to set the proxy's grant cookie, and one of its own
			http.SetCookie(w, &http.Cookie{Name: "challenge", Value: "forged", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "app", Value: "1", Path: "/"})
		}
		_, _ = w.Write([]byte("lab"))
	}))
	t.Cleanup(backend.Close)
	f := &grantFixture{key: testKey, backend: backend, now: time.Now(), groups: map[string]string{"g1": "acme"}}
	origins, err := NewEmbedMatcher([]string{platformOrigin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.handler = NewHandler(testKeys(), "challenges.example.com", "challenge",
		func(task, groupID string) (string, error) { return backend.URL, nil }).
		WithPlatformOrigins(origins).
		WithGroupTenant(func(groupID string) (string, bool) { owner, ok := f.groups[groupID]; return owner, ok })
	f.handler.now = func() time.Time { return f.now }
	return f
}

// baseGrant is a valid web grant of tenant acme for group g1 and the lab host, valid for an hour from f.now.
func (f *grantFixture) baseGrant() grant.Claims {
	c := f.key.Claims(f.now, time.Hour, labHost)
	c.Group, c.Subject = "g1", "p-u1"
	return c
}

func (f *grantFixture) grant(t *testing.T, mutate func(*grant.Claims)) string {
	t.Helper()
	c := f.baseGrant()
	if mutate != nil {
		mutate(&c)
	}
	return f.key.Sign(t, c)
}

// postTo posts the form a platform page submits: the grant and the destination, with the Origin of the platform page.
func (f *grantFixture) postTo(host, raw, next string, edit func(*http.Request)) *httptest.ResponseRecorder {
	form := url.Values{"grant": {raw}}
	if next != "" {
		form.Set("next", next)
	}
	req := httptest.NewRequest(http.MethodPost, "https://"+host+AuthPath, strings.NewReader(form.Encode()))
	req.Host = host
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", platformOrigin)
	if edit != nil {
		edit(req)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *grantFixture) open(raw string) *httptest.ResponseRecorder {
	return f.postTo(labHost, raw, "", nil)
}

func (f *grantFixture) openFrom(addr, raw string) *httptest.ResponseRecorder {
	return f.postTo(labHost, raw, "", func(r *http.Request) { r.RemoteAddr = addr })
}

func (f *grantFixture) get(host string, c *http.Cookie, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "https://"+host+path, nil)
	req.Host = host
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestAuthPost_SetsTheGrantCookieAndRedirects(t *testing.T) {
	f := newGrantFixture(t)
	raw := f.grant(t, nil)
	rec := f.open(raw)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("code = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("the answer must not be cached or leak a referrer: %v", rec.Header())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	c := cookies[0]
	if c.Name != "challenge" || c.Value != raw || c.Domain != "" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Fatalf("cookie = %+v", c)
	}
	if want := f.now.Add(time.Hour).Unix(); c.Expires.Unix() != want || c.MaxAge < 3599 || c.MaxAge > 3600 {
		t.Fatalf("expires %d max-age %d, want the grant end %d and about 3600", c.Expires.Unix(), c.MaxAge, want)
	}
	// The cookie then opens the lab.
	if next := f.get(labHost, c, "/"); next.Code != 200 || next.Body.String() != "lab" {
		t.Fatalf("lab request: %d %q", next.Code, next.Body.String())
	}
}

// A saved device link must reach its authored destination after authentication.
func TestAuthPost_NextPreservesDestination(t *testing.T) {
	f := newGrantFixture(t)
	raw := f.grant(t, nil)
	baseline := f.open(raw)
	next := "/panel?x=1#anchor"
	rec := f.postTo(labHost, raw, next, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != next {
		t.Fatalf("code = %d location = %q, want 303 %q", rec.Code, rec.Header().Get("Location"), next)
	}
	if strings.Contains(rec.Header().Get("Location"), raw) || strings.Contains(rec.Body.String(), raw) {
		t.Fatal("grant leaked in the redirect")
	}
	if rec.Header().Get("Set-Cookie") != baseline.Header().Get("Set-Cookie") {
		t.Fatal("the destination changed the cookie policy or lifetime")
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.RequestURI()))
	}))
	defer backend.Close()
	f.handler.resolver = func(task, groupID string) (string, error) { return backend.URL, nil }
	location, err := rec.Result().Location()
	if err != nil {
		t.Fatal(err)
	}
	// Browsers retain the fragment locally and omit it from the HTTP request.
	location.Fragment, location.RawFragment = "", ""
	out := f.get(labHost, rec.Result().Cookies()[0], location.RequestURI())
	if out.Code != http.StatusOK || out.Body.String() != "/panel?x=1" {
		t.Fatalf("device received %d %q, want /panel?x=1", out.Code, out.Body.String())
	}
}

func TestAuthPost_NextStaysOnDeviceOrigin(t *testing.T) {
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
			f := newGrantFixture(t)
			raw := f.grant(t, nil)
			rec := f.postTo(labHost, raw, tc.next, nil)
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != tc.want {
				t.Fatalf("code = %d location = %q, want 303 %q", rec.Code, rec.Header().Get("Location"), tc.want)
			}
			location, err := rec.Result().Location()
			if err != nil || location.Host != "" || location.User != nil {
				t.Fatalf("redirect changed device origin: %v (%v)", location, err)
			}
			if strings.Contains(location.String(), raw) || len(rec.Result().Cookies()) != 1 {
				t.Fatal("the redirect leaked the grant or failed to issue its cookie")
			}
		})
	}
}

func TestAuthPost_NextDoesNotBypassGrantVerification(t *testing.T) {
	for _, invalid := range []string{"missing", "bad", "expired"} {
		t.Run(invalid, func(t *testing.T) {
			f := newGrantFixture(t)
			raw := ""
			switch invalid {
			case "bad":
				raw = "bad-grant"
			case "expired":
				raw = f.grant(t, nil)
				f.now = f.now.Add(2 * time.Hour)
			}
			baseline := f.open(raw)
			rec := f.postTo(labHost, raw, "/panel?x=1#anchor", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "challenge", Value: "existing-session"}) })
			if rec.Code != http.StatusUnauthorized || rec.Header().Get("Location") != "" || len(rec.Result().Cookies()) != 0 {
				t.Fatalf("code = %d location = %q cookies = %v", rec.Code, rec.Header().Get("Location"), rec.Result().Cookies())
			}
			if rec.Body.String() != baseline.Body.String() || rec.Header().Get("Set-Cookie") != baseline.Header().Get("Set-Cookie") {
				t.Fatal("next changed the behavior for an invalid grant")
			}
		})
	}
}

// Every row of the claim table of the spec (2.3.1) is a refusal that sets no cookie and says nothing about the reason.
func TestAuthPost_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*grant.Claims)
	}{
		{"expired", func(c *grant.Claims) {
			c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-10 * time.Hour))
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-9 * time.Hour))
		}},
		{"another device host", func(c *grant.Claims) { c.Host = "db-abc12w.challenges.example.com" }},
		{"the first label only", func(c *grant.Claims) { c.Host = "web-abc12w" }},
		{"no host", func(c *grant.Claims) { c.Host = "" }},
		{"no client", func(c *grant.Claims) { c.Subject = "" }},
		{"another audience", func(c *grant.Claims) { c.Audience = jwt.ClaimStrings{"somewhere-else"} }},
		{"no audience", func(c *grant.Claims) { c.Audience = nil }},
		{"the device access audience only", func(c *grant.Claims) { c.Audience = jwt.ClaimStrings{grant.AudienceDeviceAccess} }},
		{"no not-before", func(c *grant.Claims) { c.NotBefore = nil }},
		{"no issued-at", func(c *grant.Claims) { c.IssuedAt = nil }},
		{"not valid yet", func(c *grant.Claims) { c.NotBefore = jwt.NewNumericDate(time.Now().Add(10 * time.Minute)) }},
		{"no issuer", func(c *grant.Claims) { c.Issuer = "" }},
		{"an unknown tenant", func(c *grant.Claims) { c.Issuer = "mallory" }},
		{"an issuer that is no name", func(c *grant.Claims) { c.Issuer = "../acme" }},
		{"a group that does not exist", func(c *grant.Claims) { c.Group = "nope" }},
		{"no group", func(c *grant.Claims) { c.Group = "" }},
		{"no lab", func(c *grant.Claims) { c.Lab = "" }},
		{"no device", func(c *grant.Claims) { c.Dev = "" }},
		{"a console grant on a web host", func(c *grant.Claims) { c.Kind = grant.KindConsole }},
		{"no kind", func(c *grant.Claims) { c.Kind = "" }},
		{"a grant over the lifetime cap", func(c *grant.Claims) { c.ExpiresAt = jwt.NewNumericDate(c.IssuedAt.Add(8*time.Hour + time.Second)) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newGrantFixture(t)
			rec := f.open(f.grant(t, c.mutate))
			if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
				t.Fatalf("code = %d cookies = %v", rec.Code, rec.Result().Cookies())
			}
		})
	}
}

func TestAuthPost_TamperedSignature(t *testing.T) {
	f := newGrantFixture(t)
	raw := f.grant(t, nil)
	parts := strings.Split(raw, ".")
	parts[1] = parts[1][:len(parts[1])-2] + "AA"
	if rec := f.open(strings.Join(parts, ".")); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("tampered payload: %d", rec.Code)
	}
	other := granttest.NewKey(t, "acme", "k1")
	if rec := f.open(other.Sign(t, f.baseGrant())); rec.Code != http.StatusUnauthorized {
		t.Fatalf("foreign key: %d", rec.Code)
	}
}

// The grant is stateless: the same grant opens again until its exp.
func TestAuthPost_IsStatelessAndOpensAgainUntilExp(t *testing.T) {
	f := newGrantFixture(t)
	raw := f.grant(t, nil)
	for i := 0; i < 2; i++ {
		if rec := f.open(raw); rec.Code != http.StatusSeeOther {
			t.Fatalf("open %d: %d", i, rec.Code)
		}
	}
	f.now = f.now.Add(2 * time.Hour)
	if rec := f.open(raw); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after exp: %d", rec.Code)
	}
}

// Only EdDSA counts: a grant signed with RSA, HMAC (any secret) or "none" is refused.
func TestAuthPost_RefusesOtherAlgorithms(t *testing.T) {
	f := newGrantFixture(t)
	claims := f.baseGrant()
	rs, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("a shared secret of the proxy, if there were one"))
	if err != nil {
		t.Fatal(err)
	}
	none := f.key.SignWith(t, claims, jwt.SigningMethodNone, "k1", grant.Type)
	for name, token := range map[string]string{"HS256 signed with the public key's bytes": rs, "none": none} {
		if rec := f.open(token); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
			t.Fatalf("%s: %d", name, rec.Code)
		}
	}
}

// The login is a POST of a form from the platform page, and nothing else is accepted on the path.
func TestAuthPost_OnlyAFormFromAPlatformOrigin(t *testing.T) {
	f := newGrantFixture(t)
	raw := f.grant(t, nil)
	refused := func(name string, rec *httptest.ResponseRecorder, code int) {
		t.Helper()
		if rec.Code != code || len(rec.Result().Cookies()) != 0 || rec.Header().Get("Location") != "" {
			t.Errorf("%s: code = %d (want %d) cookies = %v", name, rec.Code, code, rec.Result().Cookies())
		}
	}
	refused("no Origin", f.postTo(labHost, raw, "", func(r *http.Request) { r.Header.Del("Origin") }), http.StatusUnauthorized)
	refused("another site's Origin", f.postTo(labHost, raw, "", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }), http.StatusUnauthorized)
	refused("the null Origin", f.postTo(labHost, raw, "", func(r *http.Request) { r.Header.Set("Origin", "null") }), http.StatusUnauthorized)
	refused("the lab host's own Origin", f.postTo(labHost, raw, "", func(r *http.Request) { r.Header.Set("Origin", "https://"+labHost) }), http.StatusUnauthorized)
	refused("an http Origin", f.postTo(labHost, raw, "", func(r *http.Request) { r.Header.Set("Origin", "http://app.example.com") }), http.StatusUnauthorized)
	refused("a JSON body", f.postTo(labHost, raw, "", func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }), http.StatusUnsupportedMediaType)
	refused("no content type", f.postTo(labHost, raw, "", func(r *http.Request) { r.Header.Del("Content-Type") }), http.StatusUnsupportedMediaType)
	refused("a PUT", f.postTo(labHost, raw, "", func(r *http.Request) { r.Method = http.MethodPut }), http.StatusMethodNotAllowed)
	refused("a DELETE", f.postTo(labHost, raw, "", func(r *http.Request) { r.Method = http.MethodDelete }), http.StatusMethodNotAllowed)
	refused("a grant of more than 4096 bytes", f.postTo(labHost, strings.Repeat("a", grant.MaxLen+1), "", nil), http.StatusUnauthorized)
	refused("a body over 8 KiB", f.postTo(labHost, strings.Repeat("a", 9000), "", nil), http.StatusRequestEntityTooLarge)
	refused("a next field over 2048 bytes", f.postTo(labHost, raw, "/"+strings.Repeat("a", nextMax), nil), http.StatusUnauthorized)
	// the grant only in the query string is a link of the old shape
	refused("the grant in the query", f.postTo(labHost, "", "", func(r *http.Request) { r.URL.RawQuery = "grant=" + url.QueryEscape(raw) }), http.StatusUnauthorized)

	// an empty list of origins refuses every post
	f.handler.WithPlatformOrigins(nil)
	refused("no origins configured", f.open(raw), http.StatusUnauthorized)
	empty, _ := NewEmbedMatcher(nil, nil)
	f.handler.WithPlatformOrigins(empty)
	refused("an empty list", f.open(raw), http.StatusUnauthorized)
}

// A wildcard entry of the list matches one label, as the embedding lists do.
func TestAuthPost_OriginListUnderstandsWildcards(t *testing.T) {
	f := newGrantFixture(t)
	m, err := NewEmbedMatcher([]string{"https://*.example.org", "https://catalog.example.com:8443"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.handler.WithPlatformOrigins(m)
	raw := f.grant(t, nil)
	for origin, want := range map[string]int{
		"https://events.example.org":         http.StatusSeeOther,
		"https://catalog.example.com:8443":   http.StatusSeeOther,
		"https://a.b.example.org":            http.StatusUnauthorized,
		"https://example.org":                http.StatusUnauthorized,
		"https://catalog.example.com":        http.StatusUnauthorized,
		"https://app.example.com":            http.StatusUnauthorized,
		"https://events.example.org.evil.io": http.StatusUnauthorized,
	} {
		if rec := f.postTo(labHost, raw, "", func(r *http.Request) { r.Header.Set("Origin", origin) }); rec.Code != want {
			t.Errorf("Origin %s: %d, want %d", origin, rec.Code, want)
		}
	}
}

// The old link /_auth?t=<token> is a GET: it consumes nothing and fails closed, whatever it carries.
func TestAuthGet_FailsClosed(t *testing.T) {
	f := newGrantFixture(t)
	raw := f.grant(t, nil)
	for _, target := range []string{AuthPath, AuthPath + "?t=" + url.QueryEscape(raw), AuthPath + "?grant=" + url.QueryEscape(raw) + "&next=/x"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			req := httptest.NewRequest(method, "https://"+labHost+target, nil)
			req.Host = labHost
			req.Header.Set("Origin", platformOrigin)
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 || rec.Header().Get("Location") != "" {
				t.Fatalf("%s %s: %d %v", method, target, rec.Code, rec.Result().Cookies())
			}
		}
	}
}

func TestExpiredCard_NoCookieShowsTheCardWithoutRedirect(t *testing.T) {
	f := newGrantFixture(t)
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
	f := newGrantFixture(t)
	c := f.open(f.grant(t, nil)).Result().Cookies()[0]
	f.now = f.now.Add(2 * time.Hour)
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

// Two cookies of the grant's name mean one was not set by this proxy (a sibling site, a device): the request carries no grant, whichever the
// browser lists first.
func TestOneGrantCookieOnly(t *testing.T) {
	f := newGrantFixture(t)
	c := f.open(f.grant(t, nil)).Result().Cookies()[0]
	get := func(cookies ...*http.Cookie) int {
		req := httptest.NewRequest("GET", "https://"+labHost+"/", nil)
		req.Host = labHost
		for _, c := range cookies {
			req.AddCookie(c)
		}
		req.Header.Set("Accept", "text/html")
		out := httptest.NewRecorder()
		f.handler.ServeHTTP(out, req)
		return out.Code
	}
	if code := get(c); code == http.StatusUnauthorized {
		t.Fatalf("the cookie of the proxy is refused: %d", code)
	}
	if code := get(&http.Cookie{Name: c.Name, Value: "forged"}, c); code != http.StatusUnauthorized {
		t.Fatalf("two cookies of the name: %d", code)
	}
	if code := get(c, &http.Cookie{Name: c.Name, Value: "forged"}); code != http.StatusUnauthorized {
		t.Fatalf("two cookies of the name, ours first: %d", code)
	}
}

// The keys of the issuing tenant, by key id: a rotation overlaps (both keys work), a removed key stops, an unknown kid is refused.
func TestAuthPost_KeysByTenantAndKeyID(t *testing.T) {
	f := newGrantFixture(t)
	k2 := granttest.NewKey(t, "acme", "k2")
	f.handler.keys = lookupOf(map[string]granttest.Key{"acme/k1": f.key, "acme/k2": k2})
	open := func(k granttest.Key, kid string) int {
		return f.open(k.SignWith(t, f.baseGrant(), jwt.SigningMethodEdDSA, kid, grant.Type)).Code
	}
	if open(f.key, "k1") != http.StatusSeeOther || open(k2, "k2") != http.StatusSeeOther {
		t.Fatal("during a rotation both keys work")
	}
	if open(f.key, "k2") != http.StatusUnauthorized {
		t.Fatal("a key under another id's name is refused")
	}
	if open(f.key, "ghost") != http.StatusUnauthorized {
		t.Fatal("an unknown kid is refused")
	}
	if open(f.key, "") != http.StatusUnauthorized {
		t.Fatal("a grant with no kid is refused")
	}
	f.handler.keys = lookupOf(map[string]granttest.Key{"acme/k2": k2}) // the old key is removed
	if open(f.key, "k1") != http.StatusUnauthorized || open(k2, "k2") != http.StatusSeeOther {
		t.Fatal("after the removal only the new key works")
	}
}

func lookupOf(keys map[string]granttest.Key) KeyLookup {
	return func(tenant, kid string) (ed25519.PublicKey, bool) {
		k, ok := keys[tenant+"/"+kid]
		return k.Public, ok
	}
}

// CRUCIAL: a tenant cannot open a group of another tenant, with a perfectly valid signature of its own.
func TestAuthPost_RefusesAGroupOfAnotherTenant(t *testing.T) {
	f := newGrantFixture(t)
	f.groups["theirs"] = "mallory"
	rec := f.open(f.grant(t, func(c *grant.Claims) { c.Group = "theirs" }))
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("cross-tenant grant: %d", rec.Code)
	}
	// The other tenant's own key does not help either: it is not issuer acme's key.
	mallory := granttest.NewKey(t, "mallory", "k1")
	f.handler.keys = lookupOf(map[string]granttest.Key{"acme/k1": f.key, "mallory/k1": mallory})
	claims := f.baseGrant()
	claims.Issuer, claims.Group = "mallory", "g1" // g1 is acme's
	if rec := f.open(mallory.Sign(t, claims)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("mallory signing for acme's group: %d", rec.Code)
	}
	claims.Group = "theirs"
	if rec := f.open(mallory.Sign(t, claims)); rec.Code != http.StatusSeeOther {
		t.Fatalf("mallory for its own group: %d", rec.Code)
	}
}

// A session dies with the group's ownership: the group now belongs to someone else.
func TestSession_StopsWhenTheGroupChangesOwner(t *testing.T) {
	f := newGrantFixture(t)
	c := f.open(f.grant(t, nil)).Result().Cookies()[0]
	get := func() int { return f.get(labHost, c, "/").Code }
	if get() != 200 {
		t.Fatal("the session opens the lab")
	}
	f.groups["g1"] = "mallory" // at once: the cache holds a verified signature, never an ownership
	if get() != http.StatusNotFound {
		t.Fatal("a session of a group that changed owner must stop")
	}
	delete(f.groups, "g1")
	if code := get(); code != http.StatusNotFound {
		t.Fatalf("and so must a session of a group that is gone: %d", code)
	}
}

// The cookie lives until the grant ends and no longer: there is no sliding, no renewal and no idle lifetime.
func TestSession_EndsAtTheGrantExp(t *testing.T) {
	f := newGrantFixture(t)
	c := f.open(f.grant(t, nil)).Result().Cookies()[0]
	start := f.now
	f.now = start.Add(59 * time.Minute)
	if rec := f.get(labHost, c, "/"); rec.Code != 200 || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("just before exp: %d, no renewal expected: %v", rec.Code, rec.Result().Cookies())
	}
	f.now = start.Add(time.Hour + 31*time.Second) // past exp and the clock leeway
	if rec := f.get(labHost, c, "/"); rec.Code == 200 {
		t.Fatal("past the end of the grant the session is over")
	}
}

// The grant lifetime limit is the configured one.
func TestAuthPost_GrantLifetimeLimitIsConfigured(t *testing.T) {
	f := newGrantFixture(t)
	long := f.grant(t, func(c *grant.Claims) { c.ExpiresAt = jwt.NewNumericDate(f.now.Add(6 * time.Hour)) })
	if rec := f.open(long); rec.Code != http.StatusSeeOther {
		t.Fatalf("6h under the default 8h: %d", rec.Code)
	}
	over := f.grant(t, func(c *grant.Claims) { c.ExpiresAt = jwt.NewNumericDate(f.now.Add(9 * time.Hour)) })
	if rec := f.open(over); rec.Code == http.StatusSeeOther {
		t.Fatal("9h is over the default 8h limit and must be refused")
	}
	f.handler.WithGrantMaxTTL(time.Hour)
	if rec := f.open(long); rec.Code == http.StatusSeeOther {
		t.Fatal("6h over a 1h limit must be refused")
	}
}

// A device cannot set, replace or clear the proxy's grant cookie; its own cookies pass.
func TestDeviceCannotSetTheGrantCookie(t *testing.T) {
	f := newGrantFixture(t)
	c := f.open(f.grant(t, nil)).Result().Cookies()[0]
	rec := f.get(labHost, c, "/cookies")
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var names []string
	for _, sc := range rec.Result().Cookies() {
		names = append(names, sc.Name+"="+sc.Value)
	}
	if len(names) != 1 || names[0] != "app=1" {
		t.Fatalf("the response cookies are %v: the grant cookie must be stripped, the device's own kept", names)
	}
}

// A grant opens ONE device: its host claim is the whole host name, and the same group's other hosts refuse it.
func TestAuthPost_IsBoundToTheFullHostName(t *testing.T) {
	f := newGrantFixture(t)
	link := f.grant(t, nil) // host = web-abc12w.challenges.example.com
	for _, host := range []string{"web-abc12t.challenges.example.com", "other-xyz12w.challenges.example.com", "web-abc12w.other.example.com"} {
		if rec := f.postTo(host, link, "", nil); rec.Code == http.StatusSeeOther || len(rec.Result().Cookies()) != 0 {
			t.Errorf("a grant for %s opened on %s: %d %v", labHost, host, rec.Code, rec.Result().Cookies())
		}
	}
	// the right host with a port is still the same host
	if rec := f.postTo(labHost+":8443", link, "", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("the same host with a port: %d", rec.Code)
	}
}

// Sessions of two groups live side by side in one browser, one cookie per device host, and each opens only its own host.
func TestSession_CookiesOfTwoGroupsCoexistPerHost(t *testing.T) {
	f := newGrantFixture(t)
	backends := map[string]*httptest.Server{}
	for _, g := range []string{"g1", "g2"} {
		b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("lab-" + g)) }))
		t.Cleanup(b.Close)
		backends[g] = b
	}
	f.groups["g2"] = "acme"
	f.handler.resolver = func(task, groupID string) (string, error) { return backends[groupID].URL, nil }

	hostA, hostB := "web-aaa11w.challenges.example.com", "web-bbb22w.challenges.example.com"
	jar, _ := cookiejar.New(nil)
	login := func(host, group string) {
		raw := f.grant(t, func(c *grant.Claims) { c.Host, c.Group = host, group })
		rec := f.postTo(host, raw, "", nil)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("grant on %s: %d", host, rec.Code)
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
		f.handler.ServeHTTP(rec, req)
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
	if rec := f.get(other, nil, "/"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a host with no cookie: %d", rec.Code)
	}
	u, _ := url.Parse("https://" + other + "/")
	if len(jar.Cookies(u)) != 0 {
		t.Fatal("a cookie of another device host leaked to a third host")
	}
}

// The grant names the one host it was issued for: what leaks opens at most that device. A cookie of host A sent to host B (the same group,
// a real session) is refused, and so is one for a host whose kind is not web.
func TestSession_IsBoundToItsHost(t *testing.T) {
	f := newGrantFixture(t)
	c := f.open(f.grant(t, nil)).Result().Cookies()[0]
	if rec := f.get(labHost+":8443", c, "/"); rec.Code != 200 {
		t.Fatalf("its own host (with a port): %d", rec.Code)
	}
	if rec := f.get("web-zzz99w.challenges.example.com", c, "/"); rec.Code != http.StatusUnauthorized {
		t.Errorf("the cookie of %s opened another host: %d", labHost, rec.Code)
	}
	// a console host while no console is served is no host at all
	if rec := f.get("web-abc12t.challenges.example.com", c, "/"); rec.Code != http.StatusNotFound {
		t.Errorf("a console host without a console: %d", rec.Code)
	}
	// a web grant minted for a console host (no console is served here) opens nothing
	consoleHost := "web-abc12t.challenges.example.com"
	raw := f.grant(t, func(c *grant.Claims) { c.Host = consoleHost })
	cookie := &http.Cookie{Name: "challenge", Value: raw}
	if rec := f.get(consoleHost, cookie, "/"); rec.Code == 200 {
		t.Fatalf("a web grant opened a console-letter host: %d", rec.Code)
	}
}

// A verified cookie is remembered for a short time only: a key the tenant removed stops working after that time, and not later.
func TestSession_RemovedKeyStopsWithinTheCacheTime(t *testing.T) {
	f := newGrantFixture(t)
	c := f.open(f.grant(t, nil)).Result().Cookies()[0]
	if f.get(labHost, c, "/").Code != 200 {
		t.Fatal("the session opens the lab")
	}
	f.handler.keys = lookupOf(nil) // RemoveAccessKey
	f.now = f.now.Add(grantCacheTTL - time.Second)
	if f.get(labHost, c, "/").Code != 200 {
		t.Fatal("inside the cache time the verified cookie is still accepted")
	}
	f.now = f.now.Add(2 * time.Second)
	if code := f.get(labHost, c, "/").Code; code != http.StatusUnauthorized {
		t.Fatalf("a removed key must stop working after the cache time: %d", code)
	}
}

// The cache never keeps a verification past the end of the grant.
func TestSession_CacheDoesNotOutliveTheGrant(t *testing.T) {
	f := newGrantFixture(t)
	short := f.grant(t, func(c *grant.Claims) { c.ExpiresAt = jwt.NewNumericDate(f.now.Add(10 * time.Second)) })
	c := f.open(short).Result().Cookies()[0]
	if f.get(labHost, c, "/").Code != 200 {
		t.Fatal("the session opens the lab")
	}
	f.now = f.now.Add(45 * time.Second) // past exp and the leeway, inside the cache time
	if code := f.get(labHost, c, "/").Code; code != http.StatusUnauthorized {
		t.Fatalf("a cached verification must not outlive exp: %d", code)
	}
}

// The cache is bounded and does not turn a full table into a refusal.
func TestGrantCache_IsBounded(t *testing.T) {
	c := newGrantCache()
	now := time.Now()
	g := sessionGrant{}
	g.ExpiresAt = jwt.NewNumericDate(now.Add(time.Hour))
	for i := 0; i < grantCacheMax+500; i++ {
		c.put(grantCacheKey("h", fmt.Sprint(i)), g, now)
	}
	if len(c.entries) != grantCacheMax {
		t.Fatalf("entries = %d, cap %d", len(c.entries), grantCacheMax)
	}
	// expired entries are dropped to make room
	later := now.Add(grantCacheTTL + time.Second)
	c.put(grantCacheKey("h", "new"), g, later)
	if _, ok := c.get(grantCacheKey("h", "new"), later); !ok {
		t.Fatal("a full table of expired entries makes room")
	}
}

// A garbage flood from one address is stopped before any signature is verified, and does not touch another address' valid grant.
func TestAuthPost_GarbageFloodIsThrottledBeforeVerification(t *testing.T) {
	f := newGrantFixture(t)
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
	if rec := f.openFrom("203.0.113.9:5000", f.grant(t, nil)); rec.Code != http.StatusSeeOther {
		t.Fatalf("a valid grant of another address after the flood: %d", rec.Code)
	}
	if rec := f.openFrom("198.51.100.7:4000", f.grant(t, nil)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the flooding address is still over its bucket: %d", rec.Code)
	}
}

// Valid grants of one session from one address spend the (lab, address, session) bucket, and only that one.
func TestAuthPost_ValidGrantFloodIsLimitedPerSession(t *testing.T) {
	f := newGrantFixture(t)
	f.handler.WithAuthRateLimit(0.001, 3)
	good := f.grant(t, nil)
	limited := false
	for i := 0; i < 10; i++ {
		if f.openFrom("198.51.100.7:4000", good).Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("a flood of valid grants of one session must be limited")
	}
	if rec := f.openFrom("203.0.113.10:5000", good); rec.Code != http.StatusSeeOther {
		t.Fatalf("another address: %d", rec.Code)
	}
}

// Whatever cookies an attacker sends, the limiter tables do not grow with them and never pass their cap.
func TestAuthPost_AttackerChosenValuesDoNotGrowTheTables(t *testing.T) {
	f := newGrantFixture(t)
	f.handler.WithAuthRateLimit(1, 3)
	for i := 0; i < 500; i++ {
		f.postTo(labHost, "garbage", "", func(r *http.Request) {
			r.RemoteAddr = "198.51.100.7:4000"
			r.AddCookie(&http.Cookie{Name: "challenge", Value: fmt.Sprintf("c%d", i)})
		})
	}
	if n := len(f.handler.preAuthFine.keys); n != 1 {
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
func TestAuthPost_GarbageFromManyAddressesDoesNotBlockAFreshOne(t *testing.T) {
	f := newGrantFixture(t)
	f.handler.WithAuthRateLimit(1, 3)
	for i := 0; i < 1000; i++ {
		for j := 0; j < 25; j++ { // each address spends its own whole budget
			if i < 40 {
				f.openFrom(fmt.Sprintf("10.%d.%d.%d:1", i/250, i%250, 1), "garbage")
			}
		}
		f.openFrom(fmt.Sprintf("10.9.%d.%d:1", i/250, i%250), "garbage")
	}
	if rec := f.openFrom("203.0.113.77:5000", f.grant(t, nil)); rec.Code != http.StatusSeeOther {
		t.Fatalf("a valid grant from a fresh address after the flood: %d", rec.Code)
	}
}

// One IPv6 network (a /64) is one client: rotating the host part does not give a fresh bucket; another /64 has its own.
func TestAuthPost_IPv6ClientsAreKeyedByTheirSlash64(t *testing.T) {
	f := newGrantFixture(t)
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
	if rec := f.openFrom("[2001:db8:1:3::1]:1", f.grant(t, nil)); rec.Code != http.StatusSeeOther {
		t.Fatalf("another /64: %d", rec.Code)
	}
	if got := preAuthKeyOf("203.0.113.5:80"); got != "203.0.113.5/32" {
		t.Fatalf("IPv4 key = %q", got)
	}
}

func preAuthKeyOf(addr string) string {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = addr
	return preAuthClientIP(r)
}

// A request in flight ends when the grant that authorised it ends: the live check cuts it at exp.
func TestSession_InFlightRequestEndsAtTheGrantExp(t *testing.T) {
	f := newGrantFixture(t)
	started := make(chan struct{})
	hold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("x"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done() // held until the proxy cuts the request
	}))
	defer hold.Close()
	f.handler.resolver = func(task, groupID string) (string, error) { return hold.URL, nil }
	raw := f.grant(t, nil)
	c := f.open(raw).Result().Cookies()[0]

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.handler.ServeHTTP(w, r) }))
	defer srv.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest("GET", srv.URL+"/", nil)
		req.Host = labHost
		req.AddCookie(c)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not reach the device")
	}
	if n := f.handler.CheckLive(); n != 0 {
		t.Fatalf("closed %d while the grant is valid", n)
	}
	f.now = f.now.Add(time.Hour + time.Second) // the grant has ended
	if n := f.handler.CheckLive(); n != 1 {
		t.Fatalf("closed %d at the end of the grant, want 1", n)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the request outlived its grant")
	}
}

// The grant never reaches the device: the upstream is a participant's server, and HttpOnly does not hide a cookie from the server it is sent to.
// Only the device's own cookies pass; nothing in a forwarded header, query or Referer carries the grant; the post to /_auth is never proxied.
func TestGrantNeverReachesTheDevice(t *testing.T) {
	f := newGrantFixture(t)
	raw := f.grant(t, nil)
	var mu sync.Mutex
	var seen []http.Header
	var paths []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen, paths = append(seen, r.Header.Clone()), append(paths, r.URL.RequestURI())
		mu.Unlock()
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte("lab"))
	}))
	defer backend.Close()
	f.handler.resolver = func(task, groupID string) (string, error) { return backend.URL, nil }
	c := f.open(raw).Result().Cookies()[0]
	srv := httptest.NewServer(f.handler)
	defer srv.Close()

	// a plain request: the grant cookie among the device's own, two Cookie headers (a second cookie of the grant's name is refused: TestOneGrantCookieOnly), a Referer from the platform
	req, _ := http.NewRequest("GET", srv.URL+"/page?x=1", nil)
	req.Host = labHost
	req.Header.Add("Cookie", "app=1; "+c.Name+"="+c.Value)
	req.Header.Add("Cookie", "other=2")
	req.Header.Set("Referer", "https://app.example.com/tasks/1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	_ = resp.Body.Close()

	// a WebSocket upgrade
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nCookie: app=1; %s=%s\r\n\r\n", labHost, c.Name, c.Value)
	buf := make([]byte, 512)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _ := conn.Read(buf)
	_ = conn.Close()
	if !strings.Contains(string(buf[:n]), "101") {
		t.Fatalf("the upgrade did not reach the device: %q", buf[:n])
	}

	// the post that delivers a grant is answered by the proxy, never forwarded
	post := f.postTo(labHost, raw, "/after", nil)
	if post.Code != http.StatusSeeOther {
		t.Fatalf("post: %d", post.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("the device saw %d requests (paths %v), want the page and the upgrade only", len(seen), paths)
	}
	for i, h := range seen {
		for k, vs := range h {
			for _, v := range vs {
				if strings.Contains(v, raw) || (k == "Cookie" && strings.Contains(v, c.Name+"=")) {
					t.Errorf("request %d: header %s carries the grant: %q", i, k, v)
				}
			}
		}
		if !strings.Contains(h.Get("Cookie"), "app=1") {
			t.Errorf("request %d: the device's own cookie was lost: %q", i, h.Get("Cookie"))
		}
	}
	for _, p := range paths {
		if strings.Contains(p, raw) || strings.Contains(p, AuthPath) {
			t.Errorf("a forwarded path names the grant or the auth path: %q", p)
		}
	}
	if !strings.Contains(seen[0].Get("Cookie"), "other=2") {
		t.Errorf("the second Cookie header's own cookie was lost: %q", seen[0].Get("Cookie"))
	}
}
