package devicestate

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

// DefaultPollInterval is how often the layer is scanned as a safety net next to
// the filesystem events.
const DefaultPollInterval = 30 * time.Second

// Watch reports changes of a writable layer directory. It combines two signals:
// inotify events on the directory tree (low latency; directories are added as
// they appear, excluded paths are not watched) and a periodic scan that compares
// a fingerprint of the tree. The scan is the safety net for what events miss:
// the kernel's inotify watch limit, and overlayfs, which does not always raise
// events on the upper directory for changes made through the overlay mount.
// The returned channel is closed when ctx ends.
func Watch(ctx context.Context, root string, pol snapshot.Policy, poll time.Duration, maxDirs int) (<-chan struct{}, error) {
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	if maxDirs <= 0 {
		maxDirs = DefaultMaxWatchDirs
	}
	out := make(chan struct{}, 1)
	signal := func() {
		select {
		case out <- struct{}{}:
		default:
		}
	}
	// The baseline is taken now: what is in the layer already is not a change.
	last := Fingerprint(root, pol)

	w, err := fsnotify.NewWatcher()
	budget := &watchBudget{max: maxDirs}
	if err == nil {
		addTree(w, root, root, pol, budget)
	}

	go func() {
		defer close(out)
		if w != nil {
			defer w.Close()
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
				if pol.Excluded(containerPath(root, ev.Name)) {
					continue
				}
				if ev.Has(fsnotify.Create) {
					if fi, err := os.Lstat(ev.Name); err == nil && fi.IsDir() {
						addTree(w, root, ev.Name, pol, budget)
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
				if fp := Fingerprint(root, pol); fp != last {
					last = fp
					signal()
				}
			}
		}
	}()
	return out, nil
}

// containerPath converts a path under the layer root to the path inside the container.
func containerPath(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return snapshot.CleanPath(filepath.ToSlash(rel))
}

// DefaultMaxWatchDirs is how many directories of one layer are watched with inotify. A watch costs kernel memory and the
// limit (fs.inotify.max_user_watches) is shared by everything on the node, so a device that makes a million directories
// must not be able to use it all: past the cap the layer is only polled.
const DefaultMaxWatchDirs = 2000

// watchBudget counts the directories watched for one layer.
type watchBudget struct {
	max, used int
}

func (b *watchBudget) take() bool {
	if b.used >= b.max {
		return false
	}
	b.used++
	return true
}

func addTree(w *fsnotify.Watcher, root, dir string, pol snapshot.Policy, budget *watchBudget) {
	if w == nil {
		return
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if pol.Excluded(containerPath(root, p)) {
			return filepath.SkipDir
		}
		if !budget.take() {
			return filepath.SkipAll // over the cap: the periodic scan covers the rest
		}
		_ = w.Add(p) // a failure (watch limit) is covered by the periodic scan
		return nil
	})
}

// Fingerprint summarises a layer directory: every non-excluded entry with its
// size, modification time and mode. Two scans differ when something changed.
func Fingerprint(root string, pol snapshot.Policy) string {
	var sum uint64
	n := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == root {
			return nil
		}
		cp := containerPath(root, p)
		if pol.Excluded(cp) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		n++
		sum = sum*1099511628211 + hashString(cp) + uint64(fi.Size())*31 + uint64(fi.ModTime().UnixNano()) + uint64(fi.Mode())
		return nil
	})
	return strconv.Itoa(n) + ":" + strconv.FormatUint(sum, 16)
}

func hashString(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}
