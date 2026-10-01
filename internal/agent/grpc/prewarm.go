package grpc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/snapshot"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// PrewarmConfig makes the agent able to fill the image cache ahead of time.
type PrewarmConfig struct {
	// Enabled: the platform image cache exists. Off: PrewarmImages fails with FailedPrecondition.
	Enabled bool
	// RegistryAddr is host:port of the cache (zot) as the agent reaches it.
	RegistryAddr string
	// Registries are the upstream registries the cache serves; images of any other
	// registry are SKIPPED (nodes pull them directly).
	Registries []string
	// Resolver turns a tag into the digest the operator will pin labs to (the
	// platform manifest of a single-architecture cluster, the index otherwise).
	Resolver imagecache.Resolver
	// Concurrency is how many images are fetched at once (4).
	Concurrency int
	// Timeout bounds one image (10m).
	Timeout time.Duration
	// RetryAfter is how long a FAILED image stays failed before a repeated call
	// tries it again, so that a poller sees the failure (30s).
	RetryAfter time.Duration
	// StaleAfter is the age after which a DONE image is checked again when asked
	// for; the check is a manifest request, which also counts as a pull for the
	// cache's unused-image retention (30m).
	StaleAfter time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (c PrewarmConfig) withDefaults() PrewarmConfig {
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Minute
	}
	if c.RetryAfter <= 0 {
		c.RetryAfter = 30 * time.Second
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 30 * time.Minute
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

type prewarmEntry struct {
	state   protobuf.PrewarmState
	err     string
	digest  string
	updated time.Time
}

type prewarmer struct {
	cfg PrewarmConfig
	sem chan struct{}

	mu      sync.Mutex
	entries map[string]*prewarmEntry
	order   []string
}

// SetPrewarm configures cache prewarming; call it before serving.
func (h *Handler) SetPrewarm(cfg PrewarmConfig) {
	cfg = cfg.withDefaults()
	h.prewarm = &prewarmer{cfg: cfg, sem: make(chan struct{}, cfg.Concurrency), entries: map[string]*prewarmEntry{}}
}

// PrewarmImages starts (or reports on) the cache prewarm of images. It returns
// at once with the current state of every requested image; repeat the call to poll.
func (h *Handler) PrewarmImages(_ context.Context, in *protobuf.PrewarmImagesRequest) (*protobuf.PrewarmImagesResult, error) {
	p := h.prewarm
	if p == nil || !p.cfg.Enabled {
		return nil, status.Error(codes.FailedPrecondition, "the image cache is not enabled: there is nothing to prewarm")
	}
	return p.request(in.GetImages()), nil
}

func (p *prewarmer) request(images []string) *protobuf.PrewarmImagesResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(images) == 0 {
		images = append([]string(nil), p.order...)
	}
	seen := map[string]bool{}
	out := &protobuf.PrewarmImagesResult{}
	for _, img := range images {
		img = strings.TrimSpace(img)
		if img == "" || seen[img] {
			continue
		}
		seen[img] = true
		e := p.entries[img]
		if e == nil {
			e = &prewarmEntry{}
			p.entries[img] = e
			p.order = append(p.order, img)
			p.startLocked(img, e)
		} else if p.retryLocked(e) {
			p.startLocked(img, e)
		}
		out.Images = append(out.Images, p.statusLocked(img, e))
	}
	return out
}

// retryLocked: a failed image is tried again after RetryAfter; a done one is checked again once stale.
func (p *prewarmer) retryLocked(e *prewarmEntry) bool {
	switch e.state {
	case protobuf.PrewarmState_PREWARM_STATE_FAILED:
		return p.cfg.Now().Sub(e.updated) >= p.cfg.RetryAfter
	case protobuf.PrewarmState_PREWARM_STATE_DONE:
		return p.cfg.Now().Sub(e.updated) >= p.cfg.StaleAfter
	}
	return false
}

func (p *prewarmer) statusLocked(img string, e *prewarmEntry) *protobuf.PrewarmImageStatus {
	return &protobuf.PrewarmImageStatus{Image: img, State: e.state, Error: e.err, Digest: e.digest, UpdatedUnixMs: e.updated.UnixMilli()}
}

func (p *prewarmer) setLocked(e *prewarmEntry, state protobuf.PrewarmState, digest, errMsg string) {
	e.state, e.err, e.updated = state, errMsg, p.cfg.Now()
	if digest != "" {
		e.digest = digest
	}
}

// startLocked decides at once what can be decided (skipped, invalid) and
// otherwise queues the fetch behind the concurrency bound.
func (p *prewarmer) startLocked(img string, e *prewarmEntry) {
	repo, ok, err := p.cachePath(img)
	switch {
	case err != nil:
		p.setLocked(e, protobuf.PrewarmState_PREWARM_STATE_FAILED, "", err.Error())
		return
	case !ok:
		p.setLocked(e, protobuf.PrewarmState_PREWARM_STATE_SKIPPED, "", "the registry of this image is not served by the cache; nodes pull it directly")
		return
	}
	p.setLocked(e, protobuf.PrewarmState_PREWARM_STATE_QUEUED, "", "")
	go p.warm(img, repo, e)
}

// cachePath returns the repository path ("REG/repo") the cache keeps the image under,
// and whether the cache serves its registry at all.
func (p *prewarmer) cachePath(img string) (string, bool, error) {
	if _, err := name.ParseReference(img); err != nil {
		return "", false, fmt.Errorf("invalid image reference: %v", err)
	}
	rw := imagecache.Rewriter{Prefix: "cache", Registries: p.cfg.Registries}
	out := rw.Rewrite(img)
	if out == img {
		return "", false, nil
	}
	return rw.RepoOf(out), true, nil
}

func (p *prewarmer) warm(img, repo string, e *prewarmEntry) {
	p.sem <- struct{}{}
	defer func() { <-p.sem }()
	p.mu.Lock()
	p.setLocked(e, protobuf.PrewarmState_PREWARM_STATE_WARMING, "", "")
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.Timeout)
	defer cancel()
	digest, err := p.fetch(ctx, img, repo)

	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.setLocked(e, protobuf.PrewarmState_PREWARM_STATE_FAILED, digest, err.Error())
		return
	}
	p.setLocked(e, protobuf.PrewarmState_PREWARM_STATE_DONE, digest, "")
}

