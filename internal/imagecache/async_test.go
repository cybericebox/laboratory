package imagecache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type gateResolver struct {
	calls   atomic.Int32
	running atomic.Int32
	peak    atomic.Int32
	release chan struct{}
	digest  string
	err     error
}

func (g *gateResolver) Resolve(_ context.Context, _ string) (string, error) {
	g.calls.Add(1)
	n := g.running.Add(1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	<-g.release
	g.running.Add(-1)
	return g.digest, g.err
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

const dig = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestAsyncOneResolutionPerKeyAndNotifiesEveryWaiter(t *testing.T) {
	g := &gateResolver{release: make(chan struct{}), digest: dig}
	var mu sync.Mutex
	told := map[string]int{}
	a := &AsyncResolver{Sources: map[string]Resolver{OperatorSource: g}, Notify: func(w string) { mu.Lock(); told[w]++; mu.Unlock() }}
	ctx := context.Background()
	for _, w := range []string{"ns/a", "ns/b", "ns/c", "ns/a"} {
		if got := a.Lookup(ctx, OperatorSource, "nginx:1", w); !got.Pending {
			t.Fatalf("%s: %+v, want pending", w, got)
		}
	}
	close(g.release)
	waitFor(t, "the answer", func() bool { return !a.Lookup(ctx, OperatorSource, "nginx:1", "ns/a").Pending })
	if g.calls.Load() != 1 {
		t.Fatalf("%d resolutions for one key", g.calls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(told) != 3 || told["ns/a"] != 1 || told["ns/b"] != 1 || told["ns/c"] != 1 {
		t.Fatalf("waiters told: %v", told)
	}
	if got := a.Lookup(ctx, OperatorSource, "nginx:1", "ns/d"); got.Pending || got.Err != nil || got.Digest != dig {
		t.Fatalf("a later caller gets the kept answer: %+v", got)
	}
	if g.calls.Load() != 1 {
		t.Fatal("the kept answer was resolved again")
	}
}

func TestAsyncBoundsConcurrency(t *testing.T) {
	g := &gateResolver{release: make(chan struct{}), digest: dig}
	a := &AsyncResolver{Sources: map[string]Resolver{OperatorSource: g}, Concurrency: 3}
	ctx := context.Background()
	for i := range 10 {
		a.Lookup(ctx, OperatorSource, "img:"+string(rune('a'+i)), "w")
	}
	waitFor(t, "three running", func() bool { return g.running.Load() == 3 })
	time.Sleep(20 * time.Millisecond)
	if g.running.Load() != 3 || a.Pending() != 10 {
		t.Fatalf("running %d pending %d", g.running.Load(), a.Pending())
	}
	close(g.release)
	waitFor(t, "all done", func() bool { return a.Pending() == 0 })
	if g.peak.Load() > 3 {
		t.Fatalf("%d resolutions at once, limit 3", g.peak.Load())
	}
}

// What one credential source resolved is never given to a caller of another source.
func TestAsyncKeepsCredentialSourcesApart(t *testing.T) {
	open := &gateResolver{release: make(chan struct{}), digest: dig}
	private := &gateResolver{release: make(chan struct{}), err: errors.New("denied")}
	close(open.release)
	close(private.release)
	a := &AsyncResolver{Sources: map[string]Resolver{OperatorSource: open, "tenant-x": private}}
	ctx := context.Background()
	a.Lookup(ctx, OperatorSource, "registry.example/private:1", "w")
	waitFor(t, "operator answer", func() bool { return !a.Lookup(ctx, OperatorSource, "registry.example/private:1", "w").Pending })
	a.Lookup(ctx, "tenant-x", "registry.example/private:1", "w")
	var got Lookup
	waitFor(t, "tenant answer", func() bool { got = a.Lookup(ctx, "tenant-x", "registry.example/private:1", "w"); return !got.Pending })
	if got.Digest != "" || got.Err == nil {
		t.Fatalf("the tenant source was served the operator's answer: %+v", got)
	}
	if open.calls.Load() != 1 || private.calls.Load() != 1 {
		t.Fatalf("each source resolves for itself: %d %d", open.calls.Load(), private.calls.Load())
	}
	if got := a.Lookup(ctx, "nobody", "x:1", "w"); got.Err == nil || got.Pending {
		t.Fatalf("an unknown source must be an error: %+v", got)
	}
}

func TestAsyncPlatformScopeIsPartOfTheKey(t *testing.T) {
	g := &gateResolver{release: make(chan struct{}), digest: dig}
	close(g.release)
	scope := "linux/amd64"
	a := &AsyncResolver{Sources: map[string]Resolver{OperatorSource: g}, Scope: func(context.Context) string { return scope }}
	ctx := context.Background()
	a.Lookup(ctx, OperatorSource, "nginx:1", "w")
	waitFor(t, "first", func() bool { return !a.Lookup(ctx, OperatorSource, "nginx:1", "w").Pending })
	scope = "linux/arm64"
	a.Lookup(ctx, OperatorSource, "nginx:1", "w")
	waitFor(t, "second", func() bool { return !a.Lookup(ctx, OperatorSource, "nginx:1", "w").Pending })
	if g.calls.Load() != 2 {
		t.Fatalf("an answer for one platform served for another: %d resolutions", g.calls.Load())
	}
}

// A failure is shown to every lab that waited for it, then forgotten so that the next lab tries again; a lab that never comes back does
// not keep it for longer than FailureTTL.
func TestAsyncFailureIsKeptForWaitersThenForgotten(t *testing.T) {
	g := &gateResolver{release: make(chan struct{}), err: errors.New("timeout")}
	close(g.release)
	now := time.Now()
	a := &AsyncResolver{Sources: map[string]Resolver{OperatorSource: g}, FailureTTL: time.Minute, Now: func() time.Time { return now }}
	ctx := context.Background()
	a.Lookup(ctx, OperatorSource, "x:1", "ns/a")
	a.Lookup(ctx, OperatorSource, "x:1", "ns/b")
	waitFor(t, "failure", func() bool { return a.Pending() == 0 })
	for range 2 { // a lab that looks again before it is done still sees the failure
		if got := a.Lookup(ctx, OperatorSource, "x:1", "ns/a"); got.Err == nil {
			t.Fatalf("lost the failure: %+v", got)
		}
	}
	a.Done(ctx, OperatorSource, "ns/a", "x:1")
	if got := a.Lookup(ctx, OperatorSource, "x:1", "ns/b"); got.Err == nil {
		t.Fatal("ns/b still waits for the failure")
	}
	a.Done(ctx, OperatorSource, "ns/b", "x:1")
	// both done: the next lab resolves again
	if got := a.Lookup(ctx, OperatorSource, "x:1", "ns/c"); !got.Pending {
		t.Fatalf("a forgotten failure must be tried again: %+v", got)
	}
	waitFor(t, "second try", func() bool { return a.Pending() == 0 })
	if g.calls.Load() != 2 {
		t.Fatalf("%d resolutions", g.calls.Load())
	}
	// ns/c never comes back: the failure ages out
	now = now.Add(2 * time.Minute)
	if got := a.Lookup(ctx, OperatorSource, "x:1", "ns/d"); !got.Pending {
		t.Fatalf("an old failure must be tried again: %+v", got)
	}
}

func TestAsyncDigestReferenceNeedsNoRegistry(t *testing.T) {
	a := &AsyncResolver{Sources: map[string]Resolver{}}
	if got := a.Lookup(context.Background(), OperatorSource, "nginx@"+dig, "w"); got.Digest != dig || got.Pending {
		t.Fatalf("%+v", got)
	}
}
