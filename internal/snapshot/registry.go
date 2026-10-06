package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Latest is the tag every snapshot of a device is pushed under; the status
// records the digest, so older manifests are simply untagged and left to the
// registry's garbage collection.
const Latest = "latest"

// Registry is a client of the snapshot registry (zot) with write access.
type Registry struct {
	// Host is host:port of the registry, e.g. "localhost:5035". Hosts on the
	// loopback or a private network are spoken to over plain HTTP.
	Host string
	Auth authn.Authenticator
	// Transport overrides the HTTP transport (tests).
	Transport http.RoundTripper
}

func (r *Registry) opts(ctx context.Context) []remote.Option {
	o := []remote.Option{remote.WithContext(ctx)}
	if r.Auth != nil {
		o = append(o, remote.WithAuth(r.Auth))
	}
	if r.Transport != nil {
		o = append(o, remote.WithTransport(r.Transport))
	}
	return o
}

func (r *Registry) repo(repo string) (name.Repository, error) {
	return name.NewRepository(r.Host+"/"+repo, name.Insecure)
}

// Ref is the pullable reference of a pushed manifest.
func (r *Registry) Ref(repo string, digest v1.Hash) string {
	return fmt.Sprintf("%s/%s@%s", r.Host, repo, digest)
}

// Push makes img available as repo:latest. The base layers of img (the first
// baseLayers) are placed in the shared base repository once and mounted into
// repo, so a device's first snapshot does not upload the whole base image.
// sourceRepo, when set, is a repository that already holds the base layers (the
// image cache's repository of the base image): they are mounted from it, so
// nothing is uploaded at all. It returns the pullable reference by digest.
func (r *Registry) Push(ctx context.Context, repo string, img v1.Image, baseLayers int, sourceRepo string) (string, v1.Hash, error) {
	target, err := r.repo(repo)
	if err != nil {
		return "", v1.Hash{}, err
	}
	layers, err := img.Layers()
	if err != nil {
		return "", v1.Hash{}, err
	}
	if baseLayers > len(layers) {
		baseLayers = len(layers)
	}
	for _, l := range layers[:baseLayers] {
		if err := r.ensureBaseLayer(ctx, target, l, sourceRepo); err != nil {
			return "", v1.Hash{}, err
		}
	}
	tag := target.Tag(Latest)
	if err := remote.Write(tag, img, r.opts(ctx)...); err != nil {
		return "", v1.Hash{}, fmt.Errorf("push %s: %w", tag, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return "", v1.Hash{}, err
	}
	return r.Ref(repo, digest), digest, nil
}

// ensureBaseLayer guarantees that target holds layer l: already there, mounted
// from the shared base repository, or (registries without mount support)
// uploaded.
func (r *Registry) ensureBaseLayer(ctx context.Context, target name.Repository, l v1.Layer, sourceRepo string) error {
	d, err := l.Digest()
	if err != nil {
		return err
	}
	if r.HasBlob(ctx, target, d) {
		return nil
	}
	if sourceRepo != "" {
		if src, err := r.repo(sourceRepo); err == nil && r.HasBlob(ctx, src, d) {
			if ok, err := r.mountBlob(ctx, target, src, d); err == nil && ok {
				return nil
			}
		}
	}
	baseRepo, err := r.repo(BaseRepo)
	if err != nil {
		return err
	}
	if !r.HasBlob(ctx, baseRepo, d) {
		if err := remote.WriteLayer(baseRepo, l, r.opts(ctx)...); err != nil {
			return fmt.Errorf("upload base layer %s: %w", d, err)
		}
	}
	if ok, err := r.mountBlob(ctx, target, baseRepo, d); err == nil && ok {
		return nil
	}
	if err := remote.WriteLayer(target, l, r.opts(ctx)...); err != nil {
		return fmt.Errorf("upload layer %s: %w", d, err)
	}
	return nil
}

func (r *Registry) HasBlob(ctx context.Context, repo name.Repository, d v1.Hash) bool {
	resp, err := r.blobRequest(ctx, http.MethodHead, repo, nil, fmt.Sprintf("blobs/%s", d), nil)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// mountBlob asks the registry to link a blob of from into target without
// transferring it. It reports false when the registry declined and opened an
// upload session instead.
func (r *Registry) mountBlob(ctx context.Context, target, from name.Repository, d v1.Hash) (bool, error) {
	q := url.Values{"mount": {d.String()}, "from": {from.RepositoryStr()}}
	resp, err := r.blobRequest(ctx, http.MethodPost, target, &from, "blobs/uploads/", q)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusCreated, nil
}

// blobRequest sends one request to /v2/<repo>/<path>, authenticated with pull
// scope on repo (push scope for POST) and pull scope on the optional other repo.
func (r *Registry) blobRequest(ctx context.Context, method string, repo name.Repository, other *name.Repository, path string, q url.Values) (*http.Response, error) {
	scope := repo.Scope(transport.PullScope)
	if method == http.MethodPost || method == http.MethodDelete {
		scope = repo.Scope(transport.PushScope)
	}
	scopes := []string{scope}
	if other != nil {
		scopes = append(scopes, other.Scope(transport.PullScope))
	}
	auth := r.Auth
	if auth == nil {
		auth = authn.Anonymous
	}
	rt := r.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	tr, err := transport.NewWithContext(ctx, repo.Registry, auth, rt, scopes)
	if err != nil {
		return nil, err
	}
	u := url.URL{
		Scheme:   repo.Scheme(),
		Host:     repo.RegistryStr(),
		Path:     fmt.Sprintf("/v2/%s/%s", repo.RepositoryStr(), path),
		RawQuery: q.Encode(),
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	return (&http.Client{Transport: tr}).Do(req)
}

// Repos lists the repositories of the registry.
func (r *Registry) Repos(ctx context.Context) ([]string, error) {
	reg, err := name.NewRegistry(r.Host, name.Insecure)
	if err != nil {
		return nil, err
	}
	repos, err := remote.Catalog(ctx, reg, r.opts(ctx)[1:]...)
	if err != nil {
		return nil, err
	}
	sort.Strings(repos)
	return repos, nil
}

// DeleteRepo deletes every tagged manifest of a repository; the registry's
// garbage collection then frees the blobs and the untagged manifests.
func (r *Registry) DeleteRepo(ctx context.Context, repo string) error {
	target, err := r.repo(repo)
	if err != nil {
		return err
	}
	tags, err := remote.List(target, r.opts(ctx)...)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	for _, tag := range tags {
		desc, err := remote.Head(target.Tag(tag), r.opts(ctx)...)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return err
		}
		if err := remote.Delete(target.Digest(desc.Digest.String()), r.opts(ctx)...); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete %s@%s: %w", repo, desc.Digest, err)
		}
	}
	return nil
}

func isNotFound(err error) bool {
	if terr, ok := err.(*transport.Error); ok {
		return terr.StatusCode == http.StatusNotFound
	}
	return strings.Contains(err.Error(), "NAME_UNKNOWN") || strings.Contains(err.Error(), "MANIFEST_UNKNOWN")
}

// Image reads the manifest of repo:latest back (for tests and diagnostics).
func (r *Registry) Image(ctx context.Context, repo string) (v1.Image, error) {
	target, err := r.repo(repo)
	if err != nil {
		return nil, err
	}
	return remote.Image(target.Tag(Latest), r.opts(ctx)...)
}

// HasBlobIn reports whether the blob is stored in the repository named by its path.
func (r *Registry) HasBlobIn(ctx context.Context, repo string, d v1.Hash) bool {
	target, err := r.repo(repo)
	if err != nil {
		return false
	}
	return r.HasBlob(ctx, target, d)
}

// The annotations of a snapshot manifest: whose it is and how much state it holds (the uncompressed bytes the quota counts).
// They let the registry itself say what a repository of a lab that is gone still takes from whom.
const (
	AnnotationTenant     = "cybericebox.com/tenant"
	AnnotationStateBytes = "cybericebox.com/state-bytes"
)

// Annotated is img with the tenant and state size annotated on its manifest.
func Annotated(img v1.Image, tenant string, stateBytes int64) v1.Image {
	return mutate.Annotations(img, map[string]string{AnnotationTenant: tenant, AnnotationStateBytes: strconv.FormatInt(stateBytes, 10)}).(v1.Image)
}

// Supersede removes what a new snapshot has replaced: the old manifest of the repository, and the blobs only it used, at once
// instead of after the registry's garbage collection (which keeps unreferenced blobs for hours). oldRef is the pullable reference
// ("host/repo@sha256:...") of the manifest that was current before keep. A missing manifest or blob is fine, and so is a registry that
// does not allow deleting blobs: the garbage collection then does it.
func (r *Registry) Supersede(ctx context.Context, repo, oldRef string, keep v1.Image) error {
	_, digestStr, ok := strings.Cut(oldRef, "@")
	if !ok {
		return nil // the base image: it is not ours to delete
	}
	old, err := v1.NewHash(digestStr)
	if err != nil {
		return nil
	}
	if kd, err := keep.Digest(); err == nil && kd == old {
		return nil
	}
	target, err := r.repo(repo)
	if err != nil {
		return err
	}
	img, err := remote.Image(target.Digest(old.String()), r.opts(ctx)...)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	m, err := img.Manifest()
	if err != nil {
		return err
	}
	kept := map[v1.Hash]bool{}
	if layers, err := keep.Layers(); err == nil {
		for _, l := range layers {
			if d, err := l.Digest(); err == nil {
				kept[d] = true
			}
		}
	}
	if cfg, err := keep.ConfigName(); err == nil {
		kept[cfg] = true
	}
	if err := remote.Delete(target.Digest(old.String()), r.opts(ctx)...); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete superseded manifest %s: %w", old, err)
	}
	blobs := []v1.Hash{m.Config.Digest}
	for _, l := range m.Layers {
		blobs = append(blobs, l.Digest)
	}
	for _, d := range blobs {
		if kept[d] {
			continue
		}
		resp, err := r.blobRequest(ctx, http.MethodDelete, target, nil, "blobs/"+d.String(), nil)
		if err != nil {
			return fmt.Errorf("delete superseded blob %s: %w", d, err)
		}
		_ = resp.Body.Close() // 202 deleted; 404 gone; 405 the registry does not delete blobs: its garbage collection will
	}
	return nil
}

