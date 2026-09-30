package proxy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func pemOf(t *testing.T, pub ed25519.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// kubelet rotates a secret volume by swapping the ..data symlink, never by
// writing the file: the watcher must still see the new key.
func TestKeyWatcher_ReloadsAfterAKubeletStyleSwap(t *testing.T) {
	dir := t.TempDir()
	oldPub, _, _ := ed25519.GenerateKey(rand.Reader)
	newPub, _, _ := ed25519.GenerateKey(rand.Reader)

	v1 := filepath.Join(dir, "..v1")
	if err := os.Mkdir(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1, "public.pem"), pemOf(t, oldPub), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..v1", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "public.pem")
	if err := os.Symlink(filepath.Join("..data", "public.pem"), path); err != nil {
		t.Fatal(err)
	}

	kw, err := NewKeyWatcher(path)
	if err != nil {
		t.Fatal(err)
	}
	kw.poll = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = kw.Start(ctx) }()
	time.Sleep(200 * time.Millisecond)

	v2 := filepath.Join(dir, "..v2")
	if err := os.Mkdir(v2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v2, "public.pem"), pemOf(t, newPub), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink("..v2", tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if kw.Key().Equal(newPub) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the rotated key was not picked up")
}
