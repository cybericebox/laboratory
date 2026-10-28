package bandwidth

import (
	"sync"
	"time"
)

// Bucket is a token bucket counted in bytes whose rate can change at any time. The zero value is unlimited.
type Bucket struct {
	rate   float64 // bytes per second; 0 = unlimited
	burst  float64
	tokens float64
	at     time.Time
}

// kbpsToBytes turns kbit/s into bytes per second.
func kbpsToBytes(kbps int64) float64 { return float64(kbps) * 125 }

// SetRate changes the rate; the tokens kept stay (cut to the new burst). kbps 0 = unlimited.
func (b *Bucket) SetRate(kbps int64) {
	b.rate = kbpsToBytes(kbps)
	b.burst = burstFor(b.rate)
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
}

// burstFor is 400 ms of the rate, at least enough for a few full-size packets. A bucket that drops what is over (no queue) makes TCP
// back off: with 100 ms of burst a lone flow reached under half of its rate, with 400 ms nearly all of it (measured with a police on AWS).
func burstFor(rate float64) float64 {
	return max(rate*0.4, 16384)
}

func (b *Bucket) refill(now time.Time) {
	if b.at.IsZero() {
		b.tokens, b.at = b.burst, now
		return
	}
	if d := now.Sub(b.at); d > 0 {
		b.tokens = min(b.tokens+d.Seconds()*b.rate, b.burst)
		b.at = now
	}
}

// Allow takes n bytes if they are there and says whether it did (a packet that does not fit is dropped).
func (b *Bucket) Allow(now time.Time, n int) bool {
	if b.rate <= 0 {
		return true
	}
	b.refill(now)
	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}

// Reserve takes n bytes and says how long the caller has to wait before it may send them (a stream is paced, not dropped).
// The bucket may go into debt, so one write larger than the burst is still paced to the rate.
func (b *Bucket) Reserve(now time.Time, n int) time.Duration {
	if b.rate <= 0 {
		return 0
	}
	b.refill(now)
	b.tokens -= float64(n)
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

// Limited says the bucket has a rate.
func (b *Bucket) Limited() bool { return b.rate > 0 }

// Prime sets the tokens a bucket starts with (a newcomer gets a small burst, not a full one, so a flood of fresh keys cannot each
// spend a full burst).
func (b *Bucket) Prime(now time.Time, tokens float64) {
	b.at = now
	b.tokens = min(tokens, b.burst)
}

// DefaultMaxKeys bounds the participants a Throttle tracks. The keys come from sessions and clients the caller has accepted, but a
// bound is still what keeps the memory and the rescans of a throttle from growing with whatever reaches it; the participants past
// it share one bucket.
const DefaultMaxKeys = 65536

// newcomerBurst is the tokens a new participant starts with: a few full-size packets.
const newcomerBurst = 16384

// Throttle keeps participants to their fair share of a class: every participant has a bucket of its own, all of them also take
// from one bucket of the class, and the rate of each is the class divided by the number of participants that were active lately,
// never more than the cap. Participants are named by a key (a session, a client); one that sends nothing for the window stops
// counting and is forgotten later.
type Throttle[K comparable] struct {
	mu       sync.Mutex
	now      func() time.Time
	window   time.Duration
	classKbp int64
	capKbps  int64
	class    Bucket
	parts    map[K]*part
	maxKeys  int
	overflow *part // shared by the participants past maxKeys
	active   int
	nextScan time.Time
}

type part struct {
	b    Bucket
	last time.Time
}

// NewThrottle makes a throttle with no limits; SetLimits turns it on. window is how long a silent participant still counts as
// active (5 s when zero).
func NewThrottle[K comparable](window time.Duration) *Throttle[K] {
	if window <= 0 {
		window = 5 * time.Second
	}
	return &Throttle[K]{now: time.Now, window: window, parts: map[K]*part{}, maxKeys: DefaultMaxKeys}
}

// SetMaxKeys changes the bound on the participants tracked (the rest share one bucket).
func (t *Throttle[K]) SetMaxKeys(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.maxKeys = max(n, 1)
}

// SetLimits sets the class and the cap of one participant in kbit/s; 0 is no limit of that kind. Both 0 turns the throttle off.
func (t *Throttle[K]) SetLimits(classKbps, capKbps int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.classKbp, t.capKbps = classKbps, capKbps
	t.class.SetRate(classKbps)
	t.rescanLocked(t.now())
}

// Enabled says some limit is set.
func (t *Throttle[K]) Enabled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.classKbp > 0 || t.capKbps > 0
}

