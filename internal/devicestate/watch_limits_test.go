package devicestate

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

func mkdirs(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func eventuallyUsed(b *WatchBudget, want int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if b.Used() == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return b.Used() == want
}

// One budget serves the whole node: a second device finds what the first left, falls back to polling (still noticed by the scan), and the
// first one's share comes back when its watch ends.
func TestTwoLayersShareOneWatchBudget(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	mkdirs(t, a, "d0", "d1", "d2")
	mkdirs(t, b, "e0", "e1", "e2")
	budget := NewWatchBudget(4)
	pol := snapshot.NewPolicy(0, nil, 0, 0)
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	if _, err := WatchWith(ctxA, a, pol, WatchOptions{Poll: 50 * time.Millisecond, Budget: budget}); err != nil {
		t.Fatal(err)
	}
	if budget.Used() != 4 { // the root and three directories: the whole budget
		t.Fatalf("the first layer took %d of 4", budget.Used())
	}
	chB, err := WatchWith(ctxB, b, pol, WatchOptions{Poll: 50 * time.Millisecond, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	if budget.Used() != 4 {
		t.Fatalf("the second layer went over the node's budget: %d", budget.Used())
	}
	if err := os.WriteFile(filepath.Join(b, "e2", "late"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !recv(chB, 3*time.Second) {
		t.Fatal("a layer without watches must still be noticed by the periodic scan")
	}
	cancelA()
	if !eventuallyUsed(budget, 0, 3*time.Second) {
		t.Fatalf("the share of an ended watch was not given back: %d", budget.Used())
	}
}

// Past the entry cap Fingerprint returns at once with one fixed value, whatever the tree holds, and OnOverCap is told once.
func TestFingerprintOverTheEntryCapIsBoundedAndFixed(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 50; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.Close() }()
	pol := snapshot.NewPolicy(0, nil, 0, 0)
	lim := scanLimits{entries: 10, time: time.Minute}
	fp1, over1 := fingerprintRoot(rt, pol, lim)
	if err := os.WriteFile(filepath.Join(root, "g-new"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	fp2, over2 := fingerprintRoot(rt, pol, lim)
	if !over1 || !over2 || fp1 != overCapFingerprint || fp2 != overCapFingerprint {
		t.Fatalf("over the cap the value is fixed: %q %v / %q %v", fp1, over1, fp2, over2)
	}
	// under the cap it still tells a change
	big := scanLimits{entries: 1000, time: time.Minute}
	a, overA := fingerprintRoot(rt, pol, big)
	if err := os.WriteFile(filepath.Join(root, "g-new"), []byte("longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, overB := fingerprintRoot(rt, pol, big)
	if overA || overB || a == b {
		t.Fatalf("under the cap a change shows: %q %q", a, b)
	}
}

// The time cap stops a scan that takes too long (a zero budget of time: it stops at the first check).
func TestScanStopsAtTheTimeCap(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 500; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%03d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.Close() }()
	visited := 0
	over := scanTree(rt, ".", snapshot.NewPolicy(0, nil, 0, 0), scanLimits{entries: 1 << 30, time: -time.Second}, func(string, fs.DirEntry) (bool, bool) {
		visited++
		return false, false
	})
	if !over || visited >= 500 {
		t.Fatalf("the scan went on past its time: over=%v visited=%d", over, visited)
	}
}

// A symbolic link in the layer is an entry and is never followed: what is behind it, outside the layer, is not walked, not fingerprinted and
// not watched, even when the link points at a directory of the node.
func TestScanNeverLeavesTheLayer(t *testing.T) {
	outside := t.TempDir()
	mkdirs(t, outside, "secret/deep")
	if err := os.WriteFile(filepath.Join(outside, "secret", "deep", "file"), []byte("host data"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	mkdirs(t, root, "real")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../..", filepath.Join(root, "real", "up")); err != nil {
		t.Fatal(err)
	}
	rt, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.Close() }()
	var seen []string
	over := scanTree(rt, ".", snapshot.NewPolicy(0, nil, 0, 0), scanLimits{entries: 1000, time: time.Minute}, func(p string, d fs.DirEntry) (bool, bool) {
		seen = append(seen, p)
		return true, false
	})
	if over {
		t.Fatal("unexpected cap")
	}
	for _, p := range seen {
		if p == "escape/secret" || p == "real/up/secret" {
			t.Fatalf("the walk followed a link out of the layer: %v", seen)
		}
	}
	if len(seen) != 3 { // real, escape, real/up
		t.Fatalf("entries seen: %v", seen)
	}
	// a directory swapped for a link to the outside between the scan and the next one is a link then, not a directory
	if err := os.RemoveAll(filepath.Join(root, "real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "real")); err != nil {
		t.Fatal(err)
	}
	seen = nil
	scanTree(rt, ".", snapshot.NewPolicy(0, nil, 0, 0), scanLimits{entries: 1000, time: time.Minute}, func(p string, d fs.DirEntry) (bool, bool) {
		seen = append(seen, p)
		return true, false
	})
	for _, p := range seen {
		if p != "escape" && p != "real" {
			t.Fatalf("the swapped directory led out of the layer: %v", seen)
		}
	}
	if fp := Fingerprint(root, snapshot.NewPolicy(0, nil, 0, 0)); fp == "" {
		t.Fatal("Fingerprint")
	}
}

// Watching a layer registers watches only inside it.
func TestWatchRegistersOnlyInsideTheLayer(t *testing.T) {
	outside := t.TempDir()
	mkdirs(t, outside, "d1", "d2")
	root := t.TempDir()
	mkdirs(t, root, "a")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	budget := NewWatchBudget(100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := WatchWith(ctx, root, snapshot.NewPolicy(0, nil, 0, 0), WatchOptions{Poll: time.Hour, Budget: budget}); err != nil {
		t.Fatal(err)
	}
	if budget.Used() != 2 { // the root and "a"; the link and what is behind it are not watched
		t.Fatalf("watched %d directories, want 2", budget.Used())
	}
}

func TestWatchOnAMissingLayerIsAnError(t *testing.T) {
	if _, err := WatchWith(context.Background(), filepath.Join(t.TempDir(), "none"), snapshot.NewPolicy(0, nil, 0, 0), WatchOptions{}); err == nil {
		t.Fatal("a layer directory that does not exist cannot be watched")
	}
}

// The over-cap callback fires once, however many scans follow.
func TestOverCapIsToldOnce(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	told := make(chan struct{}, 10)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := WatchWith(ctx, root, snapshot.NewPolicy(0, nil, 0, 0), WatchOptions{Poll: 20 * time.Millisecond, ScanEntries: 5, OnOverCap: func() { told <- struct{}{} }}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // many polls
	cancel()
	if len(told) != 1 {
		t.Fatalf("told %d times", len(told))
	}
}
