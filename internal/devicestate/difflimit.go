package devicestate

import (
	"context"
	"sync"
	"time"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

// DefaultMaxConcurrentDiffs is how many layers the node diffs at once. A diff reads a whole writable layer and writes the filtered tar to
// StateWorkDir; devices of many tenants share the node, so how many run together is a limit of the node and not of a device.
const DefaultMaxConcurrentDiffs = 2

// diffRetryAfter is how long a live snapshot that found the temporary space spoken for waits before it asks again.
const diffRetryAfter = 30 * time.Second

// diffLimiter bounds the work of diffs on the node: a counting semaphore of the diffs running together, and a byte budget of the temporary
// files (StateWorkDir) they may hold together. A diff reserves what its layer may take at most (the write quota) before it starts.
type diffLimiter struct {
	slots  chan struct{}
	mu     sync.Mutex
	cond   chan struct{} // closed and replaced when bytes are given back
	budget int64         // 0: no byte budget
	used   int64
}

func newDiffLimiter(slots int, budget int64) *diffLimiter {
	if slots <= 0 {
		slots = DefaultMaxConcurrentDiffs
	}
	return &diffLimiter{slots: make(chan struct{}, slots), cond: make(chan struct{}), budget: budget}
}

// diffTicket is a diff's hold on the limiter: the slot lasts while the layer is read and filtered (done), the reserved bytes until the temporary
// files are removed (release). Both are safe to call more than once.
type diffTicket struct {
	l     *diffLimiter
	bytes int64
	slot  sync.Once
	disk  sync.Once
}

func (t *diffTicket) done() {
	if t == nil {
		return
	}
	t.slot.Do(func() { <-t.l.slots })
}

func (t *diffTicket) release() {
	if t == nil {
		return
	}
	t.done()
	t.disk.Do(func() { t.l.give(t.bytes) })
}

// reserveFor is what a diff of a layer under pol may leave in the work directory.
func reserveFor(pol snapshot.Policy) int64 {
	if pol.WriteQuota > 0 {
		return pol.WriteQuota
	}
	return snapshot.DefaultWriteQuota
}

// acquire waits for a free slot (the ctx ends the wait) and reserves bytes of the temporary budget. A caller that must not wait for the budget
// (wait false: a live snapshot that can come back later) gets an errDeferred instead when it is spoken for; one that must have it (an exit or
// required snapshot) waits for it. A diff that alone is bigger than the whole budget is let through when nothing else holds any, so that
// it can run at all; the filter still bounds it by the quota.
func (l *diffLimiter) acquire(ctx context.Context, bytes int64, wait bool) (*diffTicket, error) {
	select {
	case l.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	for {
		l.mu.Lock()
		if l.budget <= 0 || l.used == 0 || l.used+bytes <= l.budget {
			l.used += bytes
			l.mu.Unlock()
			return &diffTicket{l: l, bytes: bytes}, nil
		}
		changed := l.cond
		l.mu.Unlock()
		if !wait {
			<-l.slots
			return nil, &errDeferred{after: diffRetryAfter, reason: "the temporary space for snapshots on this node is in use by other devices"}
		}
		select {
		case <-changed:
		case <-ctx.Done():
			<-l.slots
			return nil, ctx.Err()
		}
	}
}

func (l *diffLimiter) give(bytes int64) {
	l.mu.Lock()
	l.used -= bytes
	if l.used < 0 {
		l.used = 0
	}
	close(l.cond)
	l.cond = make(chan struct{})
	l.mu.Unlock()
}

// Used is the bytes reserved now.
func (l *diffLimiter) Used() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.used
}
