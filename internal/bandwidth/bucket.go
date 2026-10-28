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

// burstFor is a tenth of a second of the rate, at least enough for a few full-size packets.
func burstFor(rate float64) float64 {
	return max(rate/10, 16384)
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
	return &Throttle[K]{now: time.Now, window: window, parts: map[K]*part{}}
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

// rescanLocked counts the active participants, forgets the long silent ones and gives everyone the new rate.
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
	rate := t.rateLocked()
	for _, p := range t.parts {
		p.b.SetRate(rate)
	}
}

func (t *Throttle[K]) partLocked(key K, now time.Time) *part {
	p, ok := t.parts[key]
	if !ok {
		p = &part{}
		t.parts[key] = p
		p.last = now
		t.rescanLocked(now) // a newcomer changes everybody's share at once
		return p
	}
	p.last = now
	if now.After(t.nextScan) {
		t.rescanLocked(now)
	}
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
