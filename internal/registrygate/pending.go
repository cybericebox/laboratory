package registrygate

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
)

// zot refuses a manifest whose blobs (or, for an index, whose child manifests) it does not hold, and a pull fetches the manifest first.
// So a manifest the gate has fetched waits here, in memory, and is stored when the last thing it lists is stored; until then it is
// answered from here (it is immutable, addressed by its digest, and the request has passed the proof). An index of several platforms is
// never complete (only one platform's children are ever pulled), so it stays here only, as long as the memory allows: soft state, a
// restart costs one fetch with the pull's credentials.
const (
	maxPending      = 2048
	maxPendingBytes = 64 << 20
)

type dep struct{ kind, digest string }

type pendingManifest struct {
	repo, digest, ct string
	body             []byte
	missing          map[string]struct{} // digests not yet in zot
	index            bool
	seq              uint64
}

type pendingSet struct {
	mu    sync.Mutex
	m     map[string]*pendingManifest // repo|digest
	bytes int
	seq   uint64
}

func pendKey(repo, digest string) string { return repo + "|" + digest }

// manifestDeps lists what zot must already hold for the manifest to be accepted, and whether it is an index.
func manifestDeps(body []byte) (deps []dep, index bool) {
	var m struct {
		Config struct{ Digest string }   `json:"config"`
		Layers []struct{ Digest string } `json:"layers"`
		// the blobs of an artifact manifest
		Blobs     []struct{ Digest string } `json:"blobs"`
		Manifests []struct{ Digest string } `json:"manifests"`
	}
	if json.Unmarshal(body, &m) != nil {
		return nil, false
	}
	if reDigest.MatchString(m.Config.Digest) {
		deps = append(deps, dep{kindBlobs, m.Config.Digest})
	}
	for _, l := range m.Layers {
		if reDigest.MatchString(l.Digest) {
			deps = append(deps, dep{kindBlobs, l.Digest})
		}
	}
	for _, b := range m.Blobs {
		if reDigest.MatchString(b.Digest) {
			deps = append(deps, dep{kindBlobs, b.Digest})
		}
	}
	for _, c := range m.Manifests {
		if reDigest.MatchString(c.Digest) {
			deps = append(deps, dep{kindManifests, c.Digest})
		}
	}
	return deps, len(m.Manifests) > 0
}

func (p *pendingSet) get(repo, digest string) (*pendingManifest, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pm, ok := p.m[pendKey(repo, digest)]
	return pm, ok
}

func (p *pendingSet) add(pm *pendingManifest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]*pendingManifest{}
	}
	p.seq++
	pm.seq = p.seq
	if old, ok := p.m[pendKey(pm.repo, pm.digest)]; ok {
		p.bytes -= len(old.body)
	}
	p.m[pendKey(pm.repo, pm.digest)] = pm
	p.bytes += len(pm.body)
	for (len(p.m) > maxPending || p.bytes > maxPendingBytes) && len(p.m) > 1 {
		var oldest *pendingManifest
		for _, c := range p.m {
			if c != pm && (oldest == nil || c.seq < oldest.seq) {
				oldest = c
			}
		}
		delete(p.m, pendKey(oldest.repo, oldest.digest))
		p.bytes -= len(oldest.body)
	}
}

func (p *pendingSet) remove(pm *pendingManifest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur, ok := p.m[pendKey(pm.repo, pm.digest)]; ok && cur == pm {
		delete(p.m, pendKey(pm.repo, pm.digest))
		p.bytes -= len(pm.body)
	}
}

// satisfied marks digest as stored in repo and returns the pending manifests of that repository that are now complete.
func (p *pendingSet) satisfied(repo, digest string) []*pendingManifest {
	p.mu.Lock()
	defer p.mu.Unlock()
	var done []*pendingManifest
	for _, pm := range p.m {
		if pm.repo != repo || pm.index {
			continue
		}
		delete(pm.missing, digest)
		if len(pm.missing) == 0 {
			done = append(done, pm)
		}
	}
	return done
}

// remember takes a manifest the gate fetched with a proof: stored in zot at once when zot already holds everything it lists, else kept
// here until then.
func (c *Cache) remember(ctx context.Context, repo, digest, ct string, body []byte) {
	deps, index := manifestDeps(body)
	pm := &pendingManifest{repo: repo, digest: digest, ct: ct, body: body, index: index, missing: map[string]struct{}{}}
	if !index {
		for _, d := range deps {
			if _, ok, err := c.Zot.Head(ctx, repo, d.kind, d.digest); err != nil || !ok {
				pm.missing[d.digest] = struct{}{}
			}
		}
		if len(pm.missing) == 0 {
			c.storeNow(ctx, pm)
			return
		}
	}
	c.pending.add(pm)
}

func (c *Cache) storeNow(ctx context.Context, pm *pendingManifest) {
	if c.DiskFull() {
		c.M.fills.WithLabelValues("manifest", "bypass_disk_full").Inc()
		return
	}
	// by digest, and under a tag of its own: zot's retention works on tags (pulledWithin keeps what is pulled)
	if err := c.Zot.PutManifest(ctx, pm.repo, pm.digest, pm.ct, pm.body); err != nil {
		c.M.fills.WithLabelValues("manifest", "store_failed").Inc()
		return
	}
	if err := c.Zot.PutManifest(ctx, pm.repo, "sha256-"+strings.TrimPrefix(pm.digest, "sha256:"), pm.ct, pm.body); err != nil {
		c.M.fills.WithLabelValues("manifest", "store_failed").Inc()
		return
	}
	c.M.fills.WithLabelValues("manifest", "stored").Inc()
	c.pending.remove(pm)
	c.holds(pm.repo, pm.digest)
}

// stored tells the pending manifests that a blob is now in zot, and stores the ones that are complete.
func (c *Cache) stored(ctx context.Context, repo, digest string) {
	for _, pm := range c.pending.satisfied(repo, digest) {
		c.storeNow(ctx, pm)
	}
}
