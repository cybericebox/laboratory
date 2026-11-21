package grant

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func pemOf(t *testing.T) (ed25519.PublicKey, []byte) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func TestDirKeysFindsAKeyAndStopsAtRemoval(t *testing.T) {
	dir := t.TempDir()
	pub, data := pemOf(t)
	if err := os.WriteFile(filepath.Join(dir, "acme.k1"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// the kubelet's bookkeeping entries of an atomically updated volume are no keys
	if err := os.Mkdir(filepath.Join(dir, "..2026_11_20"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	d := NewDirKeys(dir, 30*time.Second)
	d.now = func() time.Time { return now }

	if k, ok := d.Lookup("acme", "k1"); !ok || !k.Equal(pub) {
		t.Fatalf("key not found: %v", ok)
	}
	if _, ok := d.Lookup("acme", "k2"); ok {
		t.Fatal("another key id is not found")
	}
	if _, ok := d.Lookup("other", "k1"); ok {
		t.Fatal("another tenant is not found")
	}
	if err := os.Remove(filepath.Join(dir, "acme.k1")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	if _, ok := d.Lookup("acme", "k1"); !ok {
		t.Fatal("the directory is read again only after the interval")
	}
	now = now.Add(21 * time.Second)
	if _, ok := d.Lookup("acme", "k1"); ok {
		t.Fatal("a removed key must stop working after the interval")
	}
}

func TestDirKeysRefusesNamesThatEscapeTheDirectory(t *testing.T) {
	dir := t.TempDir()
	_, data := pemOf(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "evil"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	d := NewDirKeys(dir, time.Second)
	for _, c := range [][2]string{{"acme", "../evil"}, {"acme", "a/b"}, {"..", "x"}, {"acme.x", "k"}, {"", "k"}, {"acme", ""}} {
		if _, ok := d.Lookup(c[0], c[1]); ok {
			t.Errorf("%q %q must not be found", c[0], c[1])
		}
	}
}

func TestDirKeysKeepsTheKeysWhenTheDirectoryCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	_, data := pemOf(t)
	if err := os.WriteFile(filepath.Join(dir, "acme.k1"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	d := NewDirKeys(dir, time.Second)
	d.now = func() time.Time { return now }
	if _, ok := d.Lookup("acme", "k1"); !ok {
		t.Fatal("not found")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, ok := d.Lookup("acme", "k1"); !ok {
		t.Fatal("an unreadable directory keeps the keys already held")
	}
}
