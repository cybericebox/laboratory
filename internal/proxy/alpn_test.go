package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"
)

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "proxy.test"}, DNSNames: []string{"proxy.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// The HTTPS server of the proxy negotiates h2 (the gRPC access of a device needs it) and still serves HTTP/1.1 clients.
func TestHTTPServerNegotiatesH2AndHTTP11(t *testing.T) {
	cfg := shortLimits()
	srv := NewHTTPServer(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}), &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(lis, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })

	for _, tc := range []struct {
		offer []string
		alpn  string
		proto string
	}{{[]string{"h2", "http/1.1"}, "h2", "HTTP/2.0"}, {[]string{"http/1.1"}, "http/1.1", "HTTP/1.1"}} {
		conn, err := tls.Dial("tcp", lis.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: tc.offer})
		if err != nil {
			t.Fatal(err)
		}
		if got := conn.ConnectionState().NegotiatedProtocol; got != tc.alpn {
			t.Errorf("offered %v: negotiated %q, want %q", tc.offer, got, tc.alpn)
		}
		_ = conn.Close()

		client := &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: tc.offer}, ForceAttemptHTTP2: tc.alpn == "h2",
		}}
		resp, err := client.Get("https://" + lis.Addr().String() + "/")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != tc.proto {
			t.Errorf("offered %v: served over %s, want %s", tc.offer, body, tc.proto)
		}
	}
}
