package grpc

import (
	"context"
	"sync"
)

// tenantRefreshGate coalesces successful refreshes after callers recheck their
// cache. Each refresh uses its own caller's context; a failed/canceled owner
// releases the gate so live waiters can try with their own context.
type tenantRefreshGate struct {
	mu      sync.Mutex
	entries map[string]*tenantRefreshEntry
}

type tenantRefreshEntry struct {
	available chan struct{}
	users     int // owners and waiters, guarded by gate.mu
}

func (g *tenantRefreshGate) acquire(ctx context.Context, tenant string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.entries == nil {
		g.entries = map[string]*tenantRefreshEntry{}
	}
	e := g.entries[tenant]
	if e == nil {
		e = &tenantRefreshEntry{available: make(chan struct{}, 1)}
		e.available <- struct{}{}
		g.entries[tenant] = e
	}
	e.users++
	g.mu.Unlock()
	forget := func() {
		g.mu.Lock()
		e.users--
		if e.users == 0 {
			delete(g.entries, tenant)
		}
		g.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	case <-e.available:
		if err := ctx.Err(); err != nil {
			e.available <- struct{}{}
			forget()
			return nil, err
		}
		return func() { e.available <- struct{}{}; forget() }, nil
	}
}
