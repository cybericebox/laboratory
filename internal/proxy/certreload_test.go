package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
)

func writeCert(t *testing.T, dir, cn string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	must := func(name string, b []byte) {
		tmp := filepath.Join(dir, name+".tmp")
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil { // atomic, like a Secret volume swap
			t.Fatal(err)
		}
	}
	must("tls.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	must("tls.key", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
}

func servedCN(t *testing.T, w *certwatcher.CertWatcher) string {
	t.Helper()
	c, err := w.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(c.Certificate[0])
	return leaf.Subject.CommonName
}

// The proxy serves its certificate through certwatcher (cmd/proxy-l7/main.go);
// this pins that a renewed certificate is served without a restart.
func TestProxyCertWatcherServesRenewedCertificate(t *testing.T) {
	dir := t.TempDir()
	writeCert(t, dir, "old.labs.example.com")
	w, err := certwatcher.New(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	if got := servedCN(t, w); got != "old.labs.example.com" {
		t.Fatalf("cn = %s", got)
	}
	writeCert(t, dir, "new.labs.example.com")
	deadline := time.Now().Add(10 * time.Second)
	for servedCN(t, w) != "new.labs.example.com" {
		if time.Now().After(deadline) {
			t.Fatal("renewed certificate was not picked up")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
