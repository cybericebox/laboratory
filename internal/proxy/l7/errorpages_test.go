package l7

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const errHost = "web-abc123.challenges.example.com"

func errHandler(t *testing.T, resolve func(task, groupID string) (string, error)) *Handler {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	return NewHandler(staticKeys("acme", "k1", pub), testSecret, "challenges.example.com", "challenge", resolve)
}

func errCall(t *testing.T, h *Handler, host, accept, lang string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "http://"+host+"/", nil)
	req.Host = host
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	req.AddCookie(&http.Cookie{Name: "challenge", Value: signCookie(t, jwtClaims{GroupID: "g1", Client: "p-u1",
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestErrorPages_GoneIsTheSameCardForUnknownAndForbidden(t *testing.T) {
	unknown := errHandler(t, func(string, string) (string, error) { return "", http.ErrMissingFile })
	forbidden := errHandler(t, func(string, string) (string, error) { return "http://127.0.0.1:1", nil }).
		WithAccounting(NewMeter("b", time.Now()), func(string, string) (string, bool) { return "c-1", true }).
		WithAuthorizer(func(string, string, string) bool { return false })

	var bodies []string
	for name, tc := range map[string]struct {
		h    *Handler
		code int
	}{"unknown": {unknown, 404}, "forbidden": {forbidden, 404}} {
		rec := errCall(t, tc.h, errHost, "text/html", "")
		if rec.Code != tc.code || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Header().Get("Content-Type"))
		}
		b := rec.Body.String()
		if !strings.Contains(b, "Такого завдання зараз немає") || !strings.Contains(b, "Можливо, лабораторію вже вимкнено або посилання застаріло.") {
			t.Fatalf("%s: %s", name, b)
		}
		for _, leak := range []string{"abc123", "challenges.example.com", "p-u1", "127.0.0.1"} {
			if strings.Contains(b, leak) {
				t.Fatalf("%s leaks %q", name, leak)
			}
		}
		bodies = append(bodies, b)
	}
	if bodies[0] != bodies[1] {
		t.Fatal("the cards differ between the cases")
	}
}

func TestErrorPages_EnglishWhenAsked(t *testing.T) {
	h := errHandler(t, func(string, string) (string, error) { return "", http.ErrMissingFile })
	rec := errCall(t, h, errHost, "text/html", "en-US,en;q=0.9")
	if !strings.Contains(rec.Body.String(), "This task is not available right now") {
		t.Fatal(rec.Body.String())
	}
}

func TestErrorPages_PlainForNonHTMLClients(t *testing.T) {
	h := errHandler(t, func(string, string) (string, error) { return "", http.ErrMissingFile })
	for _, accept := range []string{"", "application/json", "*/*"} {
		rec := errCall(t, h, errHost, accept, "")
		if rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != "not found" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("accept %q: %d %q %s", accept, rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
		}
	}
}

func TestErrorPages_BadGatewayCard(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	resolve := func(string, string) (string, error) { return url, nil }
	for name, h := range map[string]*Handler{
		"plain proxy": errHandler(t, resolve),
		"metered": errHandler(t, resolve).
			WithAccounting(NewMeter("b", time.Now()), func(string, string) (string, bool) { return "c-1", true }),
	} {
		rec := errCall(t, h, errHost, "text/html", "")
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Сервіс завдання ще запускається або недоступний. Спробуйте за хвилину.") {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), url) {
			t.Fatalf("%s leaks the upstream address", name)
		}
		rec = errCall(t, h, errHost, "application/json", "")
		if rec.Code != http.StatusBadGateway || strings.TrimSpace(rec.Body.String()) != "bad gateway" {
			t.Fatalf("%s plain: %d %q", name, rec.Code, rec.Body.String())
		}
	}
}

func TestErrorPages_InvalidHostAndExpired(t *testing.T) {
	h := errHandler(t, nil)
	rec := errCall(t, h, "evil.com", "text/html", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Такого завдання зараз немає") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest("GET", "http://"+errHost+"/", nil)
	req.Host = errHost
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusUnauthorized || strings.TrimSpace(out.Body.String()) != "unauthorized" {
		t.Fatalf("expired, non-HTML: %d %q", out.Code, out.Body.String())
	}
}

func TestErrorPages_HaveInlineFavicon(t *testing.T) {
	h := errHandler(t, func(string, string) (string, error) { return "", http.ErrMissingFile })
	if b := errCall(t, h, errHost, "text/html", "").Body.String(); !strings.Contains(b, `<link rel="icon" type="image/png" href="data:image/png;base64,`) {
		t.Fatal(b)
	}
}

func TestErrorPages_CleanCopy(t *testing.T) {
	for _, p := range []page{pageExpired, pageGone, pageUpstream, pageBusy, pageFailed} {
		for _, c := range []cardText{p.uk, p.en} {
			if strings.HasSuffix(c.title, ".") || strings.Contains(c.title+c.hint, "..") || !strings.HasSuffix(c.hint, ".") || strings.Count(c.hint, ". ") > 1 {
				t.Fatalf("copy: %+v", c)
			}
		}
	}
}
