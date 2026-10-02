package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T, cfg *L7Config) string {
	t.Helper()
	srv := NewHTTPServer(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("ok"))
	}), nil)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })
	return lis.Addr().String()
}

func shortLimits() *L7Config {
	return &L7Config{ReadHeaderTimeout: 300 * time.Millisecond, ReadTimeout: time.Second, IdleTimeout: 400 * time.Millisecond, MaxHeaderBytes: 1024}
}

// readsUntilClosed says whether the server closes the connection within the time.
func closedWithin(conn net.Conn, d time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(d))
	_, err := io.ReadAll(conn)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return false
	}
	return true
}

// The slowloris of the audit: headers that never finish do not hold the connection.
func TestSlowHeadersAreCut(t *testing.T) {
	conn, err := net.Dial("tcp", testServer(t, shortLimits()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: x\r\nX-A: ") // one incomplete header line, then silence
	if !closedWithin(conn, 3*time.Second) {
		t.Fatal("a connection that never finishes its headers must be closed")
	}
}

// A body that stalls is cut by the read timeout.
func TestSlowBodyIsCut(t *testing.T) {
	conn, err := net.Dial("tcp", testServer(t, shortLimits()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 100000\r\n\r\nabc")
	if !closedWithin(conn, 4*time.Second) {
		t.Fatal("a stalled body must be cut by the read timeout")
	}
}

// A kept-alive connection with no next request is closed after the idle timeout; a normal request still works.
func TestIdleConnectionIsClosedAndNormalRequestsWork(t *testing.T) {
	conn, err := net.Dial("tcp", testServer(t, shortLimits()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("a normal request: %v %v", resp, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if !closedWithin(conn, 3*time.Second) {
		t.Fatal("an idle kept-alive connection must be closed")
	}
}

// Headers over the limit are refused, not buffered.
func TestHugeHeadersAreRefused(t *testing.T) {
	conn, err := net.Dial("tcp", testServer(t, shortLimits()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\nX-Big: %s\r\n\r\n", strings.Repeat("a", 8192))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status = %v err = %v, want 431", resp, err)
	}
}

func TestServerLimitsComeFromTheConfig(t *testing.T) {
	t.Setenv("SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("TLS_CERT_PATH", "/x")
	t.Setenv("TLS_KEY_PATH", "/y")
	t.Setenv("BASE_DOMAIN", "labs.example.com")
	cfg, err := LoadL7Config()
	if err != nil {
		t.Fatal(err)
	}
	srv := NewHTTPServer(cfg, http.NotFoundHandler(), nil)
	if srv.ReadHeaderTimeout != 10*time.Second || srv.ReadTimeout != 5*time.Minute || srv.IdleTimeout != 2*time.Minute || srv.MaxHeaderBytes != 65536 || srv.WriteTimeout != 0 {
		t.Fatalf("limits: %v %v %v %d %v", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout, srv.MaxHeaderBytes, srv.WriteTimeout)
	}
	if cfg.AccessTokenMaxTTL != 60*time.Second {
		t.Fatalf("the access token limit is 60s: %v", cfg.AccessTokenMaxTTL)
	}
	t.Setenv("READ_HEADER_TIMEOUT", "0s")
	if _, err := LoadL7Config(); err == nil {
		t.Fatal("a zero timeout must be refused")
	}
}
