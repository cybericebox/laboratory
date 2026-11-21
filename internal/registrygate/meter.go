package registrygate

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// MeterStore is what the meter reads from zot.
type MeterStore interface {
	ManifestBody(ctx context.Context, repo, ref string) ([]byte, error)
	Repos(ctx context.Context) ([]string, error)
	Tags(ctx context.Context, repo string) ([]string, error)
}

// annotationTenant is the annotation a snapshot manifest carries to say whose it is (snapshot.AnnotationTenant). The size annotation is
// not trusted: the sizes come from the layers the manifest lists.
const annotationTenant = "cybericebox.com/tenant"

// ZotMeter is the BlobMeter over zot: the registry bytes of a tenant's snapshots (2.4.4): the config and layer blobs of the "latest"
// manifest of every snapshot repository of the tenant's devices, each blob once, plus those of the repositories of deleted labs that still
// carry the tenant's annotation, minus the blobs the shared base image or the image cache hold too. Measured at most once per TTL.
type ZotMeter struct {
	Store    MeterStore
	Schedule *Schedule
	TTL      time.Duration
	Now      func() time.Time

	mu     sync.Mutex
	shared sharedBlobs
	per    map[string]tenantRepos
}

type sharedBlobs struct {
	at    time.Time
	blobs map[string]struct{}
}

type tenantRepos struct {
	at    time.Time
	repos map[string]map[string]int64 // repository -> blob -> size
}

func (m *ZotMeter) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *ZotMeter) ttl() time.Duration {
	if m.TTL > 0 {
		return m.TTL
	}
	return 30 * time.Second
}

// TenantBlobs implements BlobMeter.
func (m *ZotMeter) TenantBlobs(ctx context.Context, tenant, exceptRepo string) (map[string]int64, error) {
	repos, err := m.tenantRepos(ctx, tenant)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for repo, blobs := range repos {
		if repo == exceptRepo {
			continue
		}
		for d, n := range blobs {
			out[d] = n
		}
	}
	return out, nil
}

func (m *ZotMeter) tenantRepos(ctx context.Context, tenant string) (map[string]map[string]int64, error) {
	now := m.now()
	m.mu.Lock()
	if c, ok := m.per[tenant]; ok && now.Sub(c.at) < m.ttl() {
		m.mu.Unlock()
		return c.repos, nil
	}
	m.mu.Unlock()
	shared, err := m.sharedSet(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	repos := map[string]map[string]int64{}
	add := func(repo string) error {
		body, err := m.Store.ManifestBody(ctx, repo, "latest")
		if err != nil || body == nil {
			return err
		}
		blobs, ok := manifestBlobs(body)
		if !ok {
			return nil
		}
		for d := range blobs {
			if _, held := shared[d]; held {
				delete(blobs, d)
			}
		}
		repos[repo] = blobs
		return nil
	}
	for _, d := range m.Schedule.DevicesOfTenant(tenant) {
		repo := "lab/" + d.Namespace + "/" + d.Lab + "/" + d.Name
		live[repo] = true
		if err := add(repo); err != nil {
			return nil, err
		}
	}
	// the repositories of labs that are gone and wait out their retention: still the tenant's, by the annotation on their manifest
	all, err := m.Store.Repos(ctx)
	if err != nil {
		return nil, err
	}
	for _, repo := range all {
		if !strings.HasPrefix(repo, "lab/") || live[repo] || m.Schedule.knownRepo(repo) {
			continue
		}
		body, err := m.Store.ManifestBody(ctx, repo, "latest")
		if err != nil || body == nil {
			continue
		}
		var mf struct {
			Annotations map[string]string `json:"annotations"`
		}
		if json.Unmarshal(body, &mf) != nil || mf.Annotations[annotationTenant] != tenant {
			continue
		}
		if err := add(repo); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	if m.per == nil {
		m.per = map[string]tenantRepos{}
	}
	m.per[tenant] = tenantRepos{at: now, repos: repos}
	m.mu.Unlock()
	return repos, nil
}

// sharedSet is the set of blobs of the shared base repository and of the image cache (c/**), which no snapshot is charged for.
func (m *ZotMeter) sharedSet(ctx context.Context) (map[string]struct{}, error) {
	now := m.now()
	m.mu.Lock()
	if m.shared.blobs != nil && now.Sub(m.shared.at) < 10*m.ttl() {
		b := m.shared.blobs
		m.mu.Unlock()
		return b, nil
	}
	m.mu.Unlock()
	all, err := m.Store.Repos(ctx)
	if err != nil {
		return nil, err
	}
	set := map[string]struct{}{}
	for _, repo := range all {
		if repo != "base" && !strings.HasPrefix(repo, "c/") {
			continue
		}
		tags, err := m.Store.Tags(ctx, repo)
		if err != nil {
			return nil, err
		}
		for _, tag := range tags {
			if err := m.addManifestBlobs(ctx, repo, tag, set, 0); err != nil {
				return nil, err
			}
		}
	}
	m.mu.Lock()
	m.shared = sharedBlobs{at: now, blobs: set}
	m.mu.Unlock()
	return set, nil
}

func (m *ZotMeter) addManifestBlobs(ctx context.Context, repo, ref string, set map[string]struct{}, depth int) error {
	body, err := m.Store.ManifestBody(ctx, repo, ref)
	if err != nil || body == nil {
		return err
	}
	if blobs, ok := manifestBlobs(body); ok {
		for d := range blobs {
			set[d] = struct{}{}
		}
	}
	if depth > 0 {
		return nil
	}
	var idx struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if json.Unmarshal(body, &idx) == nil {
		for _, c := range idx.Manifests {
			if err := m.addManifestBlobs(ctx, repo, c.Digest, set, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
