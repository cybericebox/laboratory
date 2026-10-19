package l7

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// refusedURL is an address nobody listens on: the lab's pod is not Ready, the connection is refused.
func refusedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr
}

func TestStartingPage_RefusedConnectionIsNotAnError(t *testing.T) {
	url := refusedURL(t)
	h := errHandler(t, func(string, string) (string, error) { return url, nil })
	rec := errCall(t, h, errHost, "text/html", "")
	b := rec.Body.String()
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "4" {
		t.Fatalf("status %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	for _, want := range []string{"Лабораторія запускається…", "Сторінка оновиться сама.", `<meta http-equiv="refresh" content="4">`, `class="ic ic-loading"`, `class="rule"`} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, leak := range []string{"127.0.0.1", "refused", "abc123"} {
		if strings.Contains(b, leak) {
			t.Errorf("page leaks %q", leak)
		}
	}
	if en := errCall(t, h, errHost, "text/html", "en").Body.String(); !strings.Contains(en, "The lab is starting…") {
		t.Errorf("english page: %s", en)
	}
	if plain := errCall(t, h, errHost, "application/json", ""); plain.Code != 503 || strings.TrimSpace(plain.Body.String()) != "lab is starting" {
		t.Errorf("plain: %d %q", plain.Code, plain.Body.String())
	}
}

func TestStartingPage_OnlyAGetRefreshes(t *testing.T) {
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	fail(rec, req, 503, pageStarting, "x")
	if strings.Contains(rec.Body.String(), "http-equiv") {
		t.Fatal("a POST would be repeated as a GET")
	}
}

func TestStartingPage_AnAnsweringButBrokenLabStaysAnError(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}))
	defer broken.Close()
	h := errHandler(t, func(string, string) (string, error) { return broken.URL, nil })
	if rec := errCall(t, h, errHost, "text/html", ""); rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "Лабораторія запускається") {
		t.Fatalf("a lab that hangs up got %d", rec.Code)
	}
}
