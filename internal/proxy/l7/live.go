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

	mu     sync.Mutex
	conn   net.Conn
	closed bool
}

func (e *liveEntry) setConn(c net.Conn) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		_ = c.Close()
		return
	}
	e.conn = c
	e.mu.Unlock()
}

// close ends the request: its context ends (a plain request stops, upstream and client side) and a
// hijacked connection is closed (the proxy's copy loops then end and the backend side closes too).
func (e *liveEntry) close() {
	e.cancel()
	e.mu.Lock()
	e.closed = true
	c := e.conn
	e.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// liveSet is the registry of the requests in flight.
type liveSet struct {
	mu        sync.Mutex
	entries   map[*liveEntry]struct{}
	perGroup  map[string]int
	perClient map[liveClientKey]int
	closed    bool
	changed   chan struct{}
}

type liveClientKey struct{ group, client string }

func newLiveSet() *liveSet {
	return &liveSet{entries: map[*liveEntry]struct{}{}, perGroup: map[string]int{}, perClient: map[liveClientKey]int{}, changed: make(chan struct{})}
}

func (s *liveSet) remove(e *liveEntry) {
	s.mu.Lock()
	if _, ok := s.entries[e]; ok {
		delete(s.entries, e)
		s.perGroup[e.group]--
		if s.perGroup[e.group] == 0 {
			delete(s.perGroup, e.group)
		}
		key := liveClientKey{e.group, e.client}
		s.perClient[key]--
		if s.perClient[key] == 0 {
			delete(s.perClient, key)
		}
		close(s.changed)
		s.changed = make(chan struct{})
	}
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
	type binding struct{ group, client, lab, tenant string }
	permissions := map[binding]bool{}
	owners := map[string]AccessGroup{}
	checked := map[string]bool{}
	for _, e := range h.live.snapshot() {
		stop := !now.Before(e.deadline)
		if !stop {
			key := binding{e.group, e.client, e.lab, e.tenant}
			allowed, seen := permissions[key]
			if !seen {
				allowed = true
				if h.access != nil {
					group, exists := owners[e.group]
					if !checked[e.group] {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						var err error
						group, err = h.access.Group(ctx, e.group)
						cancel()
						exists = err == nil
						checked[e.group] = true
						if exists {
							owners[e.group] = group
						}
					}
					allowed = exists && group.Tenant == e.tenant
					if allowed {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						allowed = h.access.Allowed(ctx, group.Namespace, e.client, e.lab)
						cancel()
					}
				} else {
					if h.groupTenant != nil {
						owner, ok := h.groupTenant(e.group)
						allowed = ok && owner == e.tenant
					}
					if allowed && h.authorize != nil {
						allowed = h.authorize(e.group, e.client, e.lab)
					}
				}
				permissions[key] = allowed
			}
			stop = !allowed
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
	meter            *RequestMeter
	compression      bool
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
		if w.meter != nil {
			if flushErr := rw.Writer.Flush(); flushErr != nil {
				w.meter.Incomplete()
				_ = conn.Close()
				return nil, nil, flushErr
			}
			wrapped := &meteredUpgrade{Conn: conn, reader: rw.Reader, in: newFrameMeter(w.meter.AddIn, w.meter.Incomplete, false, w.compression), out: newFrameMeter(w.meter.AddOut, w.meter.Incomplete, true, w.compression)}
			conn = wrapped
			rw = bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
		}
		w.entry.setConn(conn)
	}
	return conn, rw, err
}

// Unwrap lets http.ResponseController reach Flush.
func (w *hijackRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Shutdown stops admissions, cancels HTTP and upgraded connections, and waits
// for handler defers to settle their meters before the caller publishes.
func (h *Handler) Shutdown(ctx context.Context) error {
	h.live.mu.Lock()
	h.live.closed = true
	h.live.mu.Unlock()
	for _, entry := range h.live.snapshot() {
		entry.close()
	}
	for {
		h.live.mu.Lock()
		if len(h.live.entries) == 0 {
			h.live.mu.Unlock()
			return nil
		}
		changed := h.live.changed
		h.live.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
func (s *liveSet) stopping() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.closed }