// fetch makes the cache hold the image: it asks the cache for the manifest by the
// digest the operator will pin labs to (zot syncs the image from upstream before it
// answers), then checks that every blob really is in the cache's storage.
func (p *prewarmer) fetch(ctx context.Context, img, repo string) (string, error) {
	var digest string
	if p.cfg.Resolver != nil {
		d, err := p.cfg.Resolver.Resolve(ctx, img)
		if err != nil {
			return "", fmt.Errorf("resolve %s upstream: %w", img, err)
		}
		digest = d
	} else if digest = imagecache.DigestOf(img); digest == "" {
		return "", fmt.Errorf("no resolver: cannot resolve the tag of %s", img)
	}
	reg := &snapshot.Registry{Host: p.cfg.RegistryAddr}
	ref, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", p.cfg.RegistryAddr, repo, digest), name.Insecure)
	if err != nil {
		return digest, err
	}
	desc, err := remote.Get(ref, remote.WithContext(ctx))
	if err != nil {
		return digest, fmt.Errorf("the cache could not fetch %s: %w", ref.Name(), err)
	}
	if err := verifyStored(ctx, reg, ref.Context(), repo, desc); err != nil {
		return digest, err
	}
	return digest, nil
}

// verifyStored walks an index or image manifest and checks that the config and
// every layer blob is stored in the cache, not only the manifest.
func verifyStored(ctx context.Context, reg *snapshot.Registry, repoName name.Repository, repo string, desc *remote.Descriptor) error {
	switch {
	case desc.MediaType.IsIndex():
		idx, err := desc.ImageIndex()
		if err != nil {
			return err
		}
		m, err := idx.IndexManifest()
		if err != nil {
			return err
		}
		for _, child := range m.Manifests {
			if !child.MediaType.IsImage() {
				continue
			}
			cd, err := remote.Get(repoName.Digest(child.Digest.String()), remote.WithContext(ctx))
			if err != nil {
				return fmt.Errorf("manifest %s of the index is not in the cache: %w", child.Digest, err)
			}
			if err := verifyStored(ctx, reg, repoName, repo, cd); err != nil {
				return err
			}
		}
		return nil
	case desc.MediaType.IsImage():
		img, err := desc.Image()
		if err != nil {
			return err
		}
		m, err := img.Manifest()
		if err != nil {
			return err
		}
		blobs := append([]v1.Descriptor{m.Config}, m.Layers...)
		for _, b := range blobs {
			if !reg.HasBlobIn(ctx, repo, b.Digest) {
				return fmt.Errorf("blob %s of %s is not stored in the cache", b.Digest, desc.Digest)
			}
		}
		return nil
	}
	return fmt.Errorf("unsupported manifest type %s", desc.MediaType)
}
