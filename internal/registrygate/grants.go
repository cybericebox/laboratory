package registrygate

import (
	"sync"
	"time"
)

// A grant is the gate's memory that a manifest was authorised for a pod's reference on a node: a blob request carries only a digest, and
// it is allowed only when the digest is one that a manifest served on that claim lists (2.4.2). No new upstream call is made for it.

type grantKey struct{ node, seg, repo string } // repo is "<registry>/<repo>"

type grant struct {
	digests map[string]struct{}
	creds   Creds
	created time.Time
	expires time.Time
}

// Grants holds the live grants. Soft state: a restart loses it (2.4.2 says how a digest-pinned reference rebuilds it).
type Grants struct {
	// Grace is how long a grant lives after its last use (GATE_PULL_GRACE, default 2 minutes); MaxAge the longest it lives at all
	// (GATE_GRANT_MAX_AGE, default 1 hour).
	Grace  time.Duration
	MaxAge time.Duration
	Now    func() time.Time

	mu sync.Mutex
	m  map[grantKey]*grant
}

const maxGrants = 50000

func (g *Grants) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *Grants) grace() time.Duration {
	if g.Grace > 0 {
		return g.Grace
	}
	return 2 * time.Minute
}

func (g *Grants) maxAge() time.Duration {
	if g.MaxAge > 0 {
		return g.MaxAge
	}
	return time.Hour
}

// Add records digests (a manifest, its children, its config and layers) in the grant of (node, segment, repo), creating it with the
// credentials that proved access, and extends its life.
func (g *Grants) Add(node, seg, repo string, creds Creds, digests ...string) {
	now := g.now()
	k := grantKey{node, seg, repo}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[grantKey]*grant{}
	}
	gr := g.m[k]
	if gr == nil || !now.Before(gr.expires) {
		if len(g.m) >= maxGrants {
			g.pruneLocked(now)
		}
		if len(g.m) >= maxGrants {
			return
		}
		gr = &grant{digests: map[string]struct{}{}, created: now}
		g.m[k] = gr
	}
	gr.creds = creds
	for _, d := range digests {
		if d != "" {
			gr.digests[d] = struct{}{}
		}
	}
	gr.expires = minTime(now.Add(g.grace()), gr.created.Add(g.maxAge()))
}

// Has says whether digest is in a live grant of (node, segment, repo), and returns the credentials of the grant. Use extends the grant.
func (g *Grants) Has(node, seg, repo, digest string) (Creds, bool) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	gr := g.m[grantKey{node, seg, repo}]
	if gr == nil || !now.Before(gr.expires) {
		return Creds{}, false
	}
	if _, ok := gr.digests[digest]; !ok {
		return Creds{}, false
	}
	gr.expires = minTime(now.Add(g.grace()), gr.created.Add(g.maxAge()))
	return gr.creds, true
}

// Len is the number of live grants (for the metric).
func (g *Grants) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.m)
}

func (g *Grants) pruneLocked(now time.Time) {
	for k, gr := range g.m {
		if !now.Before(gr.expires) {
			delete(g.m, k)
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
