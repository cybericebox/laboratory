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
	// Denied: 401, 403 or 404.
	Denied
	// Unreachable: 429, 5xx, a timeout, DNS: not a denial.
	Unreachable
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
	// verified is the set of tenants that passed a fresh check for this key; an allow of one tenant says nothing about another.
	verified map[string]struct{}
}

// Store keeps the decisions in memory: soft state. A restart loses it, and the cost is one fresh check per key, never a wrong allow.
type Store struct {
	T   Timing
	Now func() time.Time

	mu   sync.Mutex
	recs map[DecisionKey]*record
}

// NewStore is a store with the given timing.
func NewStore(t Timing) *Store {
	return &Store{T: t, Now: time.Now, recs: map[DecisionKey]*record{}}
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
	return ServeStale
}

// Record stores the outcome of a fresh check and says whether the request may be served. An allow adds the tenant to the verified set
// (a refusal clears it, for every tenant at once: the record is keyed by hash, not by tenant). An unreachable upstream changes nothing:
// the request is served only for a tenant already verified within StaleIfError, and for an anonymous repository that was allowed
// earlier, whatever its age (public data, no credential to prove).
func (s *Store) Record(k DecisionKey, tenant string, o Outcome) (serve bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	r := s.recs[k]
	switch o {
	case Allowed:
		if r == nil || !r.allowed {
			r = &record{allowed: true, verified: map[string]struct{}{}}
			s.recs[k] = r
		}
		r.checkedAt = now
		r.verified[tenant] = struct{}{}
		return true
	case Denied:
		s.recs[k] = &record{allowed: false, checkedAt: now}
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
}
