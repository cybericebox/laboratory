package grpc

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// PrewarmConfig makes the agent able to fill the image cache ahead of time.
type PrewarmConfig struct {
	// Enabled: the platform image cache exists. Off: PrewarmImages fails with FailedPrecondition.
	Enabled bool
	// RegistryAddr is host:port of the cache as the agent reaches it: the registry gate, whose cache path /v2/c/<tenant>/<registry>/<repo>
	// proves every pull against the upstream registry with the credentials the call carries.
	RegistryAddr string
	// Registries are the upstream registries the cache serves; an image of any other
	// registry is FAILED: with the cache on there is no direct pull.
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
	// CacheTLS, when set, makes the agent speak TLS to RegistryAddr (the gate's TLS port, where the cache path is served) trusting what it
	// holds; nil means plain http (a development setup, and the tests). The credentials of a call travel there, so a real setup has it.
	CacheTLS *tls.Config
	// Token is the agent's own projected ServiceAccount token, which every request to the gate carries in X-Cib-Node-Token: the gate
	// knows the caller as the agent and allows the cache path without a pod on a node. Nil: no token (the tests).
	Token *imagecache.TokenFile
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
const (
	// MaxPrewarmImages is the most images one PrewarmImages call may name, and maxPrewarmEntries the most the agent keeps track of
	// (a goroutine starts per distinct image): past the second the oldest finished one is forgotten, and when every one is still
	// running the new image is refused.
	MaxPrewarmImages  = 200
	maxPrewarmEntries = 5000
)

func (h *Handler) SetPrewarm(cfg PrewarmConfig) {
	cfg = cfg.withDefaults()
	h.prewarm = &prewarmer{cfg: cfg, sem: make(chan struct{}, cfg.Concurrency), entries: map[string]*prewarmEntry{}}
}

// PrewarmImages starts (or reports on) the cache prewarm of images. It returns
// at once with the current state of every requested image; repeat the call to poll.
//
// The cache is shared, so the caller's tenant policy applies: an image the tenant may not
// use is reported FAILED and never fetched, and the list of every warmed image (an empty
// request) shows only the images the tenant may use and that were warmed without credentials.
//
// An image whose registry matches a credential of the request is warmed through the registry gate
// with those credentials (Authorization: Basic, accepted from the agent only); the tenant is a segment of the cache path, the one a
// pod of that tenant uses, taken from the caller's certificate. The gate proves the access
// against the registry with them, exactly as it does for a pod, and only then fetches and stores.
// The credentials are the call's: no platform or operator credential ever reaches the upstream
// registry. The state of such an image is kept for the credentials that asked (a hash of the
// registry and the credentials is part of its key), never listed and never shown to a call that
// brings other credentials; only the hash stays in memory. An image that needs credentials and is
// named without them is FAILED by the gate's answer.
func (h *Handler) PrewarmImages(ctx context.Context, in *protobuf.PrewarmImagesRequest) (*protobuf.PrewarmImagesResult, error) {
	p := h.prewarm
	if p == nil || !p.cfg.Enabled {
		return nil, status.Error(codes.FailedPrecondition, "the image cache is not enabled: there is nothing to prewarm")
	}
	policy, err := h.imagePolicy(ctx)
	if err != nil {
		return nil, err
	}
	if n := len(in.GetImages()); n > MaxPrewarmImages {
		return nil, status.Errorf(codes.InvalidArgument, "at most %d images in one call, got %d", MaxPrewarmImages, n)
	}
	if err := validateRegistryCredentials(in.GetRegistryCredentials()); err != nil {
		return nil, err
	}
	refuse := func(img string) (protobuf.PrewarmState, string) {
		if err := policy.Check(img); err != nil {
			return protobuf.PrewarmState_PREWARM_STATE_FAILED, err.Error()
		}
		return protobuf.PrewarmState_PREWARM_STATE_UNSPECIFIED, ""
	}
	tenant := tenantOf(ctx)
	auths := credentialedImages(in.GetImages(), in.GetRegistryCredentials())
	if len(auths) == 0 {
		return p.request(tenant, in.GetImages(), refuse, nil), nil
	}
	byImage := map[string]*protobuf.PrewarmImageStatus{}
	var plain []string
	for _, img := range in.GetImages() {
		if _, ok := auths[strings.TrimSpace(img)]; !ok {
			plain = append(plain, img)
		}
	}
	if len(plain) > 0 {
		for _, st := range p.request(tenant, plain, refuse, nil).Images {
			byImage[st.Image] = st
		}
	}
	for img, a := range auths {
		for _, st := range p.request(tenant, []string{img}, refuse, a).Images {
			byImage[st.Image] = st
		}
	}
	out := &protobuf.PrewarmImagesResult{}
	for _, img := range in.GetImages() { // the request's order
		img = strings.TrimSpace(img)
		if st := byImage[img]; st != nil {
			out.Images = append(out.Images, st)
			delete(byImage, img)
		}
	}
	return out, nil
}

// callAuth is the credential a call brings for the registry of one image, as the gate wants it.
type callAuth struct {
	// user and pass are the call's own login at the registry of the image.
	user, pass string
	// id tells the credentials apart in the table of entries: a hash of the registry, the user and the secret.
	id string
}

// credentialedImages maps each distinct requested image whose registry has a credential in the request to the credential the gate
// is given for it. The keychain it builds from the request is the same one the lab Secret is made from.
func credentialedImages(images []string, creds []*protobuf.RegistryCredential) map[string]*callAuth {
	if len(creds) == 0 {
		return nil
	}
	doc, err := dockerConfigJSON(creds)
	if err != nil {
		return nil
	}
	kc, err := imagecache.NewDockerConfigKeychain(doc)
	if err != nil {
		return nil
	}
	out := map[string]*callAuth{}
	for _, img := range images {
		img = strings.TrimSpace(img)
		if _, seen := out[img]; img == "" || seen {
			continue
		}
		ref, err := name.ParseReference(img)
		if err != nil {
			continue // reported by the normal path
		}
		reg := ref.Context().RegistryStr()
		if !kc.Has(reg) {
			continue
		}
		a, err := kc.Resolve(ref.Context())
		if err != nil {
			continue
		}
		ac, err := a.Authorization()
		if err != nil || ac.Username == "" {
			continue // a login without a name cannot be sent
		}
		sum := sha256.Sum256([]byte(reg + "\x00" + ac.Username + "\x00" + ac.Password))
		out[img] = &callAuth{user: ac.Username, pass: ac.Password, id: hex.EncodeToString(sum[:8])}
	}
	return out
}

// request starts (or reports on) the fetch of images. refuse, when not nil, may answer a
// state and a message for an image that must not be fetched for the caller.
func (p *prewarmer) request(tenant string, images []string, refuse func(string) (protobuf.PrewarmState, string), auth *callAuth) *protobuf.PrewarmImagesResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	listing := len(images) == 0
	if listing {
		// only what was warmed for this tenant without credentials: what was warmed with someone's is theirs to ask about
		for _, k := range p.order {
			if img, ok := strings.CutPrefix(k, tenant+"\x01"); ok && !strings.Contains(img, "\x00") {
				images = append(images, img)
			}
		}
	}
	seen := map[string]bool{}
	out := &protobuf.PrewarmImagesResult{}
	for _, img := range images {
		img = strings.TrimSpace(img)
		if img == "" || seen[img] {
			continue
		}
		seen[img] = true
		if refuse != nil {
			if st, msg := refuse(img); st != protobuf.PrewarmState_PREWARM_STATE_UNSPECIFIED {
				if !listing { // asked by name: say why; a listing just leaves it out
					out.Images = append(out.Images, &protobuf.PrewarmImageStatus{Image: img, State: st, Error: msg, UpdatedUnixMs: p.cfg.Now().UnixMilli()})
				}
				continue
			}
		}
		key := tenant + "\x01" + img // the cache path carries the tenant: the same image of two tenants is two entries
		if auth != nil {
			key += "\x00" + auth.id
		}
		e := p.entries[key]
		if e == nil && len(p.entries) >= maxPrewarmEntries && !p.evictFinishedLocked() {
			out.Images = append(out.Images, &protobuf.PrewarmImageStatus{Image: img, State: protobuf.PrewarmState_PREWARM_STATE_FAILED,
				Error: "too many images are being warmed: try again later", UpdatedUnixMs: p.cfg.Now().UnixMilli()})
			continue
		}
		if e == nil {
			e = &prewarmEntry{}
			p.entries[key] = e
			p.order = append(p.order, key)
			p.startLocked(tenant, img, e, auth)
		} else if p.retryLocked(e) {
			p.startLocked(tenant, img, e, auth)
		}
		out.Images = append(out.Images, p.statusLocked(img, e))
	}
	return out
}

