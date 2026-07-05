package proxy

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	
	"github.com/fsnotify/fsnotify"
	ctrl "sigs.k8s.io/controller-runtime"
)

// KeyWatcher loads an RSA public key from a PEM file, caches it in memory,
// and reloads automatically when the file changes (kubelet secret-volume sync).
// Safe for concurrent use. Implements manager.Runnable.
type KeyWatcher struct {
	path   string
	cached atomic.Pointer[rsa.PublicKey]
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
func (kw *KeyWatcher) Key() *rsa.PublicKey {
	return kw.cached.Load()
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
					log.Error(err, "reload JWT public key", "path", kw.path)
				} else {
					log.Info("JWT public key reloaded", "path", kw.path)
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
	
	data, err := os.ReadFile(kw.path)
	if err != nil {
		return fmt.Errorf("read %s: %w", kw.path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return fmt.Errorf("no PEM block in %s", kw.path)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("not an RSA public key in %s", kw.path)
	}
	kw.cached.Store(rsaPub)
	return nil
}
