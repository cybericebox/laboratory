package imagecache

import (
	"context"
	"fmt"
	"sync"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// OperatorSource is the credential source of the operator's own pull secrets, the only one the operator resolves tags with today.
const OperatorSource = "operator"

// AsyncResolver resolves tags to digests in the background, so a reconcile never waits for a registry. A caller asks with Lookup:
// the answer is there, or it is Pending and the caller is told through Notify when it arrives (and looks again).
//
//   - One resolution per (credential source, platform scope, reference) runs at a time, however many labs ask for it.
//   - At most Concurrency resolutions run at once.
//   - The key carries the credential source, so the answer got with one set of credentials is never given to a caller that would
//     use another: each source has its own Resolver and its own entries.
//   - A success is kept for SuccessTTL (labs of one wave run one image even when the tag moves); a failure is kept until every
//     caller that waited for it has seen it (Done) and at most FailureTTL, then the next Lookup tries again.
type AsyncResolver struct {
	// Sources maps a credential source to the resolver that resolves with its credentials.
	Sources map[string]Resolver
	// Scope returns the platform scope of a resolution (see PlatformScope); nil: none. It is part of the key.
	Scope func(ctx context.Context) string
	// Concurrency bounds the resolutions in flight; 8 when not positive.
	Concurrency int
	// SuccessTTL is how long an answer is kept; 30 minutes when not positive.
	SuccessTTL time.Duration
	// FailureTTL is the longest a failure is kept for those who wait for it; 2 minutes when not positive.
	FailureTTL time.Duration
	// Notify is called, once per waiter, when a resolution the waiter asked for is done (outside every lock). Nil: nobody is told.
	Notify func(waiter string)
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu       sync.Mutex
	entries  map[string]*asyncEntry
	inflight int
	sem      chan struct{}
}

type asyncEntry struct {
	digest  string
	err     error
	done    bool
	at      time.Time
	waiters map[string]bool
}

// Lookup is the answer of AsyncResolver.Lookup.
type Lookup struct {
	// Pending: the resolution is running (or queued); Digest and Err are empty.
	Pending bool
	Digest  string
	Err     error
}

func (a *AsyncResolver) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *AsyncResolver) successTTL() time.Duration {
	if a.SuccessTTL > 0 {
		return a.SuccessTTL
	}
	return 30 * time.Minute
}

func (a *AsyncResolver) failureTTL() time.Duration {
	if a.FailureTTL > 0 {
		return a.FailureTTL
	}
	return 2 * time.Minute
}

// PlatformScope is the scope of a resolution for the platforms of the nodes: the one platform when there is exactly one (the image is
// pinned to the manifest of that platform), "" otherwise (the index digest is pinned). It matches what RegistryResolver does.
func PlatformScope(ps []v1.Platform) string {
	if len(ps) != 1 {
		return ""
	}
	return ps[0].OS + "/" + ps[0].Architecture
}

// Lookup asks for the digest of ref resolved with the credentials of source. waiter names who is asking (it is told through Notify when
// a pending answer arrives). A reference that already is a digest is answered at once.
func (a *AsyncResolver) Lookup(ctx context.Context, source, ref, waiter string) Lookup {
	if d := DigestOf(ref); d != "" {
		return Lookup{Digest: d}
	}
	inner, ok := a.Sources[source]
	if !ok {
		return Lookup{Err: fmt.Errorf("no credential source %q", source)}
	}
	scope := ""
	if a.Scope != nil {
		scope = a.Scope(ctx)
	}
	key := source + "\x00" + scope + "\x00" + ref

	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if e, ok := a.entries[key]; ok {
		switch {
		case !e.done:
			e.waiters[waiter] = true
			return Lookup{Pending: true}
		case e.err == nil && now.Sub(e.at) < a.successTTL():
			return Lookup{Digest: e.digest}
		case e.err != nil && now.Sub(e.at) < a.failureTTL():
			e.waiters[waiter] = true
			return Lookup{Err: e.err}
		}
		delete(a.entries, key) // too old: ask again
	}
	if a.entries == nil {
		a.entries = map[string]*asyncEntry{}
		n := a.Concurrency
		if n <= 0 {
			n = 8
		}
		a.sem = make(chan struct{}, n)
	}
	a.prune(now)
	e := &asyncEntry{waiters: map[string]bool{waiter: true}}
	a.entries[key] = e
	a.inflight++
	go a.run(e, inner, ref)
	return Lookup{Pending: true}
}

// Done says that waiter has used the answers for refs: a failure nobody waits for any more is forgotten, so the next lab that asks tries
// again at once.
func (a *AsyncResolver) Done(ctx context.Context, source, waiter string, refs ...string) {
	scope := ""
	if a.Scope != nil {
		scope = a.Scope(ctx)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ref := range refs {
		key := source + "\x00" + scope + "\x00" + ref
		e, ok := a.entries[key]
		if !ok || !e.done {
			continue
		}
		delete(e.waiters, waiter)
		if e.err != nil && len(e.waiters) == 0 {
			delete(a.entries, key)
		}
	}
}

// Pending is the number of resolutions that are running or queued.
func (a *AsyncResolver) Pending() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inflight
}

// prune drops the entries that are too old to be served. The caller holds mu.
func (a *AsyncResolver) prune(now time.Time) {
	for k, e := range a.entries {
		if !e.done {
			continue
		}
		if (e.err == nil && now.Sub(e.at) >= a.successTTL()) || (e.err != nil && now.Sub(e.at) >= a.failureTTL()) {
			delete(a.entries, k)
		}
	}
}

func (a *AsyncResolver) run(e *asyncEntry, inner Resolver, ref string) {
	a.sem <- struct{}{}
	// No caller's context: the answer is for everybody that asks, and the resolver has its own timeout.
	digest, err := inner.Resolve(context.Background(), ref)
	<-a.sem

	a.mu.Lock()
	e.digest, e.err, e.done, e.at = digest, err, true, a.now()
	a.inflight--
	waiters := make([]string, 0, len(e.waiters))
	for w := range e.waiters {
		waiters = append(waiters, w)
	}
	if err == nil {
		e.waiters = map[string]bool{} // a success is served to everybody until it is too old; nobody has to be remembered
	}
	a.mu.Unlock()
	if a.Notify != nil {
		for _, w := range waiters {
			a.Notify(w)
		}
	}
}
