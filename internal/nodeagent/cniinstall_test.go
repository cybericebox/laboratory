package nodeagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fastCNIRetry(t *testing.T) {
	t.Helper()
	old := cniRetryInterval
	cniRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { cniRetryInterval = old })
}

func TestInstallCNIConfWaitsForTheRealConfigAndWritesNoFallback(t *testing.T) {
	fastCNIRetry(t)
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- InstallCNIConf(dir, "/run/sock", 0) }()

	select {
	case err := <-done:
		t.Fatalf("returned with no base config: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(dir, CNIConfFile)); err == nil {
		t.Fatal("a conflist was written although no base CNI config exists")
	}

	base := `{"cniVersion":"1.0.0","name":"cilium","plugins":[{"type":"cilium-cni"}]}`
	if err := os.WriteFile(filepath.Join(dir, "05-cilium.conflist"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not pick the base config up")
	}
	data, err := os.ReadFile(filepath.Join(dir, CNIConfFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !contains(got, "cilium-cni") || contains(got, "host-local") {
		t.Errorf("the conflist must delegate to the real CNI:\n%s", got)
	}
}

func TestInstallCNIConfFallbackOnlyOnOptIn(t *testing.T) {
	fastCNIRetry(t)
	dir := t.TempDir()
	if err := InstallCNIConf(dir, "/run/sock", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, CNIConfFile))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), "host-local") {
		t.Errorf("the opt-in fallback must be written:\n%s", data)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
