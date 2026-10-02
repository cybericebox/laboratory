// Package health holds the readiness checks the platform's processes share: "ready" means the process really serves (its listener is
// up and its informer caches have synced), not merely that it started.
package health

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// CacheSynced is a check that passes once the manager's informer caches have listed everything they watch.
func CacheSynced(c cache.Cache) healthz.Checker {
	return func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), time.Second)
		defer cancel()
		if !c.WaitForCacheSync(ctx) {
			return errors.New("the informer caches have not synced yet")
		}
		return nil
	}
}

// Flag is a condition a process sets once it holds (a listener is bound), as a check.
type Flag struct{ v atomic.Bool }

// Set says the condition holds.
func (f *Flag) Set() { f.v.Store(true) }

// Check fails with the reason until the flag is set.
func (f *Flag) Check(reason string) healthz.Checker {
	return func(*http.Request) error {
		if !f.v.Load() {
			return errors.New(reason)
		}
		return nil
	}
}

// Func turns a condition into a check.
func Func(reason string, ok func(context.Context) bool) healthz.Checker {
	return func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
		defer cancel()
		if !ok(ctx) {
			return errors.New(reason)
		}
		return nil
	}
}
