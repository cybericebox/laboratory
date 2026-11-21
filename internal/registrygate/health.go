package registrygate

import (
	"strconv"
	"sync"
	"time"
)

// informerHealth is what keeps the cluster view's freshness honest: the view is "in contact" only when every kind the gate needs is
// healthy, not when any one event or the API server's version call succeeded. A kind is unhealthy when its watch reported an error
// within errWindow, or when the last cheap list of it failed.
type informerHealth struct {
	// Window is how long a watch error keeps its kind unhealthy (default one minute).
	Window time.Duration
	Now    func() time.Time

	// StallAfter is how long an informer may stay behind the API server without advancing before it counts as stalled (default 3 minutes).
	StallAfter time.Duration

	mu      sync.Mutex
	errAt   map[string]time.Time
	listErr map[string]error
	prog    map[string]*progress
}

// progress is what a silent stall is told from: the resourceVersion the informer last synced to against the one a fresh list returns.
type progress struct {
	inf, list uint64
	moved     time.Time // the last time the informer advanced or was caught up
}

func (h *informerHealth) stallAfter() time.Duration {
	if h.StallAfter > 0 {
		return h.StallAfter
	}
	return 3 * time.Minute
}

// Observe compares the resourceVersion the informer of a kind has synced to with the one the beat's list returned. An informer that is
// behind and has not moved for StallAfter is stalled: its watch is dead without having reported an error (a hung connection), which no
// event and no heartbeat can show. Versions that cannot be read are ignored.
func (h *informerHealth) Observe(kind, informerRV, listRV string) {
	inf, err1 := strconv.ParseUint(informerRV, 10, 64)
	list, err2 := strconv.ParseUint(listRV, 10, 64)
	if err1 != nil || err2 != nil {
		return
	}
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.prog == nil {
		h.prog = map[string]*progress{}
	}
	p := h.prog[kind]
	if p == nil {
		p = &progress{moved: now}
		h.prog[kind] = p
	}
	if inf >= list || inf != p.inf {
		p.moved = now
	}
	p.inf, p.list = inf, list
}

func (h *informerHealth) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *informerHealth) window() time.Duration {
	if h.Window > 0 {
		return h.Window
	}
	return time.Minute
}

// WatchError is told by the watch error handler of a kind's informer.
func (h *informerHealth) WatchError(kind string) {
	h.mu.Lock()
	if h.errAt == nil {
		h.errAt = map[string]time.Time{}
	}
	h.errAt[kind] = h.now()
	h.mu.Unlock()
}

// ListResult records the result of the periodic cheap list of a kind.
func (h *informerHealth) ListResult(kind string, err error) {
	h.mu.Lock()
	if h.listErr == nil {
		h.listErr = map[string]error{}
	}
	h.listErr[kind] = err
	h.mu.Unlock()
}

// Healthy says whether every kind is healthy; unhealthy names the first that is not.
func (h *informerHealth) Healthy() (ok bool, unhealthy string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	for k, err := range h.listErr {
		if err != nil {
			return false, k
		}
	}
	for k, p := range h.prog {
		if p.list > p.inf && now.Sub(p.moved) > h.stallAfter() {
			return false, k
		}
	}
	for k, at := range h.errAt {
		if now.Sub(at) < h.window() {
			return false, k
		}
	}
	return true, ""
}
