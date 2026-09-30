package proxy

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func writePEM(t *testing.T, der []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "public.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadLabAccessPublicKey_Ed25519(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	got, err := ReadLabAccessPublicKey(writePEM(t, der))
	if err != nil || !got.Equal(pub) {
		t.Fatalf("key = %v err = %v", got, err)
	}
}

func TestReadLabAccessPublicKey_RefusesRSAAndGarbage(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	if _, err := ReadLabAccessPublicKey(writePEM(t, der)); err == nil {
		t.Fatal("an RSA key must be refused")
	}
	if _, err := ReadLabAccessPublicKey(writePEM(t, []byte("nope"))); err == nil {
		t.Fatal("garbage must be refused")
	}
	if _, err := ReadLabAccessPublicKey(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatal("a missing file must be refused")
	}
}

func TestLoadL7Config_ShortSessionSecretIsRefused(t *testing.T) {
	t.Setenv("TLS_CERT_PATH", "a")
	t.Setenv("TLS_KEY_PATH", "b")
	t.Setenv("LAB_ACCESS_PUBLIC_KEY_PATH", "c")
	t.Setenv("BASE_DOMAIN", "example.com")
	t.Setenv("SESSION_SECRET", "too-short")
	if _, err := LoadL7Config(); err == nil {
		t.Fatal("a short SESSION_SECRET must be refused")
	}
	t.Setenv("SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	if _, err := LoadL7Config(); err != nil {
		t.Fatal(err)
	}
}
