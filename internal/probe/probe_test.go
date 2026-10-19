package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func loopback() []string { return []string{"127.0.0.1"} }

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return n
}

func TestReadyFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ready")
	c := Check{File: file}
	if err := c.Run(context.Background(), loopback); err == nil {
		t.Fatal("a missing marker file is not ready")
	}
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Run(context.Background(), loopback); err != nil {
		t.Fatalf("marker file exists: %v", err)
	}
}

func TestNothingToCheckIsAnError(t *testing.T) {
	if err := (Check{}).Run(context.Background(), loopback); err == nil {
		t.Fatal("a probe that checks nothing would report every device ready by mistake")
	}
}

func TestTCP(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portOf(t, l.Addr().String())
	c := Check{Port: port, Scheme: SchemeTCP}
	if err := c.Run(context.Background(), loopback); err != nil {
		t.Fatalf("listening port: %v", err)
	}
	_ = l.Close()
	if err := c.Run(context.Background(), loopback); err == nil {
		t.Fatal("a closed port is not ready")
	}
}

func TestHTTPAnyAnswerBelow500IsReady(t *testing.T) {
	for code, want := range map[int]bool{200: true, 204: true, 301: true, 401: true, 403: true, 404: true, 500: false, 502: false, 503: false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		c := Check{Port: portOf(t, srv.Listener.Addr().String()), Scheme: SchemeHTTP}
		err := c.Run(context.Background(), loopback)
		srv.Close()
		if (err == nil) != want {
			t.Errorf("status %d: ready=%v, want %v (%v)", code, err == nil, want, err)
		}
	}
}

func TestHTTPRedirectIsNotFollowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/never", http.StatusFound)
	}))
	defer srv.Close()
	c := Check{Port: portOf(t, srv.Listener.Addr().String()), Scheme: SchemeHTTP}
	if err := c.Run(context.Background(), loopback); err != nil {
		t.Fatalf("a redirect is an answer: %v", err)
	}
}

func TestHTTPSSelfSignedCertificateIsAccepted(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	c := Check{Port: portOf(t, srv.Listener.Addr().String()), Scheme: SchemeHTTPS}
	if err := c.Run(context.Background(), loopback); err != nil {
		t.Fatalf("https with a self-signed certificate: %v", err)
	}
}

func TestHTTPSAgainstAPlainServerIsNotReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	if err := (Check{Port: portOf(t, srv.Listener.Addr().String()), Scheme: SchemeHTTPS}).Run(context.Background(), loopback); err == nil {
		t.Fatal("a port that speaks plain http is not ready as https")
	}
}

func TestHTTPNotAnHTTPServer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("SSH-2.0-banner\r\n"))
			_ = conn.Close()
		}
	}()
	if err := (Check{Port: portOf(t, l.Addr().String()), Scheme: SchemeHTTP}).Run(context.Background(), loopback); err == nil {
		t.Fatal("a port that does not speak HTTP is not ready as http")
	}
	if err := (Check{Port: portOf(t, l.Addr().String()), Scheme: SchemeTCP}).Run(context.Background(), loopback); err != nil {
		t.Fatalf("but it is ready as tcp: %v", err)
	}
}

func TestAllChecksMustPass(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	file := filepath.Join(t.TempDir(), "ready")
	c := Check{File: file, Port: portOf(t, l.Addr().String()), Scheme: SchemeTCP}
	if err := c.Run(context.Background(), loopback); err == nil {
		t.Fatal("the port answers but the marker is missing")
	}
	_ = os.WriteFile(file, nil, 0o644)
	if err := c.Run(context.Background(), loopback); err != nil {
		t.Fatalf("both ready: %v", err)
	}
}

// An app bound to the pod's own address only (not to 127.0.0.1) still counts: the probe tries every local address.
func TestAnyLocalAddressCounts(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	c := Check{Port: portOf(t, l.Addr().String()), Scheme: SchemeTCP}
	addrs := func() []string { return []string{"192.0.2.1", "127.0.0.1"} } // the first is unroutable test net
	start := time.Now()
	if err := c.Run(context.Background(), addrs); err != nil {
		t.Fatalf("the second address answers: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("an unreachable address must not hold the probe back")
	}
}

func TestInstallCopiesAnExecutable(t *testing.T) {
	src := filepath.Join(t.TempDir(), "probe-src")
	if err := os.WriteFile(src, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "mnt")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := Install(src, dir); err != nil {
		t.Fatal(err)
	}
	got := filepath.Join(dir, InstalledName)
	b, err := os.ReadFile(got)
	if err != nil || string(b) != "binary" {
		t.Fatalf("copy = %q, %v", b, err)
	}
	st, _ := os.Stat(got)
	if st.Mode().Perm() != 0o555 {
		t.Fatalf("mode = %v, want r-xr-xr-x so any container user can run it and none can change it", st.Mode().Perm())
	}
	// running the install again over an existing copy works (init containers rerun)
	if err := Install(src, dir); err != nil {
		t.Fatalf("second install: %v", err)
	}
}
