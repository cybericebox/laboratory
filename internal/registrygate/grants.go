package registrygate

import (
	"context"
	"errors"
	"fmt"
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
	roots map[string]map[string]struct{}
	tags  map[string]string
	// order lists the roots oldest first: when the grant is full the oldest root gives way, never the whole grant.
	order []string
	// sizes are the descriptor sizes of the digests the manifests listed (the size an upload of that digest may have).
	sizes map[string]int64
	// subs are the digests that an index listed under "manifests": only they are manifests that may be served under the grant and expand it.
	subs    map[string]struct{}
	creds   Creds
	created time.Time
	expires time.Time
	// podSeen is the last time a pod (or ImagePull) of the node named this repository when a request came: the grant outlives its pod by
	// the pull grace only (2.4.3), so a layer download of a device being reset on the same node does not fail in the middle.
	podSeen time.Time
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
	if err := g.check(root, children); err != nil {
		return err
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
		if mine == nil {
			mine = map[grantKey]*grant{}
			g.m[node] = mine
		}
		if len(mine) >= g.perNode() {
			g.dropOldestLocked(mine, now)
		}
		gr = &grant{digests: map[string]struct{}{}, roots: map[string]map[string]struct{}{}, tags: map[string]string{}, sizes: map[string]int64{}, subs: map[string]struct{}{}, created: now, podSeen: now}
		mine[k] = gr
	}
	gr.creds = creds
	g.putRootLocked(gr, tag, root, children)
	gr.expires = minTime(now.Add(g.grace()), gr.created.Add(g.maxAge()))
	return nil
}

// Expand adds what a manifest lists to a root that a live grant already holds (a child manifest served under that grant, whose layers are
// then reachable). It creates nothing, changes no credentials and adds no root, and reports whether it did anything.
func (g *Grants) Expand(node, seg, repo, root string, children []string) bool {
	if g.check(root, children) != nil {
		return false
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	gr := g.m[node][grantKey{seg, repo}]
	if gr == nil || !now.Before(gr.expires) {
		return false
	}
	if _, ok := gr.subs[root]; !ok { // only a manifest that an index of the grant listed
		return false
	}
	g.putRootLocked(gr, "", root, children)
	gr.expires = minTime(now.Add(g.grace()), gr.created.Add(g.maxAge()))
	return true
}

// Renew prolongs a live grant that already holds root, on an answer given from a stale decision (StaleIfError): only the expiry moves.
// No root, child, tag or credential is added or changed, and no grant is created.
func (g *Grants) Renew(node, seg, repo, root string) bool {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	gr := g.m[node][grantKey{seg, repo}]
	if gr == nil || !now.Before(gr.expires) {
		return false
	}
	if _, ok := gr.digests[root]; !ok {
		return false
	}
	gr.expires = minTime(now.Add(g.grace()), gr.created.Add(g.maxAge()))
	return true
}

// NoteIndex records which digests an index (root, or a child served under the grant) listed under "manifests": Expand acts on those only.
func (g *Grants) NoteIndex(node, seg, repo string, subs []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	gr := g.m[node][grantKey{seg, repo}]
	if gr == nil {
		return
	}
	for _, d := range subs {
		if _, ok := gr.digests[d]; ok {
			gr.subs[d] = struct{}{}
		}
	}
}

// NoteSizes records the descriptor sizes of digests of a live grant (what an upload of such a digest may weigh). A size once recorded is
// never raised by a later manifest: the smallest ever declared stands.
func (g *Grants) NoteSizes(node, seg, repo string, sizes map[string]int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	gr := g.m[node][grantKey{seg, repo}]
	if gr == nil {
		return
	}
	for d, n := range sizes {
		if _, ok := gr.digests[d]; !ok || n < 0 {
			continue
		}
		if old, have := gr.sizes[d]; !have || n < old {
			gr.sizes[d] = n
		}
	}
}

// Size is the descriptor size of a digest of a live grant that covers it from ref (the pod's pinned digest or tag).
func (g *Grants) Size(node, seg, repo, ref, digest string) (int64, bool) {
	if !g.Covers(node, seg, repo, ref, digest) {
		return 0, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n, ok := g.m[node][grantKey{seg, repo}].sizes[digest]
	return n, ok
}

func (g *Grants) check(root string, children []string) error {
	if !digestFormat.MatchString(root) {
		return ErrBadDigest
	}
	for _, d := range children {
		if !digestFormat.MatchString(d) {
			return ErrBadDigest
		}
	}
	if len(children)+1 > g.maxDigests() {
		return fmt.Errorf("%w: the manifest lists %d, a grant holds %d (GATE_GRANT_MAX_DIGESTS)", ErrTooManyBlobs, len(children)+1, g.maxDigests())
	}
	return nil
}

// putRootLocked records a root and its children, giving way to the OLDEST roots of the grant (never wiping it) when it would pass the limit,
// so that the other pulls of the repository on this node that are still in flight keep what they were granted.
func (g *Grants) putRootLocked(gr *grant, tag, root string, children []string) {
	if _, have := gr.roots[root]; !have {
		gr.order = append(gr.order, root)
		gr.roots[root] = map[string]struct{}{}
	}
	for _, d := range children {
		gr.roots[root][d] = struct{}{}
	}
	if tag != "" {
		gr.tags[tag] = root
	}
	gr.digests[root] = struct{}{}
	for _, d := range children {
		gr.digests[d] = struct{}{}
	}
	for len(gr.digests) > g.maxDigests() && len(gr.order) > 1 {
		old := gr.order[0]
		gr.order = gr.order[1:]
		delete(gr.roots, old)
		for t, r := range gr.tags {
			if r == old {
				delete(gr.tags, t)
			}
		}
		gr.digests = map[string]struct{}{}
		for _, r := range gr.order {
			gr.digests[r] = struct{}{}
			for d := range gr.roots[r] {
				gr.digests[d] = struct{}{}
			}
		}
		for d := range gr.sizes {
			if _, ok := gr.digests[d]; !ok {
				delete(gr.sizes, d)
			}
		}
		for d := range gr.subs {
			if _, ok := gr.digests[d]; !ok {
				delete(gr.subs, d)
			}
		}
	}
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
// podPresent says whether a pod (or ImagePull) on the node still names the repository; without one the grant is honoured for the pull grace
// after the last time one was seen, then it is not.
func (g *Grants) Has(node, seg, repo, digest string, podPresent bool) (Creds, bool) {
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
	if podPresent {
		gr.podSeen = now
	} else if now.Sub(gr.podSeen) > g.grace() {
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
