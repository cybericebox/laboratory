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
	allMul      float64 // the shared bucket is this many times a key's
	now         func() time.Time
}

type bkt struct {
	tokens float64
	at     time.Time
}

func newBucketSet(rate float64, burst, maxKeys int) *bucketSet {
	return &bucketSet{rate: rate, burst: float64(burst), max: maxKeys, keys: map[string]*bkt{}, allMul: 20, now: time.Now}
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

// allow takes one token from the key's bucket and one from the shared one (the key is the lab and session, see authKey). A rate of 0 allows everything.
func (b *bucketSet) allow(key string) bool {
	if b == nil || b.rate <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if !b.all.take(now, b.rate*b.allMul, b.burst*b.allMul) {
		return false
	}
	k, ok := b.keys[key]
	if !ok {
		if len(b.keys) >= b.max {
			b.evict(now)
		}
		k = &bkt{}
		b.keys[key] = k
	}
	return k.take(now, b.rate, b.burst)
}

// evict makes room: it drops the buckets that have been idle long enough to be full again (the same as no bucket), and when none is, one
// more, so the table never exceeds its cap whatever keys an attacker picks. A new source is never refused for the table being full.
func (b *bucketSet) evict(now time.Time) {
	refill := time.Duration(b.burst / b.rate * float64(time.Second))
	for key, k := range b.keys {
		if now.Sub(k.at) >= refill {
			delete(b.keys, key)
		}
	}
	for old := range b.keys {
		if len(b.keys) < b.max {
			break
		}
		delete(b.keys, old)
	}
}

// WithAuthRateLimit limits the handoff path (/_auth) twice. Before anything is verified, every attempt costs its client address (whatever it
// sends: no lab, no cookie in that key) a token of a bucket of preAuthRate per second and preAuthBurst, so garbage cannot make the proxy verify
// signatures without end and one address cannot hold back another. After a link verifies, the lab, the address and the verified session
// (group and client of the link) share a bucket of rate per second and burst. Over either the answer is 429. Both tables are capped.
func (h *Handler) WithAuthRateLimit(rate float64, burst int) *Handler {
	h.authLimit = newBucketSet(rate, burst, 100000)
	if rate > 0 {
		h.preAuthLimit = newBucketSet(preAuthRate, preAuthBurst, 100000)
		h.preAuthLimit.allMul = 200 // the shared bucket only bounds the whole proxy; a normal user never meets it
	}
	return h
}

// The pre-verification bucket of one client address: a normal user opens a handful of links; a few per second sustained is far above that.
const (
	preAuthRate  = 3.0
	preAuthBurst = 20
)

// preAuthClientIP is the client address the proxy resolved for the connection (the PROXY protocol header of the trusted edge, else the peer).
func preAuthClientIP(r *http.Request) string {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	return ip
}

// throttlePreAuth charges the caller's address one attempt, before the link is looked at. It answers 429 itself when the address is over.
func (h *Handler) throttlePreAuth(w http.ResponseWriter, r *http.Request) bool {
	if h.preAuthLimit.allow(preAuthClientIP(r)) {
		return true
	}
	w.Header().Set("Retry-After", "5")
	fail(w, r, http.StatusTooManyRequests, pageBusy, "too many requests")
	return false
}

// throttleHandoff counts a handoff link that has already verified against the budget of its lab, address and verified session, and answers
// 429 when it is over. session is built from the verified claims, never from a raw cookie.
func (h *Handler) throttleHandoff(w http.ResponseWriter, r *http.Request, task, session string) bool {
	if h.authLimit.allow(task + "|" + preAuthClientIP(r) + "|" + session) {
		return true
	}
	w.Header().Set("Retry-After", "5")
	fail(w, r, http.StatusTooManyRequests, pageBusy, "too many requests")
	return false
}
