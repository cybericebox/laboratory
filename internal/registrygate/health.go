package registrygate

import (
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

	mu      sync.Mutex
	errAt   map[string]time.Time
	listErr map[string]error
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
	for k, at := range h.errAt {
		if now.Sub(at) < h.window() {
			return false, k
		}
	}
	return true, ""
}
