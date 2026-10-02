package demux

import (
	"sync"
	"time"
)

// Limits bound what an unauthenticated sender can make the demux hold or do. The demux is shared by every team and
// reads a public UDP port, so every table and every cost it spends on a packet from a stranger has a cap.
//
// There is no limit per source address: one NAT (a venue, a campus) hides a whole event behind one address, and behind a
// load balancer in SNAT mode every client has a node's address. What is capped is the whole table, the handshakes and
// the unmatched packets of all sources together, and the packets of one session.
type Limits struct {
	// MaxEntries is the most conntrack entries (two per session) in the whole table. A handshake that would pass it is
	// dropped (the client retries).
	MaxEntries int
	// PartialTTL is how long a handshake in progress (the init was forwarded, no answer yet) keeps its entry. It is short:
	// the entry is made before anything is authenticated, so a flood of inits must not be able to fill the table for the
	// full conntrack lifetime. A completed session keeps its entry for conntrackTTL after its last packet.
	PartialTTL time.Duration
	// GlobalHandshakeRate and GlobalHandshakeBurst limit handshake initiations of all sources together: each costs a scan of every
	// group's key, so the sources cannot add up to more than the demux can pay.
	GlobalHandshakeRate  float64
	GlobalHandshakeBurst int
	// MissRate and MissBurst limit, for all sources together, the packets that match no session or come from an unexpected
	// address (a guess of the 32-bit index, a spoof, a roam): guessing an index costs one of them, so guessing is rate-limited.
	MissRate  float64
	MissBurst int
	// RoamInterval is the least time between two address changes of one session.
	RoamInterval time.Duration
	// SessionRate and SessionBurst limit the transport packets of one session per second: the demux is shared by everyone,
	// and one participant with a valid session must not be able to use its CPU up. 0 = unlimited.
	SessionRate  float64
	SessionBurst int
	// Readers is how many goroutines read the socket (1 when zero).
	Readers int
}

// DefaultLimits mirror the chart (proxy.wg.limits).
func DefaultLimits() Limits {
	return Limits{
		MaxEntries:   100000,
		PartialTTL:   15 * time.Second,
		MissRate:     2000,
		MissBurst:    4000,
		RoamInterval: 5 * time.Second,
	}
}

// limiter is one token bucket.
type limiter struct {
	mu    sync.Mutex
	rate  float64
	burst float64
	b     bucket
	now   func() time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter(rate float64, burst int) *limiter {
	return &limiter{rate: rate, burst: float64(burst), now: time.Now}
}

// allow takes one token; false when the bucket is empty. rate 0 = unlimited.
func (l *limiter) allow() bool {
	if l.rate <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.take(l.now(), l.rate, int(l.burst))
}
