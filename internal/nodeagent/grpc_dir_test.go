//go:build linux

package nodeagent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSocketDirIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run", "cybericebox")
	if err := secureSocketDir(dir); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("new dir mode = %v", st.Mode().Perm())
	}
	// an existing, looser directory (a hostPath is made 0755) is tightened
	loose := t.TempDir()
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := secureSocketDir(loose); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(loose); st.Mode().Perm() != 0o700 {
		t.Fatalf("existing dir mode = %v", st.Mode().Perm())
	}
}