// evictFinishedLocked forgets the oldest entry that is not running (done, failed or skipped) and says whether it found one.
func (p *prewarmer) evictFinishedLocked() bool {
	for i, img := range p.order {
		e := p.entries[img]
		if e == nil || e.state == protobuf.PrewarmState_PREWARM_STATE_QUEUED || e.state == protobuf.PrewarmState_PREWARM_STATE_WARMING {
			continue
		}
		delete(p.entries, img)
		p.order = append(p.order[:i:i], p.order[i+1:]...)
		return true
	}
	return false
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

// startLocked decides at once what can be decided (refused, invalid) and
// otherwise queues the fetch behind the concurrency bound.
func (p *prewarmer) startLocked(tenant, img string, e *prewarmEntry, auth *callAuth) {
	repo, err := p.cachePath(tenant, img)
	if err != nil {
		p.setLocked(e, protobuf.PrewarmState_PREWARM_STATE_FAILED, "", err.Error())
		return
	}
	p.setLocked(e, protobuf.PrewarmState_PREWARM_STATE_QUEUED, "", "")
	go p.warm(img, repo, e, auth)
}

// cachePath returns the repository path ("c/<tenant segment>/REG/repo") the gate serves the image under for the tenant. An image the cache cannot serve (a registry it
// does not know, a reference that does not parse) is refused: with the cache on there is no direct pull.
func (p *prewarmer) cachePath(tenant, img string) (string, error) {
	rw := imagecache.Rewriter{Prefix: "cache", Registries: p.cfg.Registries}.For(tenant)
	out, err := rw.RewriteRequired(img)
	if err != nil {
		return "", err
	}
	path, ok := strings.CutPrefix(out, "cache/")
	if !ok {
		return "", fmt.Errorf("%w: %q already points at a registry", imagecache.ErrNotCacheable, img)
	}
	if i := strings.IndexAny(path, "@:"); i >= 0 && i > strings.LastIndex(path, "/") {
		path = path[:i]
	}
	return path, nil
}

func (p *prewarmer) warm(img, repo string, e *prewarmEntry, auth *callAuth) {
	p.sem <- struct{}{}
	defer func() { <-p.sem }()
	p.mu.Lock()
	p.setLocked(e, protobuf.PrewarmState_PREWARM_STATE_WARMING, "", "")
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.Timeout)
	defer cancel()
	digest, err := p.fetch(ctx, img, repo, auth)

	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.setLocked(e, protobuf.PrewarmState_PREWARM_STATE_FAILED, digest, err.Error())
		return
	}
	p.setLocked(e, protobuf.PrewarmState_PREWARM_STATE_DONE, digest, "")
}

