//go:build linux

package devicestate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestRequiredCaptureNativeOwnerLock(t *testing.T) {
	dir := t.TempDir()
	owner, err := AcquireCaptureOwner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := AcquireCaptureOwner(dir); err == nil {
		other.Close()
		t.Fatal("overlapping node-agent acquired capture ownership")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := AcquireCaptureOwner(dir)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

// This verifies real kernel freeze/sync, not a containerd capture/restore claim.
func TestRequiredCaptureNativeFreezer(t *testing.T) {
	if os.Getenv("CICE_CAPTURE_NATIVE") != "1" {
		t.Skip("requires owned privileged Linux container")
	}
	root := "/sys/fs/cgroup"
	dir := filepath.Join(root, "cri-containerd-cice-task2-"+strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dir)
	work := t.TempDir()
	path := filepath.Join(work, "ticks")
	child := exec.Command("sh", "-c", "while :; do echo x >> \"$1\"; sleep 0.01; done", "writer", path)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(filepath.Join(dir, "cgroup.freeze"), []byte("0"), 0644)
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(child.Process.Pid)), 0644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	thaw, err := Freeze(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncFilesystem(work); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	after, _ := os.ReadFile(path)
	if len(before) != len(after) || !frozen(dir) {
		t.Fatal("frozen writer continued to mutate filesystem")
	}
	ThawOrphansExcept(root, map[string]bool{dir: true})
	if !frozen(dir) {
		t.Fatal("protected required capture thawed during recovery")
	}
	thaw()
	time.Sleep(80 * time.Millisecond)
	after, _ = os.ReadFile(path)
	if len(after) <= len(before) {
		t.Fatal("writer did not resume after owned thaw")
	}
}
