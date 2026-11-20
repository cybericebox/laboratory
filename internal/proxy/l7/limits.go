package l7

import (
	"net"
	"net/http"
	"net/netip"
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
	allMul      float64 // the shared bucket is this many times a key's; 0: no shared bucket
	// overflow is the one bucket the keys that do not fit in a full table draw from (overflowMul times a key's), so a flood of new keys gets
	// a modest budget together, never a fresh burst each.
	overflow    bkt
	overflowMul float64
	lastEvict   time.Time
	now         func() time.Time
}

type bkt struct {
	tokens float64
	at     time.Time
}

func newBucketSet(rate float64, burst, maxKeys int) *bucketSet {
	return &bucketSet{rate: rate, burst: float64(burst), max: maxKeys, keys: map[string]*bkt{}, allMul: 20, overflowMul: 4, now: time.Now}
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
	if b.allMul > 0 && !b.all.take(now, b.rate*b.allMul, b.burst*b.allMul) {
		return false
	}
	k, ok := b.keys[key]
	if !ok {
		if len(b.keys) >= b.max {
			b.evict(now)
		}
		if len(b.keys) >= b.max {
			// Full of buckets that are not full: none is reset to make room. The newcomer shares the overflow bucket.
			return b.overflow.take(now, b.rate*b.overflowMul, b.burst*b.overflowMul)
		}
		k = &bkt{}
		b.keys[key] = k
	}
	return k.take(now, b.rate, b.burst)
}

// full reports whether the bucket has refilled completely: dropping it then changes nothing (a missing bucket starts full).
func (k *bkt) full(now time.Time, rate, burst float64) bool {
	return k.tokens+now.Sub(k.at).Seconds()*rate >= burst
}

// evict drops the buckets that are full again. A drained bucket is never dropped (that would hand its key a fresh burst). The scan is done
// at most once a second, so a flood of new keys does not make every request walk the table.
func (b *bucketSet) evict(now time.Time) {
	if now.Sub(b.lastEvict) < time.Second {
		return
	}
	b.lastEvict = now
	for key, k := range b.keys {
		if k.full(now, b.rate, b.burst) {
			delete(b.keys, key)
		}
	}
}

// WithAuthRateLimit limits the handoff path (/_auth). Before anything is verified, an attempt must pass two buckets of its client: the fine one
// of its address (IPv4 /32, IPv6 /64) and the coarse one of its network (IPv4 /24, IPv6 /48; ten times the rate and burst), whatever the attempt
// carries (no lab, no cookie in these keys). There is no bucket shared by all clients: what bounds the work garbage can cause is a cap on the
// verifications in flight (503 when it is full). After a link verifies, the lab, the address and the verified session (group and client of
// the link) share a bucket of rate per second and burst. Over a bucket the answer is 429. The tables are capped, never evict a drained bucket,
// and send the keys that do not fit to one shared overflow bucket.
func (h *Handler) WithAuthRateLimit(rate float64, burst int) *Handler {
	h.authLimit = newBucketSet(rate, burst, 100000)
	if rate > 0 {
		h.preAuthFine = newBucketSet(preAuthRate, preAuthBurst, 100000)
		h.preAuthCoarse = newBucketSet(preAuthRate*coarseMul, preAuthBurst*coarseMul, 100000)
		h.preAuthFine.allMul, h.preAuthCoarse.allMul = 0, 0
	}
	return h
}

// The pre-verification bucket of one client address: a normal user opens a handful of links; a few per second sustained is far above that.
const (
	preAuthRate  = 3.0
	preAuthBurst = 20
	coarseMul    = 10 // the network bucket is this many times the address one
)

// preAuthKeys are the two keys of the client of the connection (the TCP peer: the load balancer keeps the client address): the fine one (IPv4 address, IPv6 /64) and the coarse one (IPv4 /24, IPv6 /48), so one client cannot rotate through many keys.
func preAuthKeys(r *http.Request) (fine, coarse string) {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip, ip
	}
	a = a.Unmap()
	fineBits, coarseBits := 32, 24
	if a.Is6() {
		fineBits, coarseBits = 64, 48
	}
	f, _ := a.Prefix(fineBits)
	c, _ := a.Prefix(coarseBits)
	return f.String(), c.String()
}

// preAuthClientIP is the fine key (the address, or the /64 of an IPv6 one).
func preAuthClientIP(r *http.Request) string {
	f, _ := preAuthKeys(r)
	return f
}

// throttlePreAuth charges the caller one attempt at both tiers, before the link is looked at. It answers 429 itself when the client is over.
func (h *Handler) throttlePreAuth(w http.ResponseWriter, r *http.Request) bool {
	fine, coarse := preAuthKeys(r)
	if h.preAuthFine.allow(fine) && h.preAuthCoarse.allow(coarse) {
		return true
	}
	w.Header().Set("Retry-After", "5")
	fail(w, r, http.StatusTooManyRequests, pageBusy, "too many requests")
	return false
}

// acquireVerify reserves one of the verification slots; with none free it answers 503 at once, without verifying. The caller releases it.
func (h *Handler) acquireVerify(w http.ResponseWriter, r *http.Request) bool {
	select {
	case h.verifySem <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "1")
		fail(w, r, http.StatusServiceUnavailable, pageBusy, "service unavailable")
		return false
	}
}

func (h *Handler) releaseVerify() { <-h.verifySem }

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
