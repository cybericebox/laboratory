package devicestate

import (
	"context"
	"hash/fnv"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

// DefaultPollInterval is how often the layer is scanned as a safety net next to
// the filesystem events.
const DefaultPollInterval = 30 * time.Second

// The writable layer is a participant's file system. Every walk of it goes through an os.Root opened on the layer (a symlink swapped in
// during the walk cannot lead it out of the layer), reads a directory in batches, and stops at a cap on the entries it looks at and on its
// time: a layer with millions of entries or one that is made to stall a walk costs the node a bounded amount.
const (
	// DefaultScanEntries is how many entries one scan or one watch registration looks at.
	DefaultScanEntries = 200000
	// DefaultScanTime is how long one scan or one watch registration may take.
	DefaultScanTime = 10 * time.Second

	readDirBatch = 256
)

// overCapFingerprint is what Fingerprint returns for a layer it could not finish looking at. It is one fixed value: a layer over the cap is
// not scanned again and again into "changes" (a prefix of an unordered directory read would differ from run to run), so its changes come from
// the inotify events only.
const overCapFingerprint = "over-cap"

// WatchOptions configures WatchWith.
type WatchOptions struct {
	// Poll is the interval of the periodic scan (DefaultPollInterval when zero).
	Poll time.Duration
	// Budget is the node-wide budget of watched directories shared by every layer; nil gives this layer a budget of its own.
	Budget *WatchBudget
	// ScanEntries and ScanTime cap one scan or registration (DefaultScanEntries and DefaultScanTime when zero).
	ScanEntries int
	ScanTime    time.Duration
	// OnOverCap is called at most once, when the layer is past a cap and is watched by events alone.
	OnOverCap func()
}

func (o WatchOptions) limits() scanLimits {
	l := scanLimits{entries: o.ScanEntries, time: o.ScanTime}
	if l.entries <= 0 {
		l.entries = DefaultScanEntries
	}
	if l.time <= 0 {
		l.time = DefaultScanTime
	}
	return l
}

// Watch reports changes of a writable layer directory; maxDirs is the budget of watched directories for this layer alone. The engine uses
// WatchWith with the node-wide budget.
func Watch(ctx context.Context, root string, pol snapshot.Policy, poll time.Duration, maxDirs int) (<-chan struct{}, error) {
	return WatchWith(ctx, root, pol, WatchOptions{Poll: poll, Budget: NewWatchBudget(maxDirs)})
}

// WatchWith reports changes of a writable layer directory. It combines two signals:
// inotify events on the directory tree (low latency; directories are added as
// they appear, excluded paths are not watched) and a periodic scan that compares
// a fingerprint of the tree. The scan is the safety net for what events miss:
// the kernel's inotify watch limit, and overlayfs, which does not always raise
// events on the upper directory for changes made through the overlay mount.
// The returned channel is closed when ctx ends, and the layer's share of the
// watch budget is given back then.
func WatchWith(ctx context.Context, root string, pol snapshot.Policy, o WatchOptions) (<-chan struct{}, error) {
	poll := o.Poll
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	budget := o.Budget
	if budget == nil {
		budget = NewWatchBudget(0)
	}
	rt, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	lim := o.limits()
	out := make(chan struct{}, 1)
	signal := func() {
		select {
		case out <- struct{}{}:
		default:
		}
	}
	var overOnce sync.Once
	overCap := func() {
		if o.OnOverCap != nil {
			overOnce.Do(o.OnOverCap)
		}
	}
	// The baseline is taken now: what is in the layer already is not a change.
	last, over := fingerprintRoot(rt, pol, lim)
	if over {
		overCap()
	}

	share := &watchShare{b: budget}
	w, err := fsnotify.NewWatcher()
	if err == nil {
		if addTree(w, rt, root, ".", pol, share, lim) {
			overCap()
		}
	}

	go safeGo("layer watcher", func(error) {}, func() {
		defer close(out)
		defer func() { _ = rt.Close() }()
		defer share.release()
		if w != nil {
			defer func() { _ = w.Close() }()
		}
		var events <-chan fsnotify.Event
		var errs <-chan error
		if w != nil {
			events, errs = w.Events, w.Errors
		}
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					events = nil
					continue
				}
				rel, ok := relUnder(root, ev.Name)
				if !ok || pol.Excluded(snapshot.CleanPath(rel)) {
					continue
				}
				if ev.Has(fsnotify.Create) {
					if fi, err := rt.Lstat(rel); err == nil && fi.IsDir() {
						if addTree(w, rt, root, rel, pol, share, lim) {
							overCap()
						}
					}
				}
				signal()
			case _, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				// Queue overflow: changes may have been missed, so report one.
				signal()
			case <-t.C:
				fp, over := fingerprintRoot(rt, pol, lim)
				if over {
					overCap()
				}
				if fp != last {
					last = fp
					signal()
				}
			}
		}
	})
	return out, nil
}

