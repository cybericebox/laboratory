package l7

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/golang-jwt/jwt/v5"
)

type logLines struct {
	mu    sync.Mutex
	lines []map[string]any
}

func (l *logLines) sink() logr.Logger {
	return funcr.NewJSON(func(obj string) {
		var m map[string]any
		if json.Unmarshal([]byte(obj), &m) == nil {
			l.mu.Lock()
			l.lines = append(l.lines, m)
			l.mu.Unlock()
		}
	}, funcr.Options{})
}

func TestAccessLog_OneLinePerRequestWithoutAddresses(t *testing.T) {
	lab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte("hello"))
	}))
	defer lab.Close()
	var got logLines
	h := errHandler(t, func(string, string) (string, error) { return lab.URL, nil }).
		WithAccounting(NewMeter("b", time.Now()), func(string, string) (string, bool) { return "c-1", true }).
		WithAccessLog(got.sink(), 10*time.Millisecond)

	req := httptest.NewRequest("GET", "http://"+errHost+"/page/one?token=secret", nil)
	req.Host = errHost
	req.RemoteAddr = "203.0.113.9:4444"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	req.AddCookie(&http.Cookie{Name: "challenge", Value: errCookie(t)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "hello" {
		t.Fatalf("proxy: %d %q", rec.Code, rec.Body.String())
	}
	if len(got.lines) != 1 {
		t.Fatalf("%d lines", len(got.lines))
	}
	line := got.lines[0]
	if line["lab"] != "c-1" || line["method"] != "GET" || line["path"] != "/page/one" || line["status"] != float64(200) || line["bytes"] != float64(5) ||
		line["clientCancel"] != false || line["slow"] != true || line["upstreamMs"].(float64) < 30 {
		t.Fatalf("line: %v", line)
	}
	raw, _ := json.Marshal(line)
	for _, leak := range []string{"203.0.113.9", "198.51.100.7", "secret", "token"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("line leaks %q: %s", leak, raw)
		}
	}
}

func TestAccessLog_FastRequestIsNotSlow(t *testing.T) {
	lab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer lab.Close()
	var got logLines
	h := errHandler(t, func(string, string) (string, error) { return lab.URL, nil }).
		WithAccounting(NewMeter("b", time.Now()), func(string, string) (string, bool) { return "c-1", true }).
		WithAccessLog(got.sink(), 0)
	rec := errCall(t, h, errHost, "text/html", "")
	if rec.Code != 204 || len(got.lines) != 1 || got.lines[0]["slow"] != false || got.lines[0]["status"] != float64(204) {
		t.Fatalf("%d %v", rec.Code, got.lines)
	}
}

func errCookie(t *testing.T) string {
	t.Helper()
	return signCookie(t, jwtClaims{GroupID: "g1", Client: "p-u1",
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
}
