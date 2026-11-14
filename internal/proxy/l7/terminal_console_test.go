package l7

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-logr/logr"
	"github.com/golang-jwt/jwt/v5"
)

// memSink collects every log line, key-value pairs included.
type memSink struct {
	mu    sync.Mutex
	lines []string
}

func (m *memSink) Init(logr.RuntimeInfo) {}
func (m *memSink) Enabled(int) bool      { return true }
func (m *memSink) Info(_ int, msg string, kv ...any) {
	m.mu.Lock()
	m.lines = append(m.lines, fmt.Sprint(msg, kv))
	m.mu.Unlock()
}
func (m *memSink) Error(err error, msg string, kv ...any) { m.Info(0, msg+" "+err.Error(), kv...) }
func (m *memSink) WithValues(...any) logr.LogSink         { return m }
func (m *memSink) WithName(string) logr.LogSink           { return m }
func (m *memSink) all() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.lines, "\n")
}

var sessionBlockRE = regexp.MustCompile(`<script type="application/json" id="session">(.*?)</script>`)

// consoleToken reads the console session token out of the page.
func consoleToken(t *testing.T, body string) string {
	t.Helper()
	m := sessionBlockRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no session block in the page: %.300s", body)
	}
	var s struct{ Token string }
	if err := json.Unmarshal([]byte(m[1]), &s); err != nil || s.Token == "" {
		t.Fatalf("session block %q: %v", m[1], err)
	}
	return s.Token
}

func (f *termFixture) dialWith(host, origin string, protocols ...string) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.srv.URL, "http")+"/ws", &websocket.DialOptions{Host: host, HTTPHeader: hdr, Subprotocols: protocols})
}