// Active is how many participants counted as active at the last scan.
func (t *Throttle[K]) Active() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active
}

// Rate is the rate of one participant now, in kbit/s (0 = unlimited).
func (t *Throttle[K]) Rate() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rateLocked()
}

func (t *Throttle[K]) rateLocked() int64 {
	if t.classKbp <= 0 && t.capKbps <= 0 {
		return 0
	}
	return FairShare(t.classKbp, t.capKbps, t.active)
}

// rescanLocked counts the active participants, forgets the long silent ones and gives everyone the new rate. It runs at most once
// a second (and when the limits change): a newcomer is counted at once, the others meet the new share at the next scan.
func (t *Throttle[K]) rescanLocked(now time.Time) {
	t.nextScan = now.Add(time.Second)
	t.active = 0
	for key, p := range t.parts {
		switch silent := now.Sub(p.last); {
		case silent <= t.window:
			t.active++
		case silent > 4*t.window:
			delete(t.parts, key)
		}
	}
	if t.overflow != nil {
		if silent := now.Sub(t.overflow.last); silent <= t.window {
			t.active++
		} else if silent > 4*t.window {
			t.overflow = nil
		}
	}
	rate := t.rateLocked()
	for _, p := range t.parts {
		p.b.SetRate(rate)
	}
	if t.overflow != nil {
		t.overflow.b.SetRate(rate)
	}
}

func (t *Throttle[K]) partLocked(key K, now time.Time) *part {
	p, known := t.parts[key]
	if known {
		p.last = now // before the scan, so the caller counts as active in it
	}
	if now.After(t.nextScan) {
		t.rescanLocked(now)
	}
	if known {
		return p
	}
	if len(t.parts) >= t.maxKeys {
		if t.overflow == nil {
			t.overflow = t.newPartLocked(now)
		}
		t.overflow.last = now
		return t.overflow
	}
	p = t.newPartLocked(now)
	t.parts[key] = p
	return p
}

// newPartLocked makes a participant: counted at once, at the current share, with a small first burst.
func (t *Throttle[K]) newPartLocked(now time.Time) *part {
	p := &part{last: now}
	t.active++
	p.b.SetRate(t.rateLocked())
	p.b.Prime(now, newcomerBurst)
	return p
}

// Allow takes n bytes of a participant: false when its bucket or the class is empty (the packet is dropped). Nothing is taken from
// the class when the participant's own bucket refuses.
func (t *Throttle[K]) Allow(key K, n int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.classKbp <= 0 && t.capKbps <= 0 {
		return true
	}
	now := t.now()
	p := t.partLocked(key, now)
	if !p.b.Allow(now, n) {
		return false
	}
	return t.class.Allow(now, n)
}

// Delay takes n bytes of a participant and returns how long the sender has to wait (the stream is paced). The longer of the
// participant's and the class's wait counts.
func (t *Throttle[K]) Delay(key K, n int) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.classKbp <= 0 && t.capKbps <= 0 {
		return 0
	}
	now := t.now()
	p := t.partLocked(key, now)
	return max(p.b.Reserve(now, n), t.class.Reserve(now, n))
}

// SetClock replaces the clock (tests move it by hand).
func (t *Throttle[K]) SetClock(now func() time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now = now
}