// cacheClient is how the agent talks to the cache path of the gate: TLS when configured, the call's credentials when it has any.
func (p *prewarmer) cacheOptions(ctx context.Context, auth *callAuth) []remote.Option {
	authenticator := authn.Anonymous
	if auth != nil {
		authenticator = &authn.Basic{Username: auth.user, Password: auth.pass}
	}
	return []remote.Option{remote.WithContext(ctx), remote.WithAuth(authenticator), remote.WithTransport(p.transport())}
}

// transport is the transport of every request to the gate: TLS when configured, and the agent's token on each request.
func (p *prewarmer) transport() http.RoundTripper {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if p.cfg.CacheTLS != nil {
		tr.TLSClientConfig = p.cfg.CacheTLS
	}
	if p.cfg.Token == nil {
		return tr
	}
	return p.cfg.Token.RoundTripper(tr)
}

func (p *prewarmer) cacheName(repo, ref string) (name.Reference, error) {
	var opts []name.Option
	if p.cfg.CacheTLS == nil {
		opts = append(opts, name.Insecure)
	}
	sep := ":"
	if strings.HasPrefix(ref, "sha256:") {
		sep = "@"
	}
	return name.ParseReference(fmt.Sprintf("%s/%s%s%s", p.cfg.RegistryAddr, repo, sep, ref), opts...)
}

