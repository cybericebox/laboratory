package l7

import (
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

type handoffFixture struct {
	priv    *rsa.PrivateKey
	handler *Handler
	backend *httptest.Server
	now     time.Time
}

func newHandoffFixture(t *testing.T) *handoffFixture {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("lab")) }))
	t.Cleanup(backend.Close)
	f := &handoffFixture{priv: priv, backend: backend, now: time.Now()}
	f.handler = NewHandler(func() *rsa.PublicKey { return &priv.PublicKey }, testSecret, "challenges.example.com", "challenge",
		func(task, groupID string) (string, error) { return backend.URL, nil }).WithTokenMode(ModePerUser)
	f.handler.now = func() time.Time { return f.now }
	return f
}

func (f *handoffFixture) link(t *testing.T, mutate func(*handoffClaims)) string {
	t.Helper()
	claims := handoffClaims{
		GroupID: "g1", Client: "p-u1", Host: "web-abc123", Session: f.now.Add(24 * time.Hour).Unix(), Version: HandoffVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			ID: "jti-1", IssuedAt: jwt.NewNumericDate(f.now), ExpiresAt: jwt.NewNumericDate(f.now.Add(2 * time.Minute)),
		},
	}
	if mutate != nil {
		mutate(&claims)
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(f.priv)
	if err != nil {
		t.Fatal(err)
	}
	return signed
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
		{"no client", func(c *handoffClaims) { c.Client = "" }},
		{"no jti", func(c *handoffClaims) { c.ID = "" }},
		{"link that lives too long", func(c *handoffClaims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(2 * time.Hour)) }},
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
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	signed, _ := jwt.NewWithClaims(jwt.SigningMethodRS256, handoffClaims{
		GroupID: "g1", Client: "p-u1", Host: "web-abc123", Session: f.now.Add(time.Hour).Unix(),
		RegisteredClaims: jwt.RegisteredClaims{ID: "x", IssuedAt: jwt.NewNumericDate(f.now), ExpiresAt: jwt.NewNumericDate(f.now.Add(time.Minute))},
	}).SignedString(other)
	if rec := f.open(signed); rec.Code != http.StatusUnauthorized {
		t.Fatalf("foreign key: %d", rec.Code)
	}
}

func TestHandoff_ReplayedJTIIsRefused(t *testing.T) {
	f := newHandoffFixture(t)
	token := f.link(t, nil)
	if rec := f.open(token); rec.Code != http.StatusSeeOther {
		t.Fatalf("first use: %d", rec.Code)
	}
	if rec := f.open(token); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("replay: %d", rec.Code)
	}
}

func TestReplayCache_ForgetsAfterExpiryAndStaysBounded(t *testing.T) {
	c := newReplayCache(2)
	now := time.Now()
	if !c.firstUse("a", now.Add(time.Minute), now) || c.firstUse("a", now.Add(time.Minute), now) {
		t.Fatal("a seen id must be refused")
	}
	if !c.firstUse("a", now.Add(3*time.Minute), now.Add(2*time.Minute)) {
		t.Fatal("an id past its exp is forgotten")
	}
	c.firstUse("b", now.Add(10*time.Minute), now)
	c.firstUse("c", now.Add(10*time.Minute), now)
	if c.order.Len() > 2 {
		t.Fatalf("cache grew to %d", c.order.Len())
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
		if tc.lang != "" {
			req.Header.Set("Accept-Language", tc.lang)
		}
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		body := rec.Body.String()
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("Location") != "" {
			t.Fatalf("%q: code = %d location = %q", tc.lang, rec.Code, rec.Header().Get("Location"))
		}
		if !strings.Contains(body, tc.first) || !strings.Contains(body, "Відкрийте лабораторію ще раз за посиланням із завдання.") || !strings.Contains(body, "Open the lab again") || !strings.Contains(body, "data:image/webp;base64,") {
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
	out := httptest.NewRecorder()
	f.handler.ServeHTTP(out, req)
	if out.Code != http.StatusUnauthorized || !strings.Contains(out.Body.String(), "Сесія завершилася.") {
		t.Fatalf("expired cookie: %d %s", out.Code, out.Body.String())
	}
}
