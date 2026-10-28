package bandwidth

import (
	"testing"
	"time"
)

func TestBucketAllowAndRefill(t *testing.T) {
	var b Bucket
	t0 := time.Unix(1000, 0)
	if !b.Allow(t0, 1<<30) {
		t.Fatal("a bucket without a rate is unlimited")
	}
	b.SetRate(8000) // 1 MB/s, burst 100 kB
	if !b.Allow(t0, 100_000) {
		t.Fatal("the first burst must pass")
	}
	if b.Allow(t0, 1) {
		t.Fatal("the burst is spent")
	}
	if !b.Allow(t0.Add(100*time.Millisecond), 99_999) {
		t.Fatal("100 ms at 1 MB/s refills 100 kB")
	}
}

func TestBucketReservePacesALargeWrite(t *testing.T) {
	var b Bucket
	b.SetRate(8000) // 1 MB/s
	t0 := time.Unix(1000, 0)
	if d := b.Reserve(t0, 100_000); d != 0 {
		t.Fatalf("within the burst: %v", d)
	}
	if d := b.Reserve(t0, 1_000_000); d < 990*time.Millisecond || d > 1010*time.Millisecond {
		t.Fatalf("1 MB on top of an empty bucket waits about a second, waits %v", d)
	}
}

func TestBucketRateChangeKeepsTokensCapped(t *testing.T) {
	var b Bucket
	b.SetRate(80000) // burst 1 MB
	t0 := time.Unix(1000, 0)
	b.Allow(t0, 1)
	b.SetRate(800) // burst 16384 (the floor)
	if b.Allow(t0, 20000) {
		t.Fatal("tokens must be cut to the new burst")
	}
	if !b.Allow(t0, 16000) {
		t.Fatal("the new burst is there")
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestThrottle(c *clock) *Throttle[string] {
	th := NewThrottle[string](5 * time.Second)
	th.now = c.now
	return th
}

func TestThrottleDisabledAllowsEverything(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	th := newTestThrottle(c)
	if th.Enabled() || !th.Allow("a", 1<<30) || th.Delay("a", 1<<30) != 0 {
		t.Fatal("a throttle without limits is off")
	}
}

func TestThrottleFairShareFollowsActiveParticipants(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	th := newTestThrottle(c)
	th.SetLimits(100_000, 50_000) // class 100 Mbit/s, cap 50 Mbit/s
	th.Allow("a", 1)
	if got := th.Rate(); got != 50_000 {
		t.Fatalf("one participant gets the cap, got %d", got)
	}
	for _, k := range []string{"b", "c", "d"} {
		th.Allow(k, 1)
	}
	if got, active := th.Rate(), th.Active(); active != 4 || got != 25_000 {
		t.Fatalf("four active share the class: active %d rate %d", active, got)
	}
	// 6 s of silence from a..c: only d sends, the others stop counting
	c.t = c.t.Add(6 * time.Second)
	th.Allow("d", 1)
	th.Allow("d", 1)
	if got, active := th.Rate(), th.Active(); active != 1 || got != 50_000 {
		t.Fatalf("after the others went quiet: active %d rate %d", active, got)
	}
	// 30 s later the silent ones are forgotten
	c.t = c.t.Add(30 * time.Second)
	th.Allow("d", 1)
	th.mu.Lock()
	n := len(th.parts)
	th.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d participants kept", n)
	}
}

func TestThrottleHoldsAParticipantToItsRate(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	th := newTestThrottle(c)
	th.SetLimits(0, 8000) // cap 8 Mbit/s = 1 MB/s, no class
	sent := 0
	for i := 0; i < 1000; i++ { // 10 s of 1400-byte packets offered at 100 packets per 10 ms tick = far above the rate
		c.t = c.t.Add(10 * time.Millisecond)
		for j := 0; j < 100; j++ {
			if th.Allow("a", 1400) {
				sent += 1400
			}
		}
	}
	// 10 s at 1 MB/s plus the initial burst
	if sent < 9_500_000 || sent > 10_300_000 {
		t.Fatalf("a participant capped at 1 MB/s sent %d bytes in 10 s", sent)
	}
}

func TestThrottleClassBoundsAllParticipantsTogether(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	th := newTestThrottle(c)
	th.SetLimits(8000, 0) // class 1 MB/s shared, no per-participant cap
	sent := 0
	for i := 0; i < 1000; i++ {
		c.t = c.t.Add(10 * time.Millisecond)
		for _, k := range []string{"a", "b", "c", "d"} {
			for j := 0; j < 50; j++ {
				if th.Allow(k, 1400) {
					sent += 1400
				}
			}
		}
	}
	if sent < 9_000_000 || sent > 10_500_000 {
		t.Fatalf("four participants of a 1 MB/s class sent %d bytes in 10 s", sent)
	}
}

func TestThrottleDelayPaces(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	th := newTestThrottle(c)
	th.SetLimits(0, 8000) // 1 MB/s
	th.Delay("a", 100_000)
	// a newcomer starts with a small burst (16 kB): the first 100 kB is already 84 kB in debt, and 500 kB more is 584 kB of waiting
	if d := th.Delay("a", 500_000); d < 570*time.Millisecond || d > 600*time.Millisecond {
		t.Fatalf("delay %v", d)
	}
}

func TestThrottleKeysAreBounded(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	th := newTestThrottle(c)
	th.SetMaxKeys(100)
	th.SetLimits(8000, 800)
	for i := 0; i < 10000; i++ {
		th.Allow(string(rune('a'+i%26))+string(rune(i)), 100)
	}
	th.mu.Lock()
	n := len(th.parts)
	over := th.overflow != nil
	th.mu.Unlock()
	if n > 100 || !over {
		t.Fatalf("tracked %d keys (max 100), overflow bucket %v", n, over)
	}
}

func TestThrottleOverflowKeysShareOneBucket(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	th := newTestThrottle(c)
	th.SetMaxKeys(1)
	th.SetLimits(0, 800) // 100 kB/s each
	th.Allow("known", 1)
	passed := 0
	for tick := 0; tick < 100; tick++ { // 1 s: a hundred strangers each try 20 kB
		c.t = c.t.Add(10 * time.Millisecond)
		for i := 0; i < 100; i++ {
			if th.Allow(string(rune(1000+i)), 20000) {
				passed += 20000
			}
		}
	}
	// one shared bucket: about 100 kB/s plus its small first burst, not a hundred of them
	if passed > 300_000 {
		t.Fatalf("100 unknown keys passed %d bytes in a second: they must share one bucket", passed)
	}
}

func TestThrottleNewcomersDoNotTriggerARescan(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	th := newTestThrottle(c)
	th.SetLimits(8000, 0)
	th.Allow("a", 1)
	th.mu.Lock()
	next := th.nextScan
	th.mu.Unlock()
	for i := 0; i < 1000; i++ {
		th.Allow(string(rune(2000+i)), 1)
	}
	th.mu.Lock()
	same := th.nextScan.Equal(next)
	active := th.active
	th.mu.Unlock()
	if !same {
		t.Fatal("a newcomer must not rescan every participant")
	}
	if active != 1001 {
		t.Fatalf("a newcomer is counted at once: %d", active)
	}
}

func TestThrottleNewcomerStartsWithASmallBurst(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	th := newTestThrottle(c)
	th.SetLimits(0, 80000) // 10 MB/s: burst would be 1 MB
	if !th.Allow("new", 10000) {
		t.Fatal("a few packets pass")
	}
	if th.Allow("new", 1_000_000) {
		t.Fatal("a newcomer must not spend a full burst at once")
	}
}