// fetch makes the cache hold the image: it asks the gate for the manifest, reads every blob through the gate (which stores what it
// serves once it proved the pull), then asks the gate whether every blob is in its storage.
//
// A tenant's call never learns anything from the platform's credentials: with credentials in the call the gate resolves the tag with
// them; without, the gate must first serve the tag anonymously (it proves the image is public) and only then does the agent use the
// digest the operator would pin labs to (the platform manifest of a single-architecture cluster), which it asked with the platform's
// keychain for an image that is public. The digest reported is the one the gate served, and only after it served it: an image the
// caller cannot read fails with an error and no digest.
func (p *prewarmer) fetch(ctx context.Context, img, repo string, auth *callAuth) (string, error) {
	opts := p.cacheOptions(ctx, auth)
	parsed, err := name.ParseReference(img)
	if err != nil {
		return "", err
	}
	ref := parsed.Identifier() // the tag or the digest as named
	if _, isTag := parsed.(name.Tag); isTag && auth == nil {
		// The proof that the image is public comes first; the platform's resolver is asked only about an image the gate has served.
		tagRef, err := p.cacheName(repo, ref)
		if err != nil {
			return "", err
		}
		if _, err := remote.Head(tagRef, opts...); err != nil {
			return "", fmt.Errorf("the cache could not fetch %s: %w", tagRef.Name(), err)
		}
		if p.cfg.Resolver != nil {
			d, err := p.cfg.Resolver.Resolve(ctx, img)
			if err != nil {
				return "", fmt.Errorf("resolve %s upstream: %w", img, err)
			}
			ref = d
		} else {
			return "", fmt.Errorf("no resolver: cannot resolve the tag of %s", img)
		}
	}
	cref, err := p.cacheName(repo, ref)
	if err != nil {
		return "", err
	}
	desc, err := remote.Get(cref, opts...)
	if err != nil {
		return "", fmt.Errorf("the cache could not fetch %s: %w", cref.Name(), err)
	}
	if err := p.warmManifest(ctx, cref.Context(), repo, desc, opts, auth); err != nil {
		return "", err
	}
	return desc.Digest.String(), nil
}

// readBlob reads a blob through the cache to the end and discards it: the gate fills the cache with what it streams.
func readBlob(repoName name.Repository, d v1.Hash, opts []remote.Option) error {
	l, err := remote.Layer(repoName.Digest(d.String()), opts...)
	if err != nil {
		return err
	}
	rc, err := l.Compressed()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	_, err = io.Copy(io.Discard, rc)
	return err
}

// warmManifest walks an index or image manifest, reads the config and every layer through the gate (which stores what it serves) and
// checks, again through the gate, that each is held in its storage and not only proved: a blob that was served without being stored
// (the disk was full, the fill failed) is a failed prewarm.
func (p *prewarmer) warmManifest(ctx context.Context, repoName name.Repository, repo string, desc *remote.Descriptor, opts []remote.Option, auth *callAuth) error {
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
			cd, err := remote.Get(repoName.Digest(child.Digest.String()), opts...)
			if err != nil {
				return fmt.Errorf("manifest %s of the index could not be fetched through the cache: %w", child.Digest, err)
			}
			if err := p.warmManifest(ctx, repoName, repo, cd, opts, auth); err != nil {
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
		for _, b := range append([]v1.Descriptor{m.Config}, m.Layers...) {
			if p.storedInCache(ctx, repo, b.Digest, auth) {
				continue
			}
			if err := readBlob(repoName, b.Digest, opts); err != nil {
				return fmt.Errorf("blob %s of %s could not be read through the cache: %w", b.Digest, desc.Digest, err)
			}
			if !p.storedInCache(ctx, repo, b.Digest, auth) {
				return fmt.Errorf("blob %s of %s is not stored in the cache", b.Digest, desc.Digest)
			}
		}
		return nil
	}
	return fmt.Errorf("unsupported manifest type %s", desc.MediaType)
}

// storedInCache asks the gate (a HEAD of the blob, with the call's credentials) whether the blob is held in the cache's storage: the
// gate says so in the header HeaderCache. A blob it proved but did not store is answered without it.
func (p *prewarmer) storedInCache(ctx context.Context, repo string, d v1.Hash, auth *callAuth) bool {
	scheme := "https"
	client := &http.Client{Timeout: 30 * time.Second, Transport: p.transport()}
	if p.cfg.CacheTLS == nil {
		scheme = "http"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, fmt.Sprintf("%s://%s/v2/%s/blobs/%s", scheme, p.cfg.RegistryAddr, repo, d), nil)
	if err != nil {
		return false
	}
	if auth != nil {
		req.SetBasicAuth(auth.user, auth.pass)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK && resp.Header.Get(imagecache.HeaderGateCache) == imagecache.GateCacheHit
}
