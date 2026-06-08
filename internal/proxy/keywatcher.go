package proxy

import (
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
// Safe for concurrent use.
type KeyWatcher struct {
	path    string
	cached  atomic.Pointer[rsa.PublicKey]
	mu      sync.Mutex // guards reload serialisation only
}

// NewKeyWatcher creates a KeyWatcher, performs the initial load, and starts
// the background fsnotify goroutine. Stop watching by cancelling stop.
func NewKeyWatcher(path string, stop <-chan struct{}) (*KeyWatcher, error) {
	kw := &KeyWatcher{path: path}
	if err := kw.reload(); err != nil {
		return nil, err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}
	// Watch the directory — kubelet replaces the symlink, not the file itself.
	if err := watcher.Add(path); err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf("watch %s: %w", path, err)
	}

	go kw.run(watcher, stop)
	return kw, nil
}

// Key returns the cached public key. Never nil after successful construction.
func (kw *KeyWatcher) Key() *rsa.PublicKey {
	return (*rsa.PublicKey)(kw.cached.Load())
}

func (kw *KeyWatcher) run(w *fsnotify.Watcher, stop <-chan struct{}) {
	log := ctrl.Log.WithName("keywatcher")
	defer w.Close()
	for {
		select {
		case <-stop:
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create) || ev.Has(fsnotify.Rename) {
				if err := kw.reload(); err != nil {
					log.Error(err, "reload JWT public key", "path", kw.path)
				} else {
					log.Info("JWT public key reloaded", "path", kw.path)
				}
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
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
