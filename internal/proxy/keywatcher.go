package proxy

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	ctrl "sigs.k8s.io/controller-runtime"
)

// KeyWatcher loads an Ed25519 public key from a PEM file, caches it in memory,
// and reloads automatically when the file changes (kubelet secret-volume sync).
// Safe for concurrent use. Implements manager.Runnable.
type KeyWatcher struct {
	path   string
	cached atomic.Pointer[ed25519.PublicKey]
	mu     sync.Mutex
}

// NewKeyWatcher creates a KeyWatcher and performs the initial load.
// Call mgr.Add(kw) to start background watching.
func NewKeyWatcher(path string) (*KeyWatcher, error) {
	kw := &KeyWatcher{path: path}
	if err := kw.reload(); err != nil {
		return nil, err
	}
	return kw, nil
}

// Key returns the cached public key. Never nil after successful construction.
func (kw *KeyWatcher) Key() ed25519.PublicKey {
	return *kw.cached.Load()
}

// Start implements manager.Runnable. Blocks until ctx is cancelled.
func (kw *KeyWatcher) Start(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create fsnotify watcher: %w", err)
	}
	// Watch the directory — kubelet replaces the symlink, not the file itself.
	if err := watcher.Add(kw.path); err != nil {
		_ = watcher.Close()
		return fmt.Errorf("watch %s: %w", kw.path, err)
	}
	log := ctrl.Log.WithName("keywatcher")
	defer watcher.Close()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) || ev.Has(fsnotify.Rename) {
				if err := kw.reload(); err != nil {
					log.Error(err, "reload lab access public key", "path", kw.path)
				} else {
					log.Info("lab access public key reloaded", "path", kw.path)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Error(err, "fsnotify error", "path", kw.path)
		}
	}
}

func (kw *KeyWatcher) reload() error {
	kw.mu.Lock()
	defer kw.mu.Unlock()

	pub, err := ReadLabAccessPublicKey(kw.path)
	if err != nil {
		return err
	}
	kw.cached.Store(&pub)
	return nil
}
