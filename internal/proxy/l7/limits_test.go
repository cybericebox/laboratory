package l7

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// A drained bucket is never dropped to make room: its key does not get a fresh burst by being evicted.
func TestBucketSetNeverEvictsADrainedBucket(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newBucketSet(1, 3, 3)
	b.allMul = 0
	b.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !b.allow("a") {
			t.Fatalf("take %d", i)
		}
	}
	b.allow("b")
	b.allow("c") // the table is full: a is drained, b and c each spent one token
	now = now.Add(500 * time.Millisecond)
	b.allow("d") // evicts nothing that is not full
	if b.allow("a") {
		t.Fatal("a drained key was reset by eviction")
	}
	now = now.Add(10 * time.Second) // b and c are full again and may go; a has been refilled too, so it may go as well, but only because it is full
	b.allow("e")
	if len(b.keys) > 3 {
		t.Fatalf("table = %d, cap 3", len(b.keys))
	}
}

// Keys that do not fit in a table of buckets that are not full share one modest overflow bucket instead of a burst each.
func TestBucketSetOverflowIsSharedAndModest(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newBucketSet(1, 3, 2)
	b.allMul = 0
	b.now = func() time.Time { return now }
	b.allow("a")
	b.allow("b") // full table, both a little spent
	passed := 0
	for i := 0; i < 100; i++ {
		if b.allow(fmt.Sprintf("new%d", i)) {
			passed++
		}
	}
	if want := int(3 * b.overflowMul); passed != want {
		t.Fatalf("the newcomers passed %d together, want the overflow burst %d", passed, want)
	}
	if len(b.keys) != 2 {
		t.Fatalf("table = %d, the cap is 2", len(b.keys))
	}
}

// The address (fine) and the network (coarse) tiers: many addresses of one /24 (or /48) spend the network's budget; another network is free.
func TestHandoff_CoarseTiersLimitANetwork(t *testing.T) {
	f := newGrantFixture(t)
	f.handler.WithAuthRateLimit(1, 3)
	v4 := func(i int) string { return fmt.Sprintf("198.51.100.%d:1", i) }
	limited := false
	for i := 1; i <= 250; i++ {
		if f.openFrom(v4(i), "garbage").Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("250 addresses of one /24 must pass the network bucket")
	}
	if f.openFrom("198.51.100.254:1", "garbage").Code != http.StatusTooManyRequests {
		t.Fatal("a fresh address of the limited /24 shares its bucket")
	}
	if rec := f.openFrom("198.51.101.1:1", f.grant(t, nil)); rec.Code != http.StatusSeeOther {
		t.Fatalf("another /24: %d", rec.Code)
	}

	limited = false
	for i := 0; i < 250; i++ {
		if f.openFrom(fmt.Sprintf("[2001:db8:5:%x::1]:1", i), "garbage").Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("250 /64 networks of one /48 must pass the /48 bucket")
	}
	if rec := f.openFrom("[2001:db8:6::1]:1", f.grant(t, nil)); rec.Code != http.StatusSeeOther {
		t.Fatalf("another /48: %d", rec.Code)
	}
}

// The work garbage can cause is bounded by the cap on verifications in flight: with it full the answer is 503 at once, without verifying, and a
// valid link goes through when there is room again. Nothing is remembered that could refuse anybody afterwards.
func TestHandoff_VerificationCapAnswers503(t *testing.T) {
	f := newGrantFixture(t)
	f.handler.verifySem = make(chan struct{}, 2)
	f.handler.verifySem <- struct{}{}
	f.handler.verifySem <- struct{}{}
	good := f.grant(t, nil)
	rec := f.openFrom("203.0.113.1:1", good)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("full cap: %d retry-after=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	<-f.handler.verifySem
	if rec := f.openFrom("203.0.113.1:1", good); rec.Code != http.StatusSeeOther {
		t.Fatalf("with a slot free: %d", rec.Code)
	}
	if len(f.handler.verifySem) != 1 {
		t.Fatalf("a slot leaked: %d in use", len(f.handler.verifySem))
	}
}

// Under a concurrent flood the answers are only "refused" (401 garbage, 503 no slot) and the slots all come back.
func TestHandoff_VerificationCapUnderConcurrentFlood(t *testing.T) {
	f := newGrantFixture(t)
	f.handler.verifySem = make(chan struct{}, 2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := f.openFrom(fmt.Sprintf("10.%d.%d.1:1", i/200, i%200), "garbage")
			mu.Lock()
			codes[rec.Code]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	for code := range codes {
		if code != http.StatusUnauthorized && code != http.StatusServiceUnavailable {
			t.Fatalf("unexpected answer %d in %v", code, codes)
		}
	}
	if len(f.handler.verifySem) != 0 {
		t.Fatalf("slots leaked: %d", len(f.handler.verifySem))
	}
	if rec := f.openFrom("203.0.113.9:1", f.grant(t, nil)); rec.Code != http.StatusSeeOther {
		t.Fatalf("a valid link after the flood: %d", rec.Code)
	}
}
