package health

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/cache"
)

type stubCache struct {
	cache.Cache
	synced bool
}

func (s stubCache) WaitForCacheSync(ctx context.Context) bool {
	if s.synced {
		return true
	}
	<-ctx.Done()
	return false
}

func TestCacheSyncedFailsUntilTheCachesAreSynced(t *testing.T) {
	req := httptest.NewRequest("GET", "/readyz", nil)
	start := time.Now()
	if err := CacheSynced(stubCache{synced: false})(req); err == nil {
		t.Fatal("not ready before the sync")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("the check must not hang the probe")
	}
	if err := CacheSynced(stubCache{synced: true})(req); err != nil {
		t.Fatalf("ready after the sync: %v", err)
	}
}

func TestFlagAndFunc(t *testing.T) {
	req := httptest.NewRequest("GET", "/readyz", nil)
	var f Flag
	if err := f.Check("the listener is not up")(req); err == nil || err.Error() != "the listener is not up" {
		t.Fatalf("flag unset: %v", err)
	}
	f.Set()
	if err := f.Check("x")(req); err != nil {
		t.Fatalf("flag set: %v", err)
	}
	if err := Func("down", func(context.Context) bool { return false })(req); err == nil {
		t.Fatal("a false condition fails")
	}
	if err := Func("down", func(context.Context) bool { return true })(req); err != nil {
		t.Fatal(err)
	}
}
