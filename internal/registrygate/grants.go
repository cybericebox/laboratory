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
	// digests is every digest the grant allows: the manifests served and what they list.
	digests map[string]struct{}
	// roots: manifest digest -> the digests that manifest lists; tags: tag -> the manifest digest it was served as. They let a check be
	// made against ONE reference of a pod (the pinned digest, or the tag), not against anything the repository was ever asked for.
	roots   map[string]map[string]struct{}
	tags    map[string]string
	creds   Creds
	created time.Time
	expires time.Time
}

// The bounds of the table (2.4.6a): what one grant may hold and what one node may hold. A node's oldest grant gives way to a new one.
const (
	DefaultGrantMaxDigests = 512
	DefaultGrantsPerNode   = 2000
)

// Why a grant cannot be made. The caller answers the request with an error; nothing is dropped silently.
var (
	ErrTooManyBlobs = errors.New("the manifest lists more digests than a grant holds")
	ErrBadDigest    = errors.New("the manifest lists something that is not a digest")
	digestFormat    = regexp.MustCompile(`^(sha256:[0-9a-f]{64}|sha512:[0-9a-f]{128})$`)
)

// Grants holds the live grants, by node. Soft state: a restart loses it and nothing rebuilds it (2.4.2): the kubelet asks for the manifest
// again, which goes through the whole proof.
type Grants struct {
	// Grace is how long a grant lives after its last use (GATE_PULL_GRACE, default 2 minutes); MaxAge the longest it lives at all
	// (GATE_GRANT_MAX_AGE, default 1 hour).
	Grace  time.Duration
	MaxAge time.Duration
	// MaxDigests is how many digests one grant holds (GATE_GRANT_MAX_DIGESTS, 512); PerNode how many grants one node holds
	// (GATE_GRANTS_PER_NODE, 2000).
	MaxDigests int
	PerNode    int
	Now        func() time.Time

	mu sync.Mutex
	m  map[string]map[grantKey]*grant // node -> grants
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

func (g *Grants) maxDigests() int {
	if g.MaxDigests > 0 {
		return g.MaxDigests
	}
	return DefaultGrantMaxDigests
}

func (g *Grants) perNode() int {
	if g.PerNode > 0 {
		return g.PerNode
	}
	return DefaultGrantsPerNode
}

// Add records a manifest that has just passed the upstream proof (the caller's job, in the same request): root is its digest, tag the tag
// it was asked for by ("" for a digest), children what it lists. It creates the grant of (node, segment, repo) with the credentials that
// proved access or extends it. It refuses, recording nothing, a malformed digest or a manifest that lists more than a grant holds.
func (g *Grants) Add(node, seg, repo string, creds Creds, tag, root string, children []string) error {
	return g.add(node, seg, repo, creds, tag, root, children, true)
}

// Extend renews the grant of (node, segment, repo) with a manifest served on a stale decision (StaleIfError): it never creates a grant,
// and reports whether there was a live one.
func (g *Grants) Extend(node, seg, repo string, creds Creds, tag, root string, children []string) bool {
	return g.add(node, seg, repo, creds, tag, root, children, false) == nil && g.live(node, seg, repo)
}

func (g *Grants) live(node, seg, repo string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	gr := g.m[node][grantKey{seg, repo}]
	return gr != nil && g.now().Before(gr.expires)
}

func (g *Grants) add(node, seg, repo string, creds Creds, tag, root string, children []string, create bool) error {
	if !digestFormat.MatchString(root) {
		return ErrBadDigest
	}
	for _, d := range children {
		if !digestFormat.MatchString(d) {
			return ErrBadDigest
		}
	}
	if len(children)+1 > g.maxDigests() {
		return ErrTooManyBlobs
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
		delete(mine, k)
		gr = nil
	}
	if gr == nil {
		if !create {
			return nil
		}
		if mine == nil {
			mine = map[grantKey]*grant{}
			g.m[node] = mine
		}
		if len(mine) >= g.perNode() {
			g.dropOldestLocked(mine, now)
		}
		gr = &grant{digests: map[string]struct{}{}, roots: map[string]map[string]struct{}{}, tags: map[string]string{}, created: now}
		mine[k] = gr
	}
	fresh := 0
	if _, have := gr.digests[root]; !have {
		fresh++
	}
	for _, d := range children {
		if _, have := gr.digests[d]; !have {
			fresh++
		}
	}
	if len(gr.digests)+fresh > g.maxDigests() {
		// what accumulated for other references of this repository gives way: they must ask for their manifests again
		gr.digests, gr.roots, gr.tags = map[string]struct{}{}, map[string]map[string]struct{}{}, map[string]string{}
	}
	gr.creds = creds
	gr.digests[root] = struct{}{}
	set := gr.roots[root]
	if set == nil {
		set = map[string]struct{}{}
		gr.roots[root] = set
	}
	for _, d := range children {
		gr.digests[d] = struct{}{}
		set[d] = struct{}{}
	}
	if tag != "" {
		gr.tags[tag] = root
	}
	gr.expires = minTime(now.Add(g.grace()), gr.created.Add(g.maxAge()))
	return nil
}

// dropOldestLocked removes the oldest grant of a node (lapsed ones first).
func (g *Grants) dropOldestLocked(mine map[grantKey]*grant, now time.Time) {
	var oldK grantKey
	var oldAt time.Time
	first := true
	for k, gr := range mine {
		if !now.Before(gr.expires) {
			delete(mine, k)
			continue
		}
		if first || gr.created.Before(oldAt) {
			oldK, oldAt, first = k, gr.created, false
		}
	}
	if len(mine) >= g.perNode() && !first {
		delete(mine, oldK)
	}
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

// Covers says whether digest belongs to the image that reference ref (a digest, or a tag) of (node, segment, repo) names: the manifest
// itself, what it lists, and what the manifests it lists list (an index and its children), all of it as served and proven in a live grant.
// A digest granted for another image of the same repository is not covered.
func (g *Grants) Covers(node, seg, repo, ref, digest string) bool {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	gr := g.m[node][grantKey{seg, repo}]
	if gr == nil || !now.Before(gr.expires) {
		return false
	}
	root := ref
	if t, ok := gr.tags[ref]; ok {
		root = t
	}
	if _, ok := gr.digests[root]; !ok {
		return false
	}
	seen := map[string]bool{}
	queue := []string{root}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		if seen[d] {
			continue
		}
		seen[d] = true
		if d == digest {
			return true
		}
		for c := range gr.roots[d] {
			queue = append(queue, c)
		}
	}
	return false
}

// Len is the number of grants held (for the metric).
func (g *Grants) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, m := range g.m {
		n += len(m)
	}
	return n
}

// Sweep drops every lapsed grant of every node; run it now and then in the background (Run), never per request.
func (g *Grants) Sweep() {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	for node, mine := range g.m {
		for k, gr := range mine {
			if !now.Before(gr.expires) {
				delete(mine, k)
			}
		}
		if len(mine) == 0 {
			delete(g.m, node)
		}
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
