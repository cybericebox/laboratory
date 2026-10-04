package tlsreload

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type pair struct{ certPEM, keyPEM []byte }

// issue makes a self-signed cert for dns; it doubles as its own CA.
func issue(t *testing.T, dns string) pair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: dns},
		DNSNames: []string{dns}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	return pair{pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})}
}

func write(t *testing.T, dir string, p pair, mod time.Time) {
	t.Helper()
	for name, b := range map[string][]byte{"tls.crt": p.certPEM, "tls.key": p.keyPEM, "ca.crt": p.certPEM} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
}

func cn(t *testing.T, c *tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

func paths(dir string) (string, string, string) {
	return filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), filepath.Join(dir, "ca.crt")
}

func TestReloadsRenewedFiles(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	write(t, dir, issue(t, "old.example.com"), base)
	c, k, ca := paths(dir)
	f, err := New(c, k, ca)
	if err != nil {
		t.Fatal(err)
	}
	f.MinInterval = 0
	if got, _ := f.GetCertificate(nil); cn(t, got) != "old.example.com" {
		t.Fatalf("cn = %s", cn(t, got))
	}
	write(t, dir, issue(t, "new.example.com"), base.Add(time.Minute))
	if got, _ := f.GetCertificate(nil); cn(t, got) != "new.example.com" {
		t.Fatalf("after renewal cn = %s", cn(t, got))
	}
	if got, _ := f.GetClientCertificate(nil); cn(t, got) != "new.example.com" {
		t.Fatalf("client cert cn = %s", cn(t, got))
	}
}

func TestBrokenRenewalKeepsLastGoodAndRecovers(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	write(t, dir, issue(t, "good.example.com"), base)
	c, k, _ := paths(dir)
	f, _ := New(c, k, "")
	f.MinInterval = 0

	a, b := issue(t, "a.example.com"), issue(t, "b.example.com")
	// cert of a with the key of b: a half-swapped pair
	write(t, dir, pair{a.certPEM, b.keyPEM}, base.Add(time.Minute))
	if got, _ := f.GetCertificate(nil); cn(t, got) != "good.example.com" {
		t.Fatalf("a mismatched pair must not replace the good cert, got %s", cn(t, got))
	}
	write(t, dir, b, base.Add(2*time.Minute))
	if got, _ := f.GetCertificate(nil); cn(t, got) != "b.example.com" {
		t.Fatalf("recovery failed, got %s", cn(t, got))
	}
}

func TestMinIntervalThrottlesStat(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	write(t, dir, issue(t, "old.example.com"), base)
	c, k, _ := paths(dir)
	f, _ := New(c, k, "")
	clock := base
	f.now = func() time.Time { return clock }
	f.MinInterval = 10 * time.Second
	_, _ = f.GetCertificate(nil)
	write(t, dir, issue(t, "new.example.com"), base.Add(time.Minute))
	if got, _ := f.GetCertificate(nil); cn(t, got) != "old.example.com" {
		t.Fatal("reloaded inside MinInterval")
	}
	clock = clock.Add(11 * time.Second)
	if got, _ := f.GetCertificate(nil); cn(t, got) != "new.example.com" {
		t.Fatal("not reloaded after MinInterval")
	}
}

func TestNewFailsOnUnusableFiles(t *testing.T) {
	if _, err := New("/nonexistent/c", "/nonexistent/k", ""); err == nil {
		t.Fatal("expected an error")
	}
}

// A live mTLS handshake: the listener keeps running while its files are renewed,
// and new connections see the new certificate and the new client CA.
func TestServerConfigHandshakeAcrossRenewal(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	server1 := issue(t, "ctl.example.com")
	write(t, dir, server1, base)
	c, k, ca := paths(dir)
	f, err := New(c, k, ca)
	if err != nil {
		t.Fatal(err)
	}
	f.MinInterval = 0

	ln, err := tls.Listen("tcp", "127.0.0.1:0", f.ServerConfig("h2"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.(*tls.Conn).Handshake()
				_ = conn.SetReadDeadline(time.Now().Add(time.Second))
				_, _ = conn.Read(make([]byte, 1))
			}()
		}
	}()

	// handshake returns the served CN and ALPN, or the error of the handshake or,
	// under TLS 1.3, of the first read (where a client-cert rejection surfaces).
	handshake := func(root, client pair) (string, error) {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(root.certPEM)
		cc, err := tls.X509KeyPair(client.certPEM, client.keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", ln.Addr().String(),
			&tls.Config{RootCAs: pool, ServerName: "ctl.example.com", Certificates: []tls.Certificate{cc}, NextProtos: []string{"h2"}})
		if err != nil {
			return "", err
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); err != nil {
			if ne, ok := err.(net.Error); (!ok || !ne.Timeout()) && err.Error() != "EOF" {
				return "", err
			}
		}
		st := conn.ConnectionState()
		return st.PeerCertificates[0].Subject.CommonName + "/" + st.NegotiatedProtocol, nil
	}

	if got, err := handshake(server1, server1); err != nil || got != "ctl.example.com/h2" {
		t.Fatalf("before renewal: %q %v", got, err)
	}
	server2 := issue(t, "ctl.example.com")
	write(t, dir, server2, base.Add(time.Minute))
	if _, err := handshake(server1, server1); err == nil {
		t.Fatal("the old certificate must not be served or accepted as a client after renewal")
	}
	if got, err := handshake(server2, server2); err != nil || got != "ctl.example.com/h2" {
		t.Fatalf("after renewal: %q %v", got, err)
	}
}
