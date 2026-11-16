package registrygate

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestStore() (*Store, *clock) {
	c := &clock{t: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	s := NewStore(DefaultTiming)
	s.Now = c.now
	return s, c
}

var key = DecisionKey{Hash: "h1", Repo: "ghcr.io/org/img"}

// The table of 5.2, row by row.
func TestDecisionTable(t *testing.T) {
	s, c := newTestStore()
	if got := s.Lookup(key, "a"); got != Check {
		t.Fatalf("no record: %v", got)
	}
	if !s.Record(key, "a", Allowed) {
		t.Fatal("an allow serves")
	}
	if got := s.Lookup(key, "a"); got != Serve {
		t.Fatalf("allowed, tenant verified, fresh: %v", got)
	}
	c.advance(time.Minute + 59*time.Second)
	if got := s.Lookup(key, "a"); got != Serve {
		t.Fatalf("still inside max-age: %v", got)
	}
	c.advance(2 * time.Second)
	if got := s.Lookup(key, "a"); got != ServeStale {
		t.Fatalf("allowed, tenant verified, stale: %v", got)
	}
	if got := s.Lookup(key, "b"); got != Check {
		t.Fatalf("a new tenant for the hash is a mandatory fresh check, whatever the age: %v", got)
	}
	c.advance(9 * time.Minute)
	if got := s.Lookup(key, "a"); got != Check {
		t.Fatalf("older than stale-if-error even a verified tenant is checked before it is served: %v", got)
	}
	s.Record(key, "a", Allowed)
	s.Record(key, "a", Allowed)
	if got := s.Lookup(key, "b"); got != Check {
		t.Fatalf("a fresh record of tenant a does not verify tenant b: %v", got)
	}
	s.Record(key, "b", Allowed)
	if got := s.Lookup(key, "b"); got != Serve {
		t.Fatalf("tenant b verified: %v", got)
	}
	if got := s.Lookup(key, "a"); got != Serve {
		t.Fatalf("tenant a stays verified: %v", got)
	}
}

func TestDenyIsClusterWideAndShortLived(t *testing.T) {
	s, c := newTestStore()
	s.Record(key, "a", Allowed)
	s.Record(key, "b", Allowed)
	if s.Record(key, "a", Denied) {
		t.Fatal("a refusal does not serve")
	}
	// the refusal of tenant a applies to tenant b at once: same hash, same repository
	if got := s.Lookup(key, "b"); got != Refuse {
		t.Fatalf("a deny reaches every tenant of the hash: %v", got)
	}
	c.advance(29 * time.Second)
	if got := s.Lookup(key, "a"); got != Refuse {
		t.Fatalf("a refusal is repeated for 30 s without asking upstream: %v", got)
	}
	c.advance(2 * time.Second)
	if got := s.Lookup(key, "a"); got != Check {
		t.Fatalf("older than 30 s: a fresh check: %v", got)
	}
	// the deny cleared the verified set: after an allow, tenant b must be checked again
	s.Record(key, "a", Allowed)
	if got := s.Lookup(key, "b"); got != Check {
		t.Fatalf("the verified set was cleared by the deny: %v", got)
	}
}

func TestDifferentHashOrRepositoryAreOtherDecisions(t *testing.T) {
	s, _ := newTestStore()
	s.Record(key, "a", Allowed)
	if s.Lookup(DecisionKey{Hash: "h2", Repo: key.Repo}, "a") != Check {
		t.Fatal("another credential is another decision")
	}
	if s.Lookup(DecisionKey{Hash: key.Hash, Repo: "ghcr.io/org/other"}, "a") != Check {
		t.Fatal("another repository is another decision")
	}
}

func TestStaleIfError(t *testing.T) {
	s, c := newTestStore()
	s.Record(key, "a", Allowed)
	c.advance(9 * time.Minute)
	if !s.Record(key, "a", Unreachable) {
		t.Fatal("a verified tenant inside stale-if-error is served while upstream is unreachable")
	}
	c.advance(2 * time.Minute)
	if s.Record(key, "a", Unreachable) {
		t.Fatal("older than stale-if-error: refused (503), not served")
	}
	if s.Record(key, "new", Unreachable) {
		t.Fatal("a new tenant is refused while upstream is unreachable")
	}
	if s.Record(DecisionKey{Hash: "other", Repo: key.Repo}, "a", Unreachable) {
		t.Fatal("no record: refused")
	}
	// an unreachable upstream is not a denial: the record is kept
	s.Record(key, "a", Allowed)
	if got := s.Lookup(key, "a"); got != Serve {
		t.Fatalf("%v", got)
	}
}

func TestAnonymousDecisions(t *testing.T) {
	s, c := newTestStore()
	anon := DecisionKey{Repo: "docker.io/library/nginx"}
	if s.Lookup(anon, "") != Check {
		t.Fatal("unknown: ask upstream anonymously")
	}
	s.Record(anon, "", Allowed)
	if s.Lookup(anon, "") != Serve {
		t.Fatal("public: serve")
	}
	c.advance(3 * time.Minute)
	if s.Lookup(anon, "") != ServeStale {
		t.Fatal("stale: serve and revalidate")
	}
	c.advance(10 * time.Hour)
	if s.Lookup(anon, "") != Check {
		t.Fatal("very old: a fresh check, which in an outage is answered by the earlier allow")
	}
	if !s.Record(anon, "", Unreachable) {
		t.Fatal("an anonymous image allowed earlier is served for the whole outage")
	}
	s.Record(anon, "", Denied)
	if s.Lookup(anon, "") != Refuse {
		t.Fatal("not public: refused, containerd retries with credentials")
	}
	if s.Record(DecisionKey{Repo: "docker.io/never/seen"}, "", Unreachable) {
		t.Fatal("never allowed: refused during the outage")
	}
}

func TestForgetKeepsAnonymousAndRecentDecisions(t *testing.T) {
	s, c := newTestStore()
	s.Record(key, "a", Allowed)
	anon := DecisionKey{Repo: "docker.io/library/nginx"}
	s.Record(anon, "", Allowed)
	c.advance(21 * time.Minute)
	s.Forget()
	if s.Len() != 1 {
		t.Fatalf("the old credentialed decision goes, the anonymous one stays: %d", s.Len())
	}
}
