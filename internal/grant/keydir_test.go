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

// One entry that cannot be read does not keep a removed key valid: the others are read and the removed one is gone.
func TestDirKeysSkipsAnUnreadableEntry(t *testing.T) {
	dir := t.TempDir()
	_, a := pemOf(t)
	pubB, b := pemOf(t)
	for name, data := range map[string][]byte{"acme.k1": a, "acme.k2": b} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	d := NewDirKeys(dir, 30*time.Second)
	d.now = func() time.Time { return now }
	if _, ok := d.Lookup("acme", "k1"); !ok {
		t.Fatal("k1")
	}
	// k1 is removed and k2 cannot be read at the next reload
	if err := os.Remove(filepath.Join(dir, "acme.k1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "acme.k2"), 0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	if _, ok := d.Lookup("acme", "k1"); ok {
		t.Fatal("a removed key stayed valid behind an unreadable entry")
	}
	if err := os.Chmod(filepath.Join(dir, "acme.k2"), 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	if k, ok := d.Lookup("acme", "k2"); !ok || !k.Equal(pubB) {
		t.Fatal("k2 is back when it can be read")
	}
}

// A directory that cannot be read keeps the keys for a bounded time and then holds none.
func TestDirKeysFailClosedAfterAContinuousFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, data := pemOf(t)
	if err := os.WriteFile(filepath.Join(dir, "acme.k1"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	d := NewDirKeys(dir, 15*time.Second)
	d.now = func() time.Time { return now }
	if _, ok := d.Lookup("acme", "k1"); !ok {
		t.Fatal("k1")
	}
	if err := os.Rename(dir, dir+".gone"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Second)
	if _, ok := d.Lookup("acme", "k1"); !ok {
		t.Fatal("a short failure keeps the keys held")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := d.Lookup("acme", "k1"); ok {
		t.Fatal("the keys stayed valid after a continuous failure")
	}
}
