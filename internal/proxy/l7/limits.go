package l7

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// LiveCaps bound the requests in flight: an upgraded (WebSocket) connection or a long download holds a goroutine and buffers for up to
// LiveMaxLifetime, and one authenticated session could otherwise hold thousands of them. Zero = unlimited.
type LiveCaps struct {
	PerClient, PerGroup, Total int
}

// tryAdd registers the request unless a cap would be passed; it reports whether it did.
func (s *liveSet) tryAdd(e *liveEntry, caps LiveCaps) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if caps.Total > 0 && len(s.entries) >= caps.Total {
		return false
	}
	key := liveClientKey{e.group, e.client}
	if caps.PerClient > 0 && s.perClient[key] >= caps.PerClient || caps.PerGroup > 0 && s.perGroup[e.group] >= caps.PerGroup {
		return false
	}
	if _, exists := s.entries[e]; exists {
		return true
	}
	s.entries[e] = struct{}{}
	s.perGroup[e.group]++
	s.perClient[key]++
	return true
}

// WithLiveCaps sets how many requests may be in flight in all, per group and per client of a group.
func (h *Handler) WithLiveCaps(c LiveCaps) *Handler {
	h.caps = c
	return h
}

// bucketSet is a token bucket per key plus one for everything, for the paths that cost the proxy something before a request is
// authenticated.
type bucketSet struct {
	mu          sync.Mutex
	rate, burst float64
	max         int
	keys        map[string]*bkt
	all         bkt
	now         func() time.Time
}

type bkt struct {
	tokens float64
	at     time.Time
}

func newBucketSet(rate float64, burst, maxKeys int) *bucketSet {
	return &bucketSet{rate: rate, burst: float64(burst), max: maxKeys, keys: map[string]*bkt{}, now: time.Now}
}

func (b *bkt) take(now time.Time, rate, burst float64) bool {
	if b.at.IsZero() {
		b.tokens, b.at = burst, now
	}
	b.tokens += now.Sub(b.at).Seconds() * rate
	if b.tokens > burst {
		b.tokens = burst
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// allow takes one token from the key's bucket and one from the shared one (the key is the peer address; behind a passthrough route it is
// the gateway's, so the shared bucket is what counts). A rate of 0 allows everything.
func (b *bucketSet) allow(key string) bool {
	if b == nil || b.rate <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if !b.all.take(now, b.rate*20, b.burst*20) {
		return false
	}
	k, ok := b.keys[key]
	if !ok {
		if len(b.keys) >= b.max {
			for old := range b.keys { // forget one: a new source is never refused for the table being full
				delete(b.keys, old)
				break
			}
		}
		k = &bkt{}
		b.keys[key] = k
	}
	return k.take(now, b.rate, b.burst)
}

// WithAuthRateLimit limits the handoff path (/_auth), which verifies a signature and may issue a session: per peer address (rate per second
// and burst), and all peers together at twenty times that. Over it the answer is 429.
func (h *Handler) WithAuthRateLimit(rate float64, burst int) *Handler {
	h.authLimit = newBucketSet(rate, burst, 100000)
	return h
}

func peerKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