// ErrRegistryFull is the refusal of a push while the registry has too little room left.
var ErrRegistryFull = errors.New("the snapshot registry is nearly full")

// Usage is what the registry stores for the images it has a name for: every blob once, however many repositories mount it, summed
// over every tag of every repository (the snapshots, the shared base, the image cache).
func (r *Registry) Usage(ctx context.Context) (int64, error) {
	repos, err := r.Repos(ctx)
	if err != nil {
		return 0, err
	}
	seen := map[v1.Hash]int64{}
	add := func(img v1.Image) error {
		m, err := img.Manifest()
		if err != nil {
			return err
		}
		seen[m.Config.Digest] = m.Config.Size
		for _, l := range m.Layers {
			seen[l.Digest] = l.Size
		}
		return nil
	}
	for _, repo := range repos {
		target, err := r.repo(repo)
		if err != nil {
			continue
		}
		tags, err := remote.List(target, r.opts(ctx)...)
		if err != nil {
			continue // a repository that vanished or is empty
		}
		for _, tag := range tags {
			desc, err := remote.Get(target.Tag(tag), r.opts(ctx)...)
			if err != nil {
				continue
			}
			if desc.MediaType.IsIndex() {
				idx, err := desc.ImageIndex()
				if err != nil {
					continue
				}
				im, err := idx.IndexManifest()
				if err != nil {
					continue
				}
				for _, child := range im.Manifests {
					if img, err := idx.Image(child.Digest); err == nil {
						_ = add(img)
					}
				}
				continue
			}
			if img, err := desc.Image(); err == nil {
				_ = add(img)
			}
		}
	}
	var total int64
	for _, n := range seen {
		total += n
	}
	return total, nil
}

