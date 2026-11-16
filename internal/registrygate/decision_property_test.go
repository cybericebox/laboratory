package registrygate

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The decision store is a state machine under one lock. These tests apply random interleavings of checks (started at unique, increasing
// logical times, finished in any order) from many goroutines and compare the final state with a sequential model.

type op struct {
	start   time.Time
	tenant  string
	out     Outcome
	settle  bool // SettleMissing instead of Record
	withRef bool // an Allowed with a known ref
}

func propOps(rng *rand.Rand, base time.Time, n int, settle bool) []op {
	tenants := []string{"a", "b", "c"}
	outs := []Outcome{Allowed, Allowed, Denied, NotFound, Unreachable}
	ops := make([]op, n)
	for i := range ops {
		ops[i] = op{
			start:   base.Add(time.Duration(i+1) * time.Millisecond), // unique, increasing
			tenant:  tenants[rng.Intn(len(tenants))],
			out:     outs[rng.Intn(len(outs))],
			settle:  settle && rng.Intn(4) == 0,
			withRef: rng.Intn(10) != 0,
		}
	}
	rng.Shuffle(len(ops), func(i, j int) { ops[i], ops[j] = ops[j], ops[i] })
	return ops
}

func runOps(s *Store, k DecisionKey, ops []op, workers int) {
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(ops) {
					return
				}
				o := ops[i]
				if o.settle {
					s.SettleMissing(k, o.start, o.out)
					continue
				}
				var kn *knownRef
				if o.withRef {
					kn = tk
				}
				s.Record(k, o.tenant, o.out, o.start, kn)
				s.Lookup(k, o.tenant)
			}
		}()
	}
	wg.Wait()
}

func TestStoreMatchesTheSequentialModel(t *testing.T) {
	base := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	for seed := int64(0); seed < 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		s := NewStore(DefaultTiming)
		s.Now = func() time.Time { return base }
		k := DecisionKey{Hash: "h", Repo: "up/org/img"}
		ops := propOps(rng, base, 120, false)
		runOps(s, k, ops, 8)

		// the model: the record is made by the latest-started effective check (an Allowed with a ref, or a Denied)
		var best *op
		var lastDeny time.Time
		for i := range ops {
			o := &ops[i]
			effective := o.out == Denied || o.out == Allowed && o.withRef
			if !effective {
				continue
			}
			if best == nil || o.start.After(best.start) {
				best = o
			}
			if o.out == Denied && o.start.After(lastDeny) {
				lastDeny = o.start
			}
		}
		s.mu.Lock()
		r := s.recs[k]
		s.mu.Unlock()
		if best == nil {
			if r != nil {
				t.Fatalf("seed %d: no effective check, no record", seed)
			}
			continue
		}
		if r == nil || !r.startedAt.Equal(best.start) {
			t.Fatalf("seed %d: the record is made by the latest-started check (%v), got %+v", seed, best.start, r)
		}
		if r.allowed != (best.out == Allowed) {
			t.Fatalf("seed %d: (a) a result overrode a record of a later-started check: allowed=%v, latest=%v", seed, r.allowed, best.out)
		}
		if r.allowed {
			if r.known == nil {
				t.Fatalf("seed %d: (b) an allow without a known ref", seed)
			}
			allowedBy := map[string]bool{}
			for _, o := range ops {
				if o.out == Allowed && o.withRef && o.start.After(lastDeny) {
					allowedBy[o.tenant] = true
				}
			}
			for ten := range r.verified {
				if !allowedBy[ten] {
					t.Fatalf("seed %d: (c) tenant %s verified by an allow that began before the refusal at %v", seed, ten, lastDeny)
				}
			}
			if _, ok := r.verified[best.tenant]; !ok {
				t.Fatalf("seed %d: the tenant of the latest allow is verified", seed)
			}
		} else {
			for _, ten := range []string{"a", "b", "c"} {
				if got := s.Lookup(k, ten); got == Serve || got == ServeStale {
					t.Fatalf("seed %d: a refused key serves %s", seed, ten)
				}
			}
		}
	}
}

// With SettleMissing mixed in the final state depends on the order of the steps, but the invariants hold in every order.
func TestStoreInvariantsHoldWithRevalidationsAndMissingRefs(t *testing.T) {
	base := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	for seed := int64(100); seed < 140; seed++ {
		rng := rand.New(rand.NewSource(seed))
		s := NewStore(DefaultTiming)
		s.Now = func() time.Time { return base }
		k := DecisionKey{Hash: "h", Repo: "up/org/img"}
		ops := propOps(rng, base, 150, true)
		runOps(s, k, ops, 8)

		var maxStart, maxDeny time.Time
		for _, o := range ops {
			if o.start.After(maxStart) {
				maxStart = o.start
			}
			if !o.settle && o.out == Denied && o.start.After(maxDeny) {
				maxDeny = o.start
			}
		}
		s.mu.Lock()
		r := s.recs[k]
		s.mu.Unlock()
		if r == nil {
			continue
		}
		if r.startedAt.After(maxStart) {
			t.Fatalf("seed %d: a record from the future", seed)
		}
		if r.allowed {
			if r.known == nil {
				t.Fatalf("seed %d: (b) an allow without a known ref", seed)
			}
			if !r.startedAt.After(maxDeny) {
				t.Fatalf("seed %d: (c) allowed by a check that began at %v, before the refusal at %v", seed, r.startedAt, maxDeny)
			}
		}
	}
}
