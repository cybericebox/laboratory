package registrygate

import (
	"strings"
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
	if !s.Record(key, "a", Allowed, c.now(), tk) {
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
	s.Record(key, "a", Allowed, c.now(), tk)
	s.Record(key, "a", Allowed, c.now(), tk)
	if got := s.Lookup(key, "b"); got != Check {
		t.Fatalf("a fresh record of tenant a does not verify tenant b: %v", got)
	}
	s.Record(key, "b", Allowed, c.now(), tk)
	if got := s.Lookup(key, "b"); got != Serve {
		t.Fatalf("tenant b verified: %v", got)
	}
	if got := s.Lookup(key, "a"); got != Serve {
		t.Fatalf("tenant a stays verified: %v", got)
	}
}

func TestDenyIsClusterWideAndShortLived(t *testing.T) {
	s, c := newTestStore()
	s.Record(key, "a", Allowed, c.now(), tk)
	s.Record(key, "b", Allowed, c.now(), tk)
	if s.Record(key, "a", Denied, c.now(), tk) {
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
	s.Record(key, "a", Allowed, c.now(), tk)
	if got := s.Lookup(key, "b"); got != Check {
		t.Fatalf("the verified set was cleared by the deny: %v", got)
	}
}

func TestDifferentHashOrRepositoryAreOtherDecisions(t *testing.T) {
	s, c := newTestStore()
	s.Record(key, "a", Allowed, c.now(), tk)
	if s.Lookup(DecisionKey{Hash: "h2", Repo: key.Repo}, "a") != Check {
		t.Fatal("another credential is another decision")
	}
	if s.Lookup(DecisionKey{Hash: key.Hash, Repo: "ghcr.io/org/other"}, "a") != Check {
		t.Fatal("another repository is another decision")
	}
}

func TestStaleIfError(t *testing.T) {
	s, c := newTestStore()
	s.Record(key, "a", Allowed, c.now(), tk)
	c.advance(9 * time.Minute)
	if !s.Record(key, "a", Unreachable, c.now(), tk) {
		t.Fatal("a verified tenant inside stale-if-error is served while upstream is unreachable")
	}
	c.advance(2 * time.Minute)
	if s.Record(key, "a", Unreachable, c.now(), tk) {
		t.Fatal("older than stale-if-error: refused (503), not served")
	}
	if s.Record(key, "new", Unreachable, c.now(), tk) {
		t.Fatal("a new tenant is refused while upstream is unreachable")
	}
	if s.Record(DecisionKey{Hash: "other", Repo: key.Repo}, "a", Unreachable, c.now(), tk) {
		t.Fatal("no record: refused")
	}
	// an unreachable upstream is not a denial: the record is kept
	s.Record(key, "a", Allowed, c.now(), tk)
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
	s.Record(anon, "", Allowed, c.now(), tk)
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
	if !s.Record(anon, "", Unreachable, c.now(), tk) {
		t.Fatal("an anonymous image allowed earlier is served for the whole outage")
	}
	s.Record(anon, "", Denied, c.now(), tk)
	if s.Lookup(anon, "") != Refuse {
		t.Fatal("not public: refused, containerd retries with credentials")
	}
	if s.Record(DecisionKey{Repo: "docker.io/never/seen"}, "", Unreachable, c.now(), tk) {
		t.Fatal("never allowed: refused during the outage")
	}
}

// A check that began before another one never replaces what the other found: a slow check that began before a revocation finishes later
// and must not allow again what a later check refused (and the reverse: a slow refusal does not undo a newer allow).
// tk is the ref that an allow proved.
var tk = &knownRef{Kind: kindManifests, Ref: "sha256:" + strings.Repeat("a", 64)}

func TestAnOlderCheckNeverOverridesANewerRecord(t *testing.T) {
	s, c := newTestStore()
	s.Record(key, "a", Allowed, c.now(), tk)
	slowStart := c.now() // check A begins: it will answer "allowed"
	c.advance(time.Second)
	fastStart := c.now() // check B begins after the revocation and answers "denied" first
	if s.Record(key, "a", Denied, fastStart, tk) {
		t.Fatal("B refuses")
	}
	c.advance(time.Second)
	if s.Record(key, "a", Allowed, slowStart, tk) {
		t.Fatal("the slow allow that began before the refusal must not serve")
	}
	if got := s.Lookup(key, "a"); got != Refuse {
		t.Fatalf("the record stays denied: %v", got)
	}
	if got := s.Lookup(key, "b"); got != Refuse {
		t.Fatalf("for every tenant of the hash: %v", got)
	}

	// the reverse: a slow denial that began before a newer allow does not undo it
	c.advance(time.Minute)
	slowDeny := c.now()
	c.advance(time.Second)
	s.Record(key, "a", Allowed, c.now(), tk)
	if !s.Record(key, "a", Denied, slowDeny, tk) {
		t.Fatal("the tenant stays verified by the newer allow")
	}
	if got := s.Lookup(key, "a"); got != Serve {
		t.Fatalf("%v", got)
	}
	// a check that began later does replace it
	c.advance(time.Second)
	s.Record(key, "a", Denied, c.now(), tk)
	if got := s.Lookup(key, "a"); got != Refuse {
		t.Fatalf("%v", got)
	}
}

// A missing object (404) is not a refusal of the repository: it changes no decision and is never served.
func TestNotFoundLeavesTheDecisionAlone(t *testing.T) {
	s, c := newTestStore()
	if s.Record(key, "a", NotFound, c.now(), tk) {
		t.Fatal("a missing object is not served")
	}
	if got := s.Lookup(key, "a"); got != Check {
		t.Fatalf("no record was made by a 404: %v", got)
	}
	s.Record(key, "a", Allowed, c.now(), tk)
	if s.Record(key, "a", NotFound, c.now(), tk) {
		t.Fatal("a missing object is not served even in an allowed repository")
	}
	if got := s.Lookup(key, "a"); got != Serve {
		t.Fatalf("the allow stands: %v", got)
	}
}

// A blob proof belongs to the credential, the repository and the digest, and lasts MaxAge.
func TestBlobProofs(t *testing.T) {
	s, c := newTestStore()
	k := BlobKey{Hash: "h", Repo: "up/org/img", Digest: "sha256:" + string(make([]byte, 0))}
	if s.BlobProven(k) {
		t.Fatal("nothing proven yet")
	}
	s.BlobProve(k, c.now())
	if !s.BlobProven(k) {
		t.Fatal("proven")
	}
	for _, o := range []BlobKey{{"h2", k.Repo, k.Digest}, {"h", "up/org/other", k.Digest}, {"h", k.Repo, k.Digest + "x"}} {
		if s.BlobProven(o) {
			t.Fatalf("a proof is not transferable: %+v", o)
		}
	}
	c.advance(3 * time.Minute)
	if s.BlobProven(k) {
		t.Fatal("a proof is kept for MaxAge")
	}
}

func TestForgetKeepsAnonymousAndRecentDecisions(t *testing.T) {
	s, c := newTestStore()
	s.Record(key, "a", Allowed, c.now(), tk)
	anon := DecisionKey{Repo: "docker.io/library/nginx"}
	s.Record(anon, "", Allowed, c.now(), tk)
	c.advance(21 * time.Minute)
	s.Forget()
	if s.Len() != 1 {
		t.Fatalf("the old credentialed decision goes, the anonymous one stays: %d", s.Len())
	}
}

// A refusal that arrives after a newer allow is dropped as a record, but it still strips the tenants that were verified before it began.
func TestALateRefusalStripsOlderVerifications(t *testing.T) {
	s, c := newTestStore()
	t0 := c.now()
	s.Record(key, "old", Allowed, t0, tk)
	c.advance(time.Second)
	refusal := c.now()
	c.advance(time.Second)
	s.Record(key, "new", Allowed, c.now(), tk) // began after the refusal
	if s.Record(key, "old", Denied, refusal, tk) {
		t.Fatal("old was verified before the refusal began")
	}
	if got := s.Lookup(key, "old"); got != Check {
		t.Fatalf("old must be checked again: %v", got)
	}
	if got := s.Lookup(key, "new"); got != Serve {
		t.Fatalf("new began after the refusal: %v", got)
	}
}
