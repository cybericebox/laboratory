package imagecache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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

// Lookups are counted as hit, pending or miss, and each resolution is timed.
func TestAsyncMetrics(t *testing.T) {
	count := func(result string) uint64 { return sampleCount(t, pinLookups, "result", result) }
	hits, pending, misses := count("hit"), count("pending"), count("miss")
	resolved := sampleCount(t, pinResolveSeconds, "result", "ok")
	g := &gateResolver{release: make(chan struct{}), digest: dig}
	a := &AsyncResolver{Sources: map[string]Resolver{OperatorSource: g}}
	ctx := context.Background()
	a.Lookup(ctx, OperatorSource, "m:1", "a") // miss
	a.Lookup(ctx, OperatorSource, "m:1", "b") // pending
	close(g.release)
	waitFor(t, "answer", func() bool { return !a.Lookup(ctx, OperatorSource, "m:1", "c").Pending }) // hit (and maybe pending)
	if count("miss")-misses != 1 || count("pending")-pending < 1 || count("hit")-hits < 1 {
		t.Fatalf("miss %d pending %d hit %d", count("miss")-misses, count("pending")-pending, count("hit")-hits)
	}
	if got := sampleCount(t, pinResolveSeconds, "result", "ok") - resolved; got != 1 {
		t.Fatalf("%d resolutions timed", got)
	}
}

func sampleCount(t *testing.T, c prometheus.Collector, labels ...string) uint64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			ok := true
			for i := 0; i+1 < len(labels); i += 2 {
				found := false
				for _, l := range m.GetLabel() {
					found = found || (l.GetName() == labels[i] && l.GetValue() == labels[i+1])
				}
				ok = ok && found
			}
			if ok {
				if h := m.GetHistogram(); h != nil {
					n += h.GetSampleCount()
				} else if c := m.GetCounter(); c != nil {
					n += uint64(c.GetValue())
				}
			}
		}
	}
	return n
}

type staticDigest string

func (s staticDigest) Resolve(context.Context, string) (string, error) { return string(s), nil }

// The resolvers of labs' own credentials are separate sources: an answer got with one set is never given to another, one set made by two
// labs is made once, and a set nobody asks for any more is forgotten.
func TestCredentialSourcesAreSeparateAndForgotten(t *testing.T) {
	now := time.Now()
	a := &AsyncResolver{Sources: map[string]Resolver{OperatorSource: staticDigest("op")}, Now: func() time.Time { return now }, SuccessTTL: time.Minute}
	made := 0
	mk := func(d string) func() Resolver {
		return func() Resolver { made++; return staticDigest(d) }
	}
	a.EnsureSource(CredentialSource("one"), mk("d1"))
	a.EnsureSource(CredentialSource("one"), mk("other"))
	a.EnsureSource(CredentialSource("two"), mk("d2"))
	a.EnsureSource(OperatorSource, mk("hijack"))
	if made != 2 {
		t.Fatalf("a set is made once, the operator's source is never replaced: %d", made)
	}
	lookup := func(source string) string {
		var l Lookup
		waitFor(t, source, func() bool { l = a.Lookup(context.Background(), source, "nginx:1", "w"); return !l.Pending })
		if l.Err != nil {
			return "err"
		}
		return l.Digest
	}
	if got := []string{lookup(CredentialSource("one")), lookup(CredentialSource("two")), lookup(OperatorSource)}; got[0] != "d1" || got[1] != "d2" || got[2] != "op" {
		t.Fatalf("each source answers with its own credentials: %v", got)
	}
	now = now.Add(3 * time.Minute) // past twice the success time: unused sets go at the next one made
	a.EnsureSource(CredentialSource("three"), mk("d3"))
	if l := a.Lookup(context.Background(), CredentialSource("one"), "other:1", "w"); l.Err == nil {
		t.Fatal("an unused credential set is forgotten (the lab asks again with EnsureSource)")
	}
	if lookup(CredentialSource("three")) != "d3" {
		t.Fatal("the new one works")
	}
}