// relUnder is the slash path of p below root, "." for root itself; false for a path that is not below it.
func relUnder(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// DefaultMaxWatchDirs is how many directories of all the writable layers of the node are watched with inotify together. A watch costs kernel
// memory and the limit (fs.inotify.max_user_watches) is shared by everything on the node, so devices that make a million directories must
// not be able to use it all: past the budget a layer is only polled.
const DefaultMaxWatchDirs = 2000

// WatchBudget is the node-wide budget of watched directories, shared by every layer that is followed; a layer gives its share back when its
// watch ends.
type WatchBudget struct {
	mu        sync.Mutex
	max, used int
}

// NewWatchBudget is a budget of max directories (DefaultMaxWatchDirs when max is not positive).
func NewWatchBudget(max int) *WatchBudget {
	if max <= 0 {
		max = DefaultMaxWatchDirs
	}
	return &WatchBudget{max: max}
}

// Used is how many directories are watched now.
func (b *WatchBudget) Used() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

func (b *WatchBudget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used >= b.max {
		return false
	}
	b.used++
	return true
}

func (b *WatchBudget) give(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	if b.used < 0 {
		b.used = 0
	}
}

// watchShare is what one layer has taken from the budget.
type watchShare struct {
	b     *WatchBudget
	mu    sync.Mutex
	taken int
}

func (s *watchShare) take() bool {
	if !s.b.take() {
		return false
	}
	s.mu.Lock()
	s.taken++
	s.mu.Unlock()
	return true
}

func (s *watchShare) release() {
	s.mu.Lock()
	n := s.taken
	s.taken = 0
	s.mu.Unlock()
	s.b.give(n)
}

// scanLimits cap one walk: the entries it looks at and the time it takes.
type scanLimits struct {
	entries int
	time    time.Duration
}

// scanTree visits the entries below the directory start (a slash path inside rt, "." for the root) until it has seen them all or hit a cap,
// and says whether it hit one. Directories are read in batches and walked depth first with no recursion; entries of excluded paths are not
// visited and excluded directories are not entered. visit says whether to enter a directory and whether to stop the whole walk. Symbolic
// links are entries like any other and are never followed, and nothing outside rt can be reached.
func scanTree(rt *os.Root, start string, pol snapshot.Policy, lim scanLimits, visit func(p string, d fs.DirEntry) (descend, stop bool)) (over bool) {
	deadline := time.Now().Add(lim.time)
	seen := 0
	stack := []string{start}
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		f, err := rt.Open(dir)
		if err != nil {
			continue // gone, or not a directory any more
		}
		for {
			batch, err := f.ReadDir(readDirBatch)
			for _, d := range batch {
				seen++
				if seen > lim.entries || (seen%64 == 0 && time.Now().After(deadline)) {
					_ = f.Close()
					return true
				}
				p := path.Join(dir, d.Name())
				if pol.Excluded(snapshot.CleanPath(p)) {
					continue
				}
				descend, stop := visit(p, d)
				if stop {
					_ = f.Close()
					return false
				}
				if descend && d.IsDir() {
					stack = append(stack, p)
				}
			}
			if err != nil || len(batch) == 0 {
				break
			}
		}
		_ = f.Close()
	}
	return false
}

// addTree watches the directory dir (a slash path inside rt, "." for the root) and every directory below it, as far as the node's budget goes
// and the caps allow. A failure to watch is covered by the periodic scan. It says whether it was stopped by a cap of the scan.
func addTree(w *fsnotify.Watcher, rt *os.Root, root, dir string, pol snapshot.Policy, share *watchShare, lim scanLimits) (over bool) {
	if w == nil {
		return false
	}
	add := func(rel string) bool {
		if !share.take() {
			return false // over the budget: the periodic scan covers the rest
		}
		_ = w.Add(filepath.Join(root, filepath.FromSlash(rel)))
		return true
	}
	if dir != "." && pol.Excluded(snapshot.CleanPath(dir)) {
		return false
	}
	if !add(dir) {
		return false
	}
	return scanTree(rt, dir, pol, lim, func(p string, d fs.DirEntry) (bool, bool) {
		if !d.IsDir() {
			return false, false
		}
		if !add(p) {
			return false, true
		}
		return true, false
	})
}

// Fingerprint summarises a layer directory: every non-excluded entry with its
// size, modification time and mode. Two scans differ when something changed.
// A layer past the scan caps has one fixed fingerprint (see overCapFingerprint).
func Fingerprint(root string, pol snapshot.Policy) string {
	rt, err := os.OpenRoot(root)
	if err != nil {
		return "0:0"
	}
	defer func() { _ = rt.Close() }()
	fp, _ := fingerprintRoot(rt, pol, WatchOptions{}.limits())
	return fp
}

// fingerprintRoot is Fingerprint on an open layer root; it also says whether a cap stopped the scan. The sum over the entries does not depend
// on the order in which the directories were read.
func fingerprintRoot(rt *os.Root, pol snapshot.Policy, lim scanLimits) (string, bool) {
	var sum uint64
	n := 0
	over := scanTree(rt, ".", pol, lim, func(p string, d fs.DirEntry) (bool, bool) {
		fi, err := d.Info()
		if err != nil {
			return d.IsDir(), false
		}
		n++
		sum += entryHash(snapshot.CleanPath(p), fi)
		return d.IsDir(), false
	})
	if over {
		return overCapFingerprint, true
	}
	return strconv.Itoa(n) + ":" + strconv.FormatUint(sum, 16), false
}

func entryHash(cp string, fi fs.FileInfo) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(cp))
	var b [24]byte
	put := func(i int, v uint64) {
		for k := 0; k < 8; k++ {
			b[i+k] = byte(v >> (8 * k))
		}
	}
	put(0, uint64(fi.Size()))
	put(8, uint64(fi.ModTime().UnixNano()))
	put(16, uint64(fi.Mode()))
	_, _ = h.Write(b[:])
	return h.Sum64()
}
