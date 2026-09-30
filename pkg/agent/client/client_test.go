package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootCAsEmptyFileMeansSystemRoots(t *testing.T) {
	pool, err := rootCAs("")
	if err != nil || pool != nil {
		t.Fatalf("rootCAs(\"\") = %v, %v; want nil pool (system roots)", pool, err)
	}
}

func TestRootCAsKeepsPrivatePoolForAFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := rootCAs(filepath.Join(dir, "missing.pem")); err == nil {
		t.Fatal("a missing CA file must be an error, not a silent fallback to system roots")
	}
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rootCAs(junk); err == nil {
		t.Fatal("a CA file without certificates must be an error")
	}
}
