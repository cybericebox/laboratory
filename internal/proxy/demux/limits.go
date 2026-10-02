package demux

import (
	"sync"
	"time"
)

// Limits bound what an unauthenticated sender can make the demux hold or do. The demux is shared by every team and
// reads a public UDP port, so every table and every cost it spends on a packet from a stranger has a cap.
type Limits struct {
	// MaxEntries is the most conntrack entries (two per session) in the whole table; MaxEntriesPerSource the most one
	// client address may own. A handshake that would pass either is dropped (the client retries).
	MaxEntries          int
	MaxEntriesPerSource int
	// HandshakeRate and HandshakeBurst limit handshake initiations per source address (per second, and the burst):
	// each one costs a scan of the groups' mac1 keys.
	HandshakeRate  float64
	HandshakeBurst int
	// MissRate and MissBurst limit, per source address, the packets that match no session (transport and cookie
	// packets with an unknown index): a guess of the 32-bit index costs one of them, so guessing is rate-limited.
	MissRate  float64
	MissBurst int
	// RoamInterval is the least time between two address changes of one session.
	RoamInterval time.Duration
	// MaxSources bounds the memory of the per-source limiters; past it, the source that has been quiet longest is forgotten to
	// make room (a new source is never refused for that).
	MaxSources int
	// GlobalHandshakeRate and GlobalHandshakeBurst limit handshake initiations of all sources together: each costs a scan of every
	// group's key, so spoofed sources (each with a budget of its own) cannot add up to more than the demux can pay.
	GlobalHandshakeRate  float64
	GlobalHandshakeBurst int
	// SessionRate and SessionBurst limit the transport packets of one session per second, and OwnerRate and OwnerBurst those of all
	// the sessions of one client address: the demux is shared by everyone, and one participant with a valid session must not be able to
	// use its CPU up. 0 = unlimited.
	SessionRate  float64
	SessionBurst int
	OwnerRate    float64
	OwnerBurst   int
	// Readers is how many goroutines read the socket (1 when zero).
	Readers int
}

// DefaultLimits mirror the chart (proxy.wg.limits).
func DefaultLimits() Limits {
	return Limits{
		MaxEntries: 100000, MaxEntriesPerSource: 64,
		HandshakeRate: 20, HandshakeBurst: 50,
		MissRate: 50, MissBurst: 100,
		RoamInterval: 5 * time.Second,
		MaxSources:   100000,
	}
}

// limiter is a token bucket per source address.
type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	max     int
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter(rate float64, burst, maxSources int) *limiter {
	return &limiter{rate: rate, burst: float64(burst), max: maxSources, buckets: map[string]*bucket{}, now: time.Now}
}

// allow takes one token of the source's bucket; false when it is empty (or the table of sources is full and this one
// is new).
func (l *limiter) allow(source string) bool {
	if l.rate <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[source]
	if !ok {
		if len(l.buckets) >= l.max {
			l.evictOldest()
		}
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[source] = b
	}
	b.tokens += now.Sub(b.at).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evictOldest forgets the source that has been quiet longest among a few that happen to come up first: a new source is never refused
// because the table is full, which would let an attacker lock every new participant out by filling it. Called with l.mu held.
func (l *limiter) evictOldest() {
	var oldest string
	var oldestAt time.Time
	n := 0
	for k, b := range l.buckets {
		if oldest == "" || b.at.Before(oldestAt) {
			oldest, oldestAt = k, b.at
		}
		if n++; n >= 16 {
			break
		}
	}
	if oldest != "" {
		delete(l.buckets, oldest)
	}
}

// sweep forgets the sources whose bucket is full again (idle), so the table does not grow with every address seen.
func (l *limiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.at).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
}
