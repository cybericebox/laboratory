package l7

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// DefaultLiveMaxLifetime caps how long one request or upgraded (WebSocket) connection may stay open.
const DefaultLiveMaxLifetime = 12 * time.Hour

// liveEntry is a request being served: what to cancel, or the hijacked connection to close, when the
// client loses access. Access is checked once per request, so without this an upgraded connection (a web
// terminal) or a long download would outlive a lock that denies every new request.
type liveEntry struct {
	group, client, lab, tenant string
	deadline                   time.Time
	cancel                     context.CancelFunc

	mu   sync.Mutex
	conn net.Conn
}

func (e *liveEntry) setConn(c net.Conn) {
	e.mu.Lock()
	e.conn = c
	e.mu.Unlock()
}

// close ends the request: its context ends (a plain request stops, upstream and client side) and a
// hijacked connection is closed (the proxy's copy loops then end and the backend side closes too).
func (e *liveEntry) close() {
	e.cancel()
	e.mu.Lock()
	c := e.conn
	e.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// liveSet is the registry of the requests in flight.
type liveSet struct {
	mu      sync.Mutex
	entries map[*liveEntry]struct{}
}

func newLiveSet() *liveSet { return &liveSet{entries: map[*liveEntry]struct{}{}} }

func (s *liveSet) remove(e *liveEntry) {
	s.mu.Lock()
	delete(s.entries, e)
	s.mu.Unlock()
}

func (s *liveSet) snapshot() []*liveEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*liveEntry, 0, len(s.entries))
	for e := range s.entries {
		out = append(out, e)
	}
	return out
}

// WithLiveMaxLifetime sets the longest a request or upgraded connection may last (zero: the default).
func (h *Handler) WithLiveMaxLifetime(d time.Duration) *Handler {
	h.liveMax = d
	return h
}

// liveDeadline is when a request that starts now must end: the session's absolute end, and the lifetime cap.
func (h *Handler) liveDeadline(abs int64) time.Time {
	max := h.liveMax
	if max <= 0 {
		max = DefaultLiveMaxLifetime
	}
	d := h.now().Add(max)
	if abs > 0 {
		if a := time.Unix(abs, 0); a.Before(d) {
			d = a
		}
	}
	return d
}

// CheckLive closes every request in flight that must not go on: its deadline passed, the group no longer
// belongs to the tenant that issued the session, or the group policy no longer lets the client reach the
// lab. It uses the same checks as a new request, so a lock takes effect on open sessions as soon as the
// proxy's own copy of the policy does. It returns how many it closed.
func (h *Handler) CheckLive() int {
	now := h.now()
	closed := 0
	for _, e := range h.live.snapshot() {
		stop := now.After(e.deadline)
		if !stop && h.groupTenant != nil {
			if owner, ok := h.groupTenant(e.group); !ok || owner != e.tenant {
				stop = true
			}
		}
		if !stop && h.authorize != nil && !h.authorize(e.group, e.client, e.lab) {
			stop = true
		}
		if stop {
			e.close()
			closed++
		}
	}
	return closed
}

// RunLiveCheck calls CheckLive every interval until ctx ends.
func (h *Handler) RunLiveCheck(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.CheckLive()
		}
	}
}

// hijackRecorder remembers the connection a hijack (a WebSocket upgrade) takes over, so it can be closed later.
type hijackRecorder struct {
	http.ResponseWriter
	entry *liveEntry
	// cookieName is the proxy's session cookie: a device may not set it, not even in an informational (1xx) response, which the
	// reverse proxy relays before the final one and ModifyResponse never sees.
	cookieName       string
	host, baseDomain string
}

// WriteHeader strips the session cookie from the headers of an informational response before they go out.
func (w *hijackRecorder) WriteHeader(code int) {
	if code >= 100 && code < 200 && w.cookieName != "" {
		deviceResponseFilter(w.Header(), w.cookieName, w.host, w.baseDomain)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		// The server's read and write deadlines (the HTTP timeouts) were set on the connection for the request; an upgraded
		// connection is bounded by the live-check lifetime instead.
		_ = conn.SetDeadline(time.Time{})
		w.entry.setConn(conn)
	}
	return conn, rw, err
}

// Unwrap lets http.ResponseController reach Flush.
func (w *hijackRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
