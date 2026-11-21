package devicestate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

func fillUpper(t *testing.T, dir, sub string, n int) {
	t.Helper()
	d := filepath.Join(dir, sub)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(d, fmt.Sprintf("f%04d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func sparseFile(t *testing.T, p string, size int64) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

func workDirEmpty(t *testing.T, r *rig) {
	t.Helper()
	ents, err := os.ReadDir(r.e.WorkDir)
	if err != nil || len(ents) != 0 {
		t.Fatalf("nothing may be written to the work directory before the pre-scan passes: %v %v", ents, err)
	}
}

// A layer of many empty files is refused by the pre-scan without containerd being asked for a diff; the refusal holds the device like the
// filter's does, so the same layer is not looked at again before the hold ends.
func TestPrescanRefusesTooManyEntriesWithoutADiffAndHolds(t *testing.T) {
	r, clk := rigWithClock(t)
	r.e.Now = clk.now
	r.pod.Policy = r.pod.Policy.WithMaxEntries(5)
	r.cl.pods = []PodInfo{r.pod}
	tr := r.track(t)
	fillUpper(t, r.rt.upper, "flood", 40)

	if err := tr.snapshot(context.Background(), true); err != nil {
		t.Fatalf("a refused live snapshot is not an error: %v", err)
	}
	if r.rt.diffs != 0 {
		t.Fatalf("containerd was asked for a diff of a layer over the entry cap: %d", r.rt.diffs)
	}
	workDirEmpty(t, r)
	if _, warns, _ := r.cl.snapshot(); len(warns) != 1 || !strings.Contains(warns[0], "more than 5 entries") {
		t.Fatalf("warnings %v", warns)
	}
	// the hold: the next live snapshot waits instead of scanning again
	var def *errDeferred
	if err := tr.snapshot(context.Background(), true); !errors.As(err, &def) || def.after <= 0 || def.after > quotaHold {
		t.Fatalf("ErrEntries must set holdUntil like ErrQuota: %v", err)
	}
	// after the hold, with the layer back under the cap, the snapshot goes through the diff
	if err := os.RemoveAll(filepath.Join(r.rt.upper, "flood")); err != nil {
		t.Fatal(err)
	}
	clk.advance(quotaHold + time.Second)
	r.rt.setDiff(tarOf(map[string]string{"a": "1"}))
	if err := tr.snapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if r.rt.diffs != 1 {
		t.Fatalf("diffs %d", r.rt.diffs)
	}
}

// A sparse file claims a size it does not occupy and a tar of it streams every zero: the pre-scan counts the apparent size and refuses it
// before anything is written, without a diff. (A file over the file size limit is skipped by the filter and never counted.)
func TestPrescanRefusesASparseFileOverTheQuotaBeforeAnyWrite(t *testing.T) {
	r, _ := rigWithClock(t)
	r.pod.Policy = r.pod.Policy.WithMaxFileSize(1 << 30) // a file this size is not skipped for its size
	r.cl.pods = []PodInfo{r.pod}
	tr := r.track(t)
	sparseFile(t, filepath.Join(r.rt.upper, "hole"), 1<<29) // half a gigabyte that takes no disk; the quota is 1 MiB
	if err := tr.snapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if r.rt.diffs != 0 {
		t.Fatal("a diff of a sparse terabyte was started")
	}
	workDirEmpty(t, r)
	if _, warns, _ := r.cl.snapshot(); len(warns) != 1 || !strings.Contains(warns[0], "quota") {
		t.Fatalf("warnings %v", warns)
	}
	// a required capture gets the error itself
	if err := tr.snapshotLocked(context.Background(), false, true); !errors.Is(err, snapshot.ErrQuota) {
		t.Fatalf("required capture: %v", err)
	}
	if r.rt.diffs != 0 {
		t.Fatal("a required capture started the diff")
	}
}

// What the filter leaves out does not count: excluded paths (and their size) and files over the file size limit.
func TestPrescanCountsOnlyWhatTheFilterKeeps(t *testing.T) {
	r, _ := rigWithClock(t)
	r.pod.Policy = r.pod.Policy.WithMaxEntries(5).WithMaxFileSize(1 << 10)
	r.cl.pods = []PodInfo{r.pod}
	tr := r.track(t)
	fillUpper(t, r.rt.upper, "tmp", 200)                           // excluded by default
	sparseFile(t, filepath.Join(r.rt.upper, "big"), 1<<40)         // over the file size limit: skipped, not counted
	sparseFile(t, filepath.Join(r.rt.upper, "tmp", "huge"), 1<<40) // excluded: not counted
	fillUpper(t, r.rt.upper, "data", 3)                            // 1 dir + 3 files: under the cap
	r.rt.setDiff(tarOf(map[string]string{"data/f0000": "x"}))
	if err := tr.snapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if r.rt.diffs != 1 {
		t.Fatalf("a layer that fits must be diffed: %d", r.rt.diffs)
	}
}

func TestPrescanLayerUnit(t *testing.T) {
	dir := t.TempDir()
	fillUpper(t, dir, "a", 10)
	pol := snapshot.NewPolicy(0, nil, 1<<20, 0)
	if err := prescanLayer(dir, pol.WithMaxEntries(100), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := prescanLayer(dir, pol.WithMaxEntries(5), time.Minute); !errors.Is(err, snapshot.ErrEntries) || !strings.Contains(err.Error(), "more than 5 entries") {
		t.Fatalf("%v", err)
	}
	// the time cap: a scan that is not allowed any time cannot say the layer is small, so the layer is refused
	if err := prescanLayer(dir, pol.WithMaxEntries(100000), time.Nanosecond); !errors.Is(err, snapshot.ErrEntries) {
		fillUpper(t, dir, "b", 500)
		if err := prescanLayer(dir, pol.WithMaxEntries(100000), time.Nanosecond); !errors.Is(err, snapshot.ErrEntries) {
			t.Fatalf("a scan out of time must refuse: %v", err)
		}
	}
	if err := prescanLayer(filepath.Join(dir, "missing"), pol, time.Minute); err != nil {
		t.Fatalf("no layer directory: the diff reports it, not the scan: %v", err)
	}
	// a symlink out of the layer is one entry, not a tree
	outside := t.TempDir()
	fillUpper(t, outside, "x", 300)
	link := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(link, "out")); err != nil {
		t.Fatal(err)
	}
	if err := prescanLayer(link, pol.WithMaxEntries(5), time.Minute); err != nil {
		t.Fatalf("the scan followed a link out of the layer: %v", err)
	}
}

// Two diffs run one after the other when the node's limit is one; the second waits for the first.
func TestDiffsAreLimitedNodeWide(t *testing.T) {
	r, _ := rigWithClock(t)
	r.e.MaxConcurrentDiffs = 1
	tr := r.track(t)
	held, err := r.e.diffGate().acquire(context.Background(), 1, true) // another device's diff
	if err != nil {
		t.Fatal(err)
	}
	r.rt.setDiff(tarOf(map[string]string{"a": "1"}))
	done := make(chan error, 1)
	go func() { done <- tr.snapshot(context.Background(), true) }()
	select {
	case err := <-done:
		t.Fatalf("the second diff ran beside the first: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	r.rt.mu.Lock()
	started := r.rt.diffs
	r.rt.mu.Unlock()
	if started != 0 {
		t.Fatalf("a diff started while the node's only slot was taken: %d", started)
	}
	held.release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting diff did not run when the slot was free")
	}
	if r.rt.diffs != 1 {
		t.Fatalf("diffs %d", r.rt.diffs)
	}
}

// The temporary space is a budget of the node: a live snapshot that finds it spoken for is deferred (not failed), an exit snapshot waits.
func TestTemporaryDiskBudgetDefersALiveSnapshotAndWaitsForAnExitOne(t *testing.T) {
	r, _ := rigWithClock(t)
	r.e.TempBudgetBytes = 1 << 20 // exactly one layer at the quota of this device
	tr := r.track(t)
	held, err := r.e.diffGate().acquire(context.Background(), 1<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	r.rt.setDiff(tarOf(map[string]string{"a": "1"}))
	var def *errDeferred
	if err := tr.snapshot(context.Background(), true); !errors.As(err, &def) || !strings.Contains(def.reason, "temporary space") {
		t.Fatalf("a live snapshot must be deferred: %v", err)
	}
	if r.rt.diffs != 0 {
		t.Fatal("a diff started over the budget")
	}
	if _, warns, _ := r.cl.snapshot(); len(warns) != 0 {
		t.Fatalf("a deferral is not a warning: %v", warns)
	}
	done := make(chan error, 1)
	go func() { done <- tr.snapshot(context.Background(), false) }() // the exit snapshot
	select {
	case err := <-done:
		t.Fatalf("an exit snapshot must wait for the space, not fail: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	held.release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r.e.diffGate().Used() != 0 {
		t.Fatalf("the reservation of a finished snapshot was not given back: %d", r.e.diffGate().Used())
	}
}

func TestDiffLimiterTicketsAreIdempotentAndBalanced(t *testing.T) {
	l := newDiffLimiter(2, 100)
	a, err := l.acquire(context.Background(), 60, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.acquire(context.Background(), 40, true)
	if err != nil {
		t.Fatal(err)
	}
	if l.Used() != 100 {
		t.Fatal(l.Used())
	}
	a.done()
	a.done()
	a.release()
	a.release()
	b.release()
	if l.Used() != 0 || len(l.slots) != 0 {
		t.Fatalf("used %d, slots in use %d", l.Used(), len(l.slots))
	}
	// a wait that ends with its context gives nothing away
	c, _ := l.acquire(context.Background(), 100, true)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(ctx, 50, true); err == nil {
		t.Fatal("it must wait for the budget")
	}
	if len(l.slots) != 1 {
		t.Fatalf("an abandoned wait kept a slot: %d", len(l.slots))
	}
	c.release()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk, err := l.acquire(context.Background(), 60, true)
			if err == nil {
				time.Sleep(5 * time.Millisecond)
				tk.release()
			}
		}()
	}
	wg.Wait()
	if l.Used() != 0 || len(l.slots) != 0 {
		t.Fatalf("after a crowd: used %d, slots %d", l.Used(), len(l.slots))
	}
}
