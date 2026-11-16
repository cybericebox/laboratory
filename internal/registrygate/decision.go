package registrygate

import (
	"sync"
	"time"
)

// Action is what the gate does with a request on the cache path, given what it remembers (docs/research/2026-11-16-zot-tenant-cache.md,
// section 5.2, the table).
type Action int

const (
	// Serve: the decision is fresh and the tenant is verified for this hash and repository.
	Serve Action = iota
	// ServeStale: the decision is old but the tenant is verified: serve now, check again in the background.
	ServeStale
	// Check: ask upstream now, before serving (no record, a new tenant for the hash, or a refusal old enough to ask again).
	Check
	// Refuse: a refusal younger than DeniedFor is repeated without asking upstream.
	Refuse
)

// Outcome is the result of a check against the upstream registry.
type Outcome int

const (
	// Allowed: the HEAD of the object with the pull's credentials answered 200.
	Allowed Outcome = iota
	// Denied: 401, 403 or a refused token: the credentials have no right to the repository.
	Denied
	// Unreachable: 429, 5xx, a timeout, DNS, a redirect that did not end in a 200: not a denial.
	Unreachable
	// NotFound: 404, the object is not there. It says nothing about the repository, so it never changes a decision.
	NotFound
)

// Timing is the set of intervals of the decision table.
type Timing struct {
	// MaxAge is how long a fresh check is reused for a verified tenant (2m).
	MaxAge time.Duration
	// DeniedFor is how long a refusal is repeated without asking upstream (30s).
	DeniedFor time.Duration
	// StaleIfError is how long a verified pull is served while upstream is unreachable (10m).
	StaleIfError time.Duration
}

// DefaultTiming is the chart's default.
var DefaultTiming = Timing{MaxAge: 2 * time.Minute, DeniedFor: 30 * time.Second, StaleIfError: 10 * time.Minute}

// DecisionKey names a decision: the hash of the credentials (HMAC with the gate's key; it never leaves the gate) and the repository
// "<registry>/<repo>". The empty hash is the anonymous decision of a repository.
type DecisionKey struct {
	Hash string
	Repo string
}

type record struct {
	allowed   bool
	checkedAt time.Time
	// startedAt is when the check that made this record began: a result of a check that began earlier never replaces it (a slow check
	// that began before a revocation must not allow again what a later check refused).
	startedAt time.Time
	// verified is the set of tenants that passed a fresh check for this key; an allow of one tenant says nothing about another.
	verified map[string]struct{}
}

// Store keeps the decisions in memory: soft state. A restart loses it, and the cost is one fresh check per key, never a wrong allow.
type Store struct {
	T   Timing
	Now func() time.Time

	mu    sync.Mutex
	recs  map[DecisionKey]*record
	blobs map[BlobKey]time.Time
}

// NewStore is a store with the given timing.
func NewStore(t Timing) *Store {
	return &Store{T: t, Now: time.Now, recs: map[DecisionKey]*record{}, blobs: map[BlobKey]time.Time{}}
}

// Lookup is the table of 5.2 for a key and a tenant ("" for the anonymous decision, where the tenant plays no part).
func (s *Store) Lookup(k DecisionKey, tenant string) Action {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recs[k]
	if r == nil {
		return Check
	}
	age := s.Now().Sub(r.checkedAt)
	if !r.allowed {
		if age < s.T.DeniedFor {
			return Refuse
		}
		return Check
	}
	if k.Hash != "" {
		if _, ok := r.verified[tenant]; !ok {
			return Check // a new tenant for this hash: a mandatory fresh check
		}
	}
	if age <= s.T.MaxAge {
		return Serve
	}
	if age <= s.T.StaleIfError {
		return ServeStale
	}
	// older than any stale answer may be: ask upstream before serving, so an outage then ends in a refusal (5.2, point 5)
	return Check
}

// Record stores the outcome of a fresh check that began at started (Now() taken before the first upstream call) and says whether the
// request may be served. An allow adds the tenant to the verified set (a refusal clears it, for every tenant at once: the record is
// keyed by hash, not by tenant). A result never replaces a record made by a check that began LATER: it is dropped, and the request is
// answered by what the newer record says. An unreachable upstream and a missing object change nothing: the request is served only
// for a tenant already verified within StaleIfError, and for an anonymous repository that was allowed earlier, whatever its age
// (public data, no credential to prove).
func (s *Store) Record(k DecisionKey, tenant string, o Outcome, started time.Time) (serve bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	r := s.recs[k]
	if o == NotFound {
		return false // a missing object changes no decision and is never served
	}
	if r != nil && started.Before(r.startedAt) && (o == Allowed || o == Denied) {
		return s.verifiedLocked(k, r, tenant, now)
	}
	switch o {
	case Allowed:
		if r == nil || !r.allowed {
			r = &record{allowed: true, verified: map[string]struct{}{}}
			s.recs[k] = r
		}
		r.checkedAt, r.startedAt = now, started
		r.verified[tenant] = struct{}{}
		return true
	case Denied:
		s.recs[k] = &record{allowed: false, checkedAt: now, startedAt: started}
		return false
	}
	if r == nil || !r.allowed {
		return false
	}
	if k.Hash == "" {
		return true
	}
	if _, ok := r.verified[tenant]; !ok {
		return false
	}
	return now.Sub(r.checkedAt) <= s.T.StaleIfError
}

// verifiedLocked says whether the record lets the tenant be served: an allowed record that holds the tenant, within StaleIfError.
func (s *Store) verifiedLocked(k DecisionKey, r *record, tenant string, now time.Time) bool {
	if !r.allowed {
		return false
	}
	if k.Hash != "" {
		if _, ok := r.verified[tenant]; !ok {
			return false
		}
	}
	return now.Sub(r.checkedAt) <= s.T.StaleIfError
}

// BlobKey names the proof that a blob is in a repository for a credential.
type BlobKey struct {
	Hash   string
	Repo   string
	Digest string
}

// BlobProven says whether a HEAD of exactly this blob in exactly this repository with these credentials answered 200 within MaxAge. A
// digest that a manifest lists, or that another repository holds, is no proof that the repository has it: a caller who controls an
// upstream repository can list any digest in a manifest of his own.
func (s *Store) BlobProven(k BlobKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.blobs[k]
	return ok && s.Now().Sub(at) <= s.T.MaxAge
}

// BlobProve records the 200 of such a HEAD, which began at started; a proof is kept for MaxAge and the table is bounded.
func (s *Store) BlobProve(k BlobKey, started time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.blobs) >= maxBlobProofs {
		cutoff := s.Now().Add(-s.T.MaxAge)
		for old, at := range s.blobs {
			if at.Before(cutoff) {
				delete(s.blobs, old)
			}
		}
		if len(s.blobs) >= maxBlobProofs {
			return
		}
	}
	s.blobs[k] = started
}

// maxBlobProofs bounds the table of blob proofs.
const maxBlobProofs = 100000

// Len is the number of decisions held (for the metric).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recs)
}

// Forget drops decisions that are older than twice the longest window they can still matter for; called now and then so the table does
// not grow with every credential ever seen.
func (s *Store) Forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.Now().Add(-2 * s.T.StaleIfError)
	for k, r := range s.recs {
		if k.Hash != "" && r.checkedAt.Before(cutoff) {
			delete(s.recs, k)
		}
	}
	for k, at := range s.blobs {
		if at.Before(s.Now().Add(-s.T.MaxAge)) {
			delete(s.blobs, k)
		}
	}
}
