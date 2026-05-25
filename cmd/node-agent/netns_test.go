//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindPodNetNS_Found(t *testing.T) {
	// Create fake /proc structure in a temp dir
	tmp := t.TempDir()
	pid := "12345"
	procDir := filepath.Join(tmp, pid)
	if err := os.MkdirAll(procDir, 0755); err != nil {
		t.Fatal(err)
	}

	podUID := "abc-def-123"
	cgroup := "12:cpuset:/kubepods/pod" + podUID + "/container1\n"
	if err := os.WriteFile(filepath.Join(procDir, "cgroup"), []byte(cgroup), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := findPodNetNSIn(tmp, podUID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(tmp, pid, "ns", "net")
	if result != want {
		t.Errorf("got %q, want %q", result, want)
	}
}

func TestFindPodNetNS_NotFound(t *testing.T) {
	tmp := t.TempDir()
	_, err := findPodNetNSIn(tmp, "unknown-pod-uid")
	if err == nil {
		t.Fatal("expected error when pod not found")
	}
}
