package registrygate

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"time"
)

// A grant is the gate's memory that a manifest was authorised for a pod's reference on a node: a blob request carries only a digest, and
// it is allowed only when the digest is one that a manifest served on that claim lists (2.4.2). No new upstream call is made for it.

type grantKey struct{ seg, repo string } // repo is "<registry>/<repo>"

type grant struct {
	digests map[string]struct{}
	creds   Creds
	created time.Time
	expires time.Time
}

// The bounds of the table: what one grant may hold, what one node may hold, and the whole table.
const (
	maxDigestsPerGrant = 4096
	maxGrantsPerNode   = 512
	maxGrants          = 50000
)

// Why a grant cannot be made. The caller answers the request with an error; nothing is dropped silently.
var (
	ErrGrantsFull    = errors.New("the node holds as many pull grants as it may")
	ErrTooManyBlobs  = errors.New("the manifest lists more digests than a grant holds")
	ErrBadDigest     = errors.New("the manifest lists something that is not a digest")
	digestFormatOnce = regexp.MustCompile(`^(sha256:[0-9a-f]{64}|sha512:[0-9a-f]{128})$`)
)

// Grants holds the live grants, by node. Soft state: a restart loses it (2.4.2 says how a digest-pinned reference rebuilds it).
type Grants struct {
	// Grace is how long a grant lives after its last use (GATE_PULL_GRACE, default 2 minutes); MaxAge the longest it lives at all
	// (GATE_GRANT_MAX_AGE, default 1 hour).
	Grace  time.Duration
	MaxAge time.Duration
	Now    func() time.Time

	mu    sync.Mutex
	m     map[string]map[grantKey]*grant // node -> grants
	total int
}

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
// credentials that proved access, and extends its life. It refuses, and records nothing, when a digest is malformed, when the grant would
// hold too many, or when the node (or the table) holds as many grants as it may.
func (g *Grants) Add(node, seg, repo string, creds Creds, digests ...string) error {
	for _, d := range digests {
		if d != "" && !digestFormatOnce.MatchString(d) {
			return ErrBadDigest
		}
	}
	now := g.now()
	k := grantKey{seg, repo}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]map[grantKey]*grant{}
	}
	mine := g.m[node]
	gr := mine[k]
	if gr != nil && !now.Before(gr.expires) {
		g.removeLocked(node, k)
		mine, gr = g.m[node], nil
	}
	if gr == nil {
		if len(mine) >= maxGrantsPerNode {
			g.pruneNodeLocked(node, now)
			mine = g.m[node]
		}
		if len(mine) >= maxGrantsPerNode || g.total >= maxGrants {
			return ErrGrantsFull
		}
		if len(digests) > maxDigestsPerGrant {
			return ErrTooManyBlobs
		}
		if mine == nil {
			mine = map[grantKey]*grant{}
			g.m[node] = mine
		}
		gr = &grant{digests: map[string]struct{}{}, created: now}
		mine[k] = gr
		g.total++
	}
	fresh := 0
	for _, d := range digests {
		if _, have := gr.digests[d]; d != "" && !have {
			fresh++
		}
	}
	if len(gr.digests)+fresh > maxDigestsPerGrant {
		return ErrTooManyBlobs
	}
	gr.creds = creds
	for _, d := range digests {
		if d != "" {
			gr.digests[d] = struct{}{}
		}
	}
	gr.expires = minTime(now.Add(g.grace()), gr.created.Add(g.maxAge()))
	return nil
}

// Has says whether digest is in a live grant of (node, segment, repo), and returns the credentials of the grant. Use extends the grant.
func (g *Grants) Has(node, seg, repo, digest string) (Creds, bool) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	gr := g.m[node][grantKey{seg, repo}]
	if gr == nil || !now.Before(gr.expires) {
		return Creds{}, false
	}
	if _, ok := gr.digests[digest]; !ok {
		return Creds{}, false
	}
	gr.expires = minTime(now.Add(g.grace()), gr.created.Add(g.maxAge()))
	return gr.creds, true
}

// Len is the number of grants held (for the metric).
func (g *Grants) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.total
}

func (g *Grants) removeLocked(node string, k grantKey) {
	if _, ok := g.m[node][k]; ok {
		delete(g.m[node], k)
		g.total--
		if len(g.m[node]) == 0 {
			delete(g.m, node)
		}
	}
}

// pruneNodeLocked drops the lapsed grants of one node: bounded by what a node may hold, so the hot path never scans the whole table.
func (g *Grants) pruneNodeLocked(node string, now time.Time) {
	for k, gr := range g.m[node] {
		if !now.Before(gr.expires) {
			g.removeLocked(node, k)
		}
	}
}

// Sweep drops every lapsed grant of every node; run it now and then in the background (Run), never per request.
func (g *Grants) Sweep() {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	for node := range g.m {
		g.pruneNodeLocked(node, now)
	}
}

// Run sweeps every interval until ctx ends.
func (g *Grants) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.Sweep()
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