// RetainedBytes is the state a tenant still has in the repositories no live device owns (live is the set of repository names of
// the existing devices): the snapshots of labs that are gone and wait out their retention. It reads the annotations of each
// repository's latest manifest, so a repository pushed before they existed counts for nothing.
func (r *Registry) RetainedBytes(ctx context.Context, tenant string, live map[string]bool) (int64, error) {
	repos, err := r.Repos(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, repo := range repos {
		if !strings.HasPrefix(repo, RepoPrefix+"/") || live[repo] {
			continue
		}
		target, err := r.repo(repo)
		if err != nil {
			continue
		}
		desc, err := remote.Get(target.Tag(Latest), r.opts(ctx)...)
		if err != nil {
			continue
		}
		var m v1.Manifest
		if err := json.Unmarshal(desc.Manifest, &m); err != nil || m.Annotations[AnnotationTenant] != tenant {
			continue
		}
		if n, err := strconv.ParseInt(m.Annotations[AnnotationStateBytes], 10, 64); err == nil && n > 0 {
			total += n
		}
	}
	return total, nil
}

// Capacity says whether the registry has room for a push: its volume is Total bytes, and a push is refused once what is stored plus
// what the push adds would take more than all but ReserveFraction of it. Usage is measured at most once per TTL (it reads every
// manifest).
type Capacity struct {
	Registry        *Registry
	Total           int64
	ReserveFraction float64
	TTL             time.Duration

	mu   sync.Mutex
	used int64
	at   time.Time
	now  func() time.Time
}

// Check returns ErrRegistryFull (wrapped with the numbers) when adding bytes would pass the line. With no Total it never refuses.
func (c *Capacity) Check(ctx context.Context, bytes int64) error {
	if c == nil || c.Total <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = time.Minute
	}
	if c.at.IsZero() || now().Sub(c.at) > ttl {
		used, err := c.Registry.Usage(ctx)
		if err != nil {
			return fmt.Errorf("measure the registry: %w", err)
		}
		c.used, c.at = used, now()
	}
	limit := int64(float64(c.Total) * (1 - c.ReserveFraction))
	if c.used+bytes > limit {
		return fmt.Errorf("%w: it holds %d of %d bytes and keeps %.0f%% free, this snapshot adds up to %d", ErrRegistryFull, c.used, c.Total, c.ReserveFraction*100, bytes)
	}
	// What this push adds is counted until the next measurement.
	c.used += bytes
	return nil
}

// Repo and Options expose the repository name and request options to tests of other packages.
func (r *Registry) Repo(repo string) (name.Repository, error)   { return r.repo(repo) }
func (r *Registry) Options(ctx context.Context) []remote.Option { return r.opts(ctx) }