// The console of a page framed by another site (another registrable domain), with no cookie jar anywhere: the handoff link is answered with
// the page and a token, the page opens the socket with the token as a subprotocol, only the first subprotocol comes back, no Set-Cookie ever
// goes out, no log line holds the token or the link, and the CSP has no 'unsafe-inline' for scripts.
func TestConsoleWorksCookielessInACrossSiteFrame(t *testing.T) {
	sink := &memSink{}
	f := newTermFixture(t, func(c *TerminalConfig) {
		c.Embed = matcher(t, []string{"https://catalog.example.org"}, nil)
		c.Log = logr.New(sink)
	})
	f.h.WithAccessLog(logr.New(sink), 0)
	link := f.link(termHost)
	req, _ := http.NewRequest("GET", f.srv.URL+AuthPath+"?t="+url.QueryEscape(link), nil)
	req.Host = termHost
	req.Header.Set("Referer", "https://catalog.example.org/tasks/1") // what the browser sends for the frame
	resp, err := http.DefaultClient.Do(req)                          // a client with no cookie jar
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		body.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the page: %d %s", resp.StatusCode, body.String())
	}
	if len(resp.Header.Values("Set-Cookie")) != 0 || resp.Header.Get("Location") != "" {
		t.Fatalf("the console answer sets no cookie and does not redirect: %v", resp.Header)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors https://catalog.example.org") {
		t.Errorf("the allowed ancestor is not named: %s", csp)
	}
	for _, d := range strings.Split(csp, ";") {
		if strings.Contains(d, "script-src") && (strings.Contains(d, "unsafe") || strings.Contains(d, "nonce") || strings.Contains(d, "http")) {
			t.Errorf("script-src is not strict: %q", d)
		}
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers: %v", resp.Header)
	}
	if strings.Contains(body.String(), "onclick=") || strings.Contains(body.String(), "<script>") {
		t.Errorf("an inline script or handler in the page")
	}
	token := consoleToken(t, body.String())
	if strings.Contains(body.String(), link) {
		t.Error("the handoff link is echoed in the page")
	}

	// the socket: from the framing site's origin, token as the second subprotocol
	c, hresp, err := f.dialWith(termHost, "https://catalog.example.org", ConsoleSubprotocol, token)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	if got := c.Subprotocol(); got != ConsoleSubprotocol || strings.Contains(hresp.Header.Get("Sec-WebSocket-Protocol"), token) {
		t.Fatalf("only the first subprotocol comes back: %q / %q", got, hresp.Header.Get("Sec-WebSocket-Protocol"))
	}
	if err := c.Write(context.Background(), websocket.MessageText, []byte(`{"cols":100,"rows":30}`)); err != nil {
		t.Fatal(err)
	}
	if m := readText(t, c); m.Type != "ready" {
		t.Fatalf("not ready: %+v", m)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	// the console's own origin works too (a top-level page); another site's does not, token or not
	if c2, _, err := f.dialWith(termHost, "http://"+termHost, ConsoleSubprotocol, token); err != nil {
		t.Errorf("the host's own origin: %v", err)
	} else {
		_ = c2.CloseNow()
	}
	for _, origin := range []string{"https://evil.example.net", "https://catalog.example.org.evil.net", ""} {
		if c3, _, err := f.dialWith(termHost, origin, ConsoleSubprotocol, token); err == nil {
			_ = c3.CloseNow()
			t.Errorf("origin %q opened the console", origin)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if logs := sink.all(); strings.Contains(logs, token) || strings.Contains(logs, link) || strings.Contains(logs, "t="+url.QueryEscape(link)) {
		t.Errorf("a log line holds the token or the link:\n%s", logs)
	}
}

// What the token is worth: one host, one signature, one use, a limited time, and the exact subprotocols.
func TestConsoleTokenIsCheckedOnTheSocket(t *testing.T) {
	f := newTermFixture(t, nil)
	rec := f.get("/", nil)
	token := consoleToken(t, rec.Body.String())
	own := "http://" + termHost
	dial := func(host string, protocols ...string) error {
		c, _, err := f.dialWith(host, "http://"+host, protocols...)
		if err == nil {
			_ = c.CloseNow()
		}
		return err
	}
	if err := dial(termHost, ConsoleSubprotocol, token); err != nil {
		t.Fatalf("the token of this host: %v", err)
	}
	_ = own
	// a token for host A on host B (both consoles of the same group)
	if err := dial(termHostFor("lab1", "plain"), ConsoleSubprotocol, token); err == nil {
		t.Error("a token of one console host opened another")
	}
	// expired
	var claims jwtClaims
	_, _ = jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return testSecret, nil })
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	if err := dial(termHost, ConsoleSubprotocol, signCookie(t, claims)); err == nil {
		t.Error("an expired token opened the console")
	}
	// the cookie of a web session (use empty) is no console token, and a console token is no web cookie
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Hour))
	claims.Use = ""
	if err := dial(termHost, ConsoleSubprotocol, signCookie(t, claims)); err == nil {
		t.Error("a web session token opened the console")
	}
	req, _ := http.NewRequest("GET", f.srv.URL+"/", nil)
	req.Host = termHostFor("lab1", "kali")
	web := "web-k10w." + termBaseDom
	req2, _ := http.NewRequest("GET", f.srv.URL+"/", nil)
	req2.Host = web
	req2.AddCookie(&http.Cookie{Name: "challenge", Value: token})
	if resp, err := http.DefaultClient.Do(req2); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Error("a console token opened a web host as a cookie")
		}
	}
	// a token signed with another secret
	bad, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("another-secret-another-secret-123456"))
	claims.Use = "console"
	if err := dial(termHost, ConsoleSubprotocol, bad); err == nil {
		t.Error("a token signed with another key opened the console")
	}
	// the subprotocols: wrong name, missing token, token first, extra entries, none
	for name, protos := range map[string][]string{
		"wrong name":     {"cib.console.v2", token},
		"no token":       {ConsoleSubprotocol},
		"token first":    {token, ConsoleSubprotocol},
		"an extra entry": {ConsoleSubprotocol, token, "x"},
		"none":           nil,
	} {
		if err := dial(termHost, protos...); err == nil {
			t.Errorf("%s: the console opened", name)
		}
	}
	// no cookie is looked at on the console host, and no cookie path serves it
	for _, path := range []string{"/", "/ws"} {
		req, _ := http.NewRequest("GET", f.srv.URL+path, nil)
		req.Host = termHost
		req.AddCookie(&http.Cookie{Name: "challenge", Value: rehostWeb(t, f.cookie, termHost)})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with a web cookie on the console host: %d", path, resp.StatusCode)
		}
	}
}

// A reload of the console page (no ?t=) is "session over".
func TestConsolePageWithoutLinkIsSessionOver(t *testing.T) {
	f := newTermFixture(t, nil)
	f.cookie = "" // nothing makes a link
	rec := f.get("/", map[string]string{"Accept": "text/html"})
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Сесію завершено") || strings.Contains(rec.Body.String(), `id="session"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// The page script removes ?t= from the address and offers the token as the second subprotocol.
func TestConsoleScriptDropsTheLinkAndUsesSubprotocols(t *testing.T) {
	js, err := terminalFS.ReadFile("terminalassets/terminal.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, want := range []string{"history.replaceState", "'cib.console.v1'", "SESSION.token", "document.getElementById('session')"} {
		if !strings.Contains(src, want) {
			t.Errorf("terminal.js lacks %s", want)
		}
	}
	if strings.Contains(src, "document.cookie") || strings.Contains(src, "localStorage") || strings.Contains(src, "sessionStorage") {
		t.Error("the console script stores the token somewhere")
	}
	if i, j := strings.Index(src, "history.replaceState"), strings.Index(src, "new WebSocket("); i < 0 || j < 0 || i > j {
		t.Error("the link must be removed from the address before the socket opens")
	}
}

func rehostWeb(t *testing.T, token, host string) string {
	t.Helper()
	var claims jwtClaims
	if _, err := jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return testSecret, nil }); err != nil {
		t.Fatal(err)
	}
	claims.Host, claims.Use = host, ""
	return signCookie(t, claims)
}
