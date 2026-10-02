package l7

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func liveFixture(t *testing.T, backend http.Handler) (*Handler, *httptest.Server, *atomic.Bool, string) {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	b := httptest.NewServer(backend)
	t.Cleanup(b.Close)
	var allowed atomic.Bool
	allowed.Store(true)
	h := NewHandler(staticKeys("acme", "k1", pub), testSecret, "challenges.example.com", "challenge",
		func(task, groupID string) (string, error) { return b.URL, nil }).
		WithAuthorizer(func(group, client, lab string) bool { return allowed.Load() })
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cookie := signCookie(t, jwtClaims{GroupID: "g1", Client: "p-u1", Tenant: "acme",
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
	return h, srv, &allowed, cookie
}

// An upgraded connection (a web terminal) is open when the lock comes: it is closed, not left to end by itself.
func TestLiveUpgradedConnectionIsClosedWhenAccessIsRevoked(t *testing.T) {
	h, srv, allowed, cookie := liveFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(conn, conn) // echo
	}))
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET /term HTTP/1.1\r\nHost: web-abc123.challenges.example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nCookie: challenge=%s\r\n\r\n", cookie)
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "101") {
		t.Fatalf("upgrade: %q %v", line, err)
	}
	for {
		l, _ := br.ReadString('\n')
		if l == "\r\n" || l == "" {
			break
		}
	}
	fmt.Fprint(c, "ping\n")
	if got, _ := br.ReadString('\n'); got != "ping\n" {
		t.Fatalf("echo: %q", got)
	}

	if n := h.CheckLive(); n != 0 {
		t.Fatalf("access still holds: closed %d", n)
	}
	fmt.Fprint(c, "again\n")
	if got, _ := br.ReadString('\n'); got != "again\n" {
		t.Fatalf("still open: %q", got)
	}

	allowed.Store(false)
	if n := h.CheckLive(); n != 1 {
		t.Fatalf("closed %d, want 1", n)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadString('\n'); err == nil {
		t.Fatal("the connection must be closed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("the connection was not closed")
	}
}

// A long download or stream stops when access is revoked.
func TestLiveLongRequestIsCancelledWhenAccessIsRevoked(t *testing.T) {
	h, srv, allowed, cookie := liveFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for i := 0; i < 600; i++ {
			if _, err := fmt.Fprintf(w, "chunk %d\n", i); err != nil {
				return
			}
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	req, _ := http.NewRequest("GET", srv.URL+"/stream", nil)
	req.Host = "web-abc123.challenges.example.com"
	req.AddCookie(&http.Cookie{Name: "challenge", Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	allowed.Store(false)
	h.CheckLive()
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, br); done <- err }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream must stop after the revocation")
	}
}

func TestLiveDeadlineAndTenantChange(t *testing.T) {
	h, srv, _, cookie := liveFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	now := time.Now()
	h.now = func() time.Time { return now }
	h.WithLiveMaxLifetime(time.Minute)
	started := make(chan struct{})
	go func() {
		req, _ := http.NewRequest("GET", srv.URL+"/hang", nil)
		req.Host = "web-abc123.challenges.example.com"
		req.AddCookie(&http.Cookie{Name: "challenge", Value: cookie})
		close(started)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	for i := 0; i < 100 && len(h.live.snapshot()) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if len(h.live.snapshot()) != 1 {
		t.Fatal("the request is tracked")
	}
	if h.CheckLive() != 0 {
		t.Fatal("nothing to close yet")
	}
	now = now.Add(2 * time.Minute)
	if h.CheckLive() != 1 {
		t.Fatal("the lifetime cap closes it")
	}
	for i := 0; i < 100 && len(h.live.snapshot()) != 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if len(h.live.snapshot()) != 0 {
		t.Fatal("a finished request leaves the registry")
	}
}
