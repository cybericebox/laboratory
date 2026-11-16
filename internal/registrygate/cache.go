package registrygate

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/imagepolicy"
)

const (
	// maxManifestSize bounds a manifest the gate reads into memory (the OCI recommendation is 4 MiB).
	maxManifestSize = 4 << 20
	// authRealm is the challenge of a request without credentials: containerd answers it with the credentials of the pod.
	authRealm = `Basic realm="registry-gate"`
	// the stable bodies of a refusal (the contract's reasons).
	reasonNotAllowed  = "IMAGE_REGISTRY_NOT_ALLOWED"
	reasonUnavailable = "IMAGE_REGISTRY_UNAVAILABLE"
)

// Cache is the cache path of the gate: for every pull the proof against the upstream registry with the pull's own credentials, and the
// shared copy in zot that is served only after the proof (docs/research/2026-11-16-zot-tenant-cache.md, section 5).
type Cache struct {
	Registries *RegistrySet
	// Allowed is the laboratory's list of registries lab images may come from (the chart's registry.allowedRegistries). A registry that is
	// not on it is refused before anything else, and no upstream is called for it. Empty or "*": every registry of Registries.
	Allowed []string
	// Key is the secret of the credential hash: HMAC-SHA256(Key, registry, user, secret). The hash never leaves the process.
	Key   []byte
	Up    *Upstream
	Zot   *Zot
	Store *Store
	// DiskFull says that the volume is above its high-water mark: a miss is then streamed through and not stored.
	DiskFull func() bool
	// Fill is the optional login used to fill an image proved public (rate-limit relief); never for a private one.
	Fill AnonymousFill
	M    *Metrics

	checks, reval, fills flight
	pending              pendingSet

	mu    sync.Mutex
	where map[string]map[string]struct{} // digest -> cache repositories that hold it in zot
}

// NewCache assembles the cache path.
func NewCache(regs *RegistrySet, key []byte, up *Upstream, zot *Zot, store *Store, m *Metrics) *Cache {
	if m == nil {
		m = NewMetrics()
	}
	return &Cache{Registries: regs, Key: key, Up: up, Zot: zot, Store: store, M: m, DiskFull: func() bool { return false },
		where: map[string]map[string]struct{}{}}
}

type caller struct {
	tenant string
	creds  Creds
	hash   string // "" = anonymous
}

func (c *Cache) hash(registry string, cr Creds) string {
	m := hmac.New(sha256.New, c.Key)
	for _, s := range []string{registry, cr.User, cr.Pass} {
		m.Write([]byte(s))
		m.Write([]byte{0})
	}
	return hex.EncodeToString(m.Sum(nil))
}

func refuse(w http.ResponseWriter, status int, code, msg string, challenge bool) {
	if challenge {
		w.Header().Set("Www-Authenticate", authRealm)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

// ServeHTTP is one request of the cache path.
func (c *Cache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		refuse(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "the cache is read-only", false)
		return
	}
	o, ok := parseCachePath(r.URL.EscapedPath())
	if !ok {
		refuse(w, http.StatusNotFound, "NAME_UNKNOWN", "not an object of the cache", false)
		return
	}
	reg, ok := c.Registries.Get(o.Registry)
	if !ok || !c.registryAllowed(o.Registry) {
		c.M.refusals.WithLabelValues("not_allowed").Inc()
		refuse(w, http.StatusForbidden, "DENIED", reasonNotAllowed, false)
		return
	}
	who, ok := c.caller(o, r)
	if !ok {
		c.M.refusals.WithLabelValues("bad_credentials").Inc()
		refuse(w, http.StatusUnauthorized, "UNAUTHORIZED", "the credentials are not in the form the platform writes", true)
		return
	}
	ctx := r.Context()
	up := Object{Repo: o.Repo, Kind: o.Kind, Ref: o.Ref, Accept: r.Header.Get("Accept")}
	digest, status := c.authorize(ctx, reg, o, up, who)
	switch status {
	case http.StatusOK:
	case http.StatusServiceUnavailable:
		c.M.refusals.WithLabelValues("unavailable").Inc()
		refuse(w, status, "UNAVAILABLE", reasonUnavailable, false)
		return
	case http.StatusNotFound:
		c.M.refusals.WithLabelValues("not_found").Inc()
		refuse(w, status, "NAME_UNKNOWN", "the object is not in the upstream repository", false)
		return
	case http.StatusBadGateway:
		c.M.refusals.WithLabelValues("bad_gateway").Inc()
		refuse(w, status, "UNAVAILABLE", "the upstream registry gave no digest", false)
		return
	default:
		c.M.refusals.WithLabelValues("denied").Inc()
		refuse(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required", true)
		return
	}
	o.Ref = digest // a tag is resolved upstream with the pull's credentials and never answered from memory
	up.Ref = digest
	if o.Kind == kindManifests {
		c.serveManifest(w, r, reg, o, up, who)
		return
	}
	c.serveBlob(w, r, reg, o, up, who)
}

// caller reads the Authorization header: none is anonymous, Basic is "<tenant>~<user>:<secret>".
func (c *Cache) caller(o objectRef, r *http.Request) (caller, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return caller{}, true
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		return caller{}, false
	}
	tenant, up, ok := splitUser(user)
	if !ok {
		return caller{}, false
	}
	cr := Creds{User: up, Pass: pass}
	return caller{tenant: tenant, creds: cr, hash: c.hash(o.Registry, cr)}, true
}

// authorize is the check of 5.2. It returns the digest of the object to serve (a tag is resolved upstream here) and an HTTP status:
// 200 serve, 401 refused, 503 upstream unreachable and no stale answer allowed, 502 no digest.
func (c *Cache) authorize(ctx context.Context, reg Registry, o objectRef, up Object, who caller) (string, int) {
	key := DecisionKey{Hash: who.hash, Repo: o.fullRepo()}
	if c.Store.Missing(MissKey{Hash: who.hash, Repo: key.Repo, Kind: o.Kind, Ref: o.Ref}) {
		return "", statusOf(NotFound, who.hash == "") // found missing a moment ago: the same ref is not asked again
	}
	if o.isTag() {
		// a tag moves: the one HEAD that resolves it is a fresh check too
		started := c.Store.Now()
		out, hdr, _ := c.Up.Proof(ctx, reg, up, who.creds, who.hash)
		c.M.checks.WithLabelValues(outcomeName(out)).Inc()
		if out == NotFound {
			c.settleMissing(ctx, reg, key, who, up)
			return "", statusOf(NotFound, who.hash == "")
		}
		var kn *knownRef
		d := hdr.Get("Docker-Content-Digest")
		if out == Allowed { // the tag resolves to its digest; without one the tag itself is what a later check asks about
			kn = &knownRef{kindManifests, d, up.Accept}
			if !reDigest.MatchString(d) {
				kn = &knownRef{kindManifests, up.Ref, up.Accept}
			}
		}
		serve := c.Store.Record(key, who.tenant, out, started, kn)
		if !serve {
			return "", statusOf(out, who.hash == "")
		}
		if out != Allowed {
			return "", http.StatusServiceUnavailable // a tag cannot be resolved without upstream
		}
		if !reDigest.MatchString(d) {
			return "", http.StatusBadGateway
		}
		return d, http.StatusOK
	}
	for attempt := 0; attempt < 2; attempt++ {
		switch c.Store.Lookup(key, who.tenant) {
		case Serve:
			c.M.decisions.WithLabelValues("serve").Inc()
			return o.Ref, http.StatusOK
		case ServeStale:
			c.M.decisions.WithLabelValues("stale").Inc()
			c.revalidate(reg, key, who, up)
			return o.Ref, http.StatusOK
		case Refuse:
			c.M.decisions.WithLabelValues("refused").Inc()
			return "", http.StatusUnauthorized
		}
		c.M.decisions.WithLabelValues("check").Inc()
		var result int
		// one fresh check per decision at a time: the others of a burst wait for it and then look the decision up again
		_, leader := c.checks.Do(key.Hash+"|"+key.Repo+"|"+who.tenant, func() error {
			result = c.freshCheck(ctx, reg, key, who, up, false)
			return nil
		})
		if leader {
			return o.Ref, result
		}
	}
	return o.Ref, c.freshCheck(ctx, reg, key, who, up, false)
}

// freshCheck asks upstream and records the answer. A revalidation (the background check of a stale decision) asks about a ref that was
// already proven for the key, not about the ref of the request: a 404 on it is a revocation or a hidden repository.
func (c *Cache) freshCheck(ctx context.Context, reg Registry, key DecisionKey, who caller, up Object, revalidating bool) int {
	known := false
	if kg, ok := c.Store.Known(key); ok && revalidating {
		up, known = Object{Repo: up.Repo, Kind: kg.Kind, Ref: kg.Ref, Accept: kg.Accept}, true
	}
	started := c.Store.Now()
	out, _ := c.Up.Check(ctx, reg, up, who.creds, who.hash)
	c.M.checks.WithLabelValues(outcomeName(out)).Inc()
	if out == NotFound {
		if !known {
			c.settleMissing(ctx, reg, key, who, up)
			return statusOf(NotFound, who.hash == "")
		}
		out = Denied // the ref that was proven is gone for these credentials
	}
	if c.Store.Record(key, who.tenant, out, started, &knownRef{up.Kind, up.Ref, up.Accept}) {
		return http.StatusOK
	}
	return statusOf(out, who.hash == "")
}

// settleMissing handles a 404 on the proof of a ref the caller chose: the request is not served and the ref alone is noted missing
// for a short while. What it never does is deny the repository because of the caller's ref: a 404 on a ref of an attacker's choosing
// must not deny it for everybody who shares the hash (or the anonymous key). But when the key holds an Allowed record, a ref that was
// already proven for it is asked about, and that is OUR ref, so the re-check is safe for any key, the anonymous one included (a public
// image that turned private must stop being served from the cache): 401, 403 or 404 on it denies the key, in one atomic step of the
// store. An Allowed record always has a known ref (the store refuses an allow without one). With no Allowed record (a first contact,
// a refused key) nothing is recorded about the repository.
func (c *Cache) settleMissing(ctx context.Context, reg Registry, key DecisionKey, who caller, up Object) {
	c.Store.NoteMissing(MissKey{Hash: who.hash, Repo: key.Repo, Kind: up.Kind, Ref: up.Ref})
	kg, ok := c.Store.Known(key)
	if !ok {
		return // no allowed record: nothing to re-check (an allowed record always has a known ref)
	}
	started := c.Store.Now()
	if kg.Kind == up.Kind && kg.Ref == up.Ref {
		c.Store.SettleMissing(key, started, NotFound) // the proven ref itself is gone
		return
	}
	out, _ := c.Up.Check(ctx, reg, Object{Repo: up.Repo, Kind: kg.Kind, Ref: kg.Ref, Accept: kg.Accept}, who.creds, who.hash)
	c.M.checks.WithLabelValues("recheck_" + outcomeName(out)).Inc()
	c.Store.SettleMissing(key, started, out)
}

// statusOf is the answer to a request that may not be served. An outage is a 503 with a stable body, so a pod shows ImagePullBackOff and
// not a misleading 401, except for a request without credentials: its 401 makes containerd come back with the pod's credentials, which
// may still be served from a verified decision.
func statusOf(o Outcome, anonymous bool) int {
	switch {
	case o == Unreachable && !anonymous:
		return http.StatusServiceUnavailable
	case o == NotFound && !anonymous:
		return http.StatusNotFound
	}
	return http.StatusUnauthorized
}

func outcomeName(o Outcome) string {
	switch o {
	case Allowed:
		return "allowed"
	case Denied:
		return "denied"
	case NotFound:
		return "not_found"
	}
	return "unreachable"
}

// revalidate checks again in the background (once per decision) while the stale answer is being served.
func (c *Cache) revalidate(reg Registry, key DecisionKey, who caller, up Object) {
	c.reval.Start(key.Hash+"|"+key.Repo+"|"+who.tenant, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c.freshCheck(ctx, reg, key, who, up, true)
	})
}

// ---- serving -------------------------------------------------------------------------------------------------------------------------

func (c *Cache) holds(repo, digest string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.where[digest] == nil {
		c.where[digest] = map[string]struct{}{}
	}
	c.where[digest][repo] = struct{}{}
}

// mountSource is a repository that holds the blob in zot. Holding it is not the caller's right to it: a manifest that lists a digest
// proves nothing (whoever controls an upstream repository can list any digest), so a blob is linked into a repository only after
// proveBlob.
func (c *Cache) mountSource(repo, digest string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for other := range c.where[digest] {
		if other != repo {
			return other
		}
	}
	return ""
}

// proveBlob is the proof that THIS blob is in the requesting repository for the caller: one HEAD upstream with the caller's own
// credentials, kept for MaxAge per credential, repository and digest. It stands before a blob that was not fetched for this
// repository is linked in or served. Only a direct 200 proves it: a redirect or a 404 is inconclusive, nothing is linked in and the
// blob is fetched through for this repository instead.
func (c *Cache) proveBlob(ctx context.Context, reg Registry, o objectRef, who caller) bool {
	k := BlobKey{Hash: who.hash, Repo: o.fullRepo(), Digest: o.Ref}
	if c.Store.BlobProven(k) {
		return true
	}
	started := c.Store.Now()
	out, _, _ := c.Up.Proof(ctx, reg, Object{Repo: o.Repo, Kind: kindBlobs, Ref: o.Ref}, who.creds, who.hash)
	c.M.checks.WithLabelValues("blob_" + outcomeName(out)).Inc()
	if out != Allowed {
		return false
	}
	c.Store.BlobProve(k, started)
	return true
}

func copyHeaders(w http.ResponseWriter, h http.Header, names ...string) {
	for _, n := range names {
		if v := h.Get(n); v != "" {
			w.Header().Set(n, v)
		}
	}
}

// streamFrom writes a zot (or upstream) response to the client.
func streamFrom(w http.ResponseWriter, r *http.Request, resp *http.Response, digest string) {
	copyHeaders(w, resp.Header, "Content-Type", "Content-Length", "Content-Range", "Accept-Ranges")
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
}

func (c *Cache) serveManifest(w http.ResponseWriter, r *http.Request, reg Registry, o objectRef, up Object, who caller) {
	ctx := r.Context()
	repo := o.zotRepo()
	if _, ok, err := c.Zot.Head(ctx, repo, kindManifests, o.Ref); err == nil && ok {
		if resp, err := c.Zot.Get(ctx, repo, kindManifests, o.Ref, "", r.Header.Get("Accept")); err == nil && resp != nil {
			defer func() { _ = resp.Body.Close() }()
			c.M.cache.WithLabelValues("manifest", "hit").Inc()
			c.holds(repo, o.Ref)
			w.Header().Set(imagecache.HeaderGateCache, imagecache.GateCacheHit)
			streamFrom(w, r, resp, o.Ref)
			return
		}
	}
	if pm, ok := c.pending.get(repo, o.Ref); ok {
		c.M.cache.WithLabelValues("manifest", "hit_memory").Inc()
		c.writeManifest(w, r, pm.ct, o.Ref, pm.body)
		return
	}
	c.M.cache.WithLabelValues("manifest", "miss").Inc()
	fc, fk := c.fillCreds(who, o.Registry)
	resp, out, _ := c.Up.Fetch(ctx, reg, up, fc, fk)
	if resp == nil {
		c.M.fills.WithLabelValues("manifest", "upstream_"+outcomeName(out)).Inc()
		c.refuseUpstream(w, out, who.hash == "")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
	if err != nil || len(body) > maxManifestSize {
		c.M.fills.WithLabelValues("manifest", "bad").Inc()
		refuse(w, http.StatusBadGateway, "UNAVAILABLE", "the upstream manifest cannot be read", false)
		return
	}
	sum := sha256.Sum256(body)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != o.Ref {
		c.M.fills.WithLabelValues("manifest", "digest_mismatch").Inc()
		refuse(w, http.StatusBadGateway, "DIGEST_INVALID", "the upstream manifest does not match its digest", false)
		return
	}
	ct := manifestType(body, resp.Header.Get("Content-Type"))
	c.remember(ctx, repo, o.Ref, ct, body)
	c.writeManifest(w, r, ct, o.Ref, body)
}

func (c *Cache) writeManifest(w http.ResponseWriter, r *http.Request, ct, digest string, body []byte) {
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// manifestType is the media type zot is told a manifest has: what the manifest says of itself, else what the upstream sent when that is
// a manifest type, else the OCI image manifest.
func manifestType(body []byte, sent string) string {
	var m struct {
		MediaType string `json:"mediaType"`
	}
	if json.Unmarshal(body, &m) == nil && m.MediaType != "" {
		return m.MediaType
	}
	if strings.Contains(sent, "manifest") || strings.Contains(sent, "image.index") {
		return sent
	}
	return "application/vnd.oci.image.manifest.v1+json"
}

var errFillFailed = errors.New("the blob could not be stored")

func (c *Cache) serveBlob(w http.ResponseWriter, r *http.Request, reg Registry, o objectRef, up Object, who caller) {
	ctx := r.Context()
	repo := o.zotRepo()
	if c.serveBlobFromZot(w, r, repo, o.Ref) {
		return
	}
	if r.Method == http.MethodHead {
		out, hdr, _ := c.Up.Head(ctx, reg, up, who.creds, who.hash)
		if out != Allowed {
			c.refuseUpstream(w, out, who.hash == "")
			return
		}
		copyHeaders(w, hdr, "Content-Length", "Content-Type")
		w.Header().Set("Docker-Content-Digest", o.Ref)
		w.WriteHeader(http.StatusOK)
		return
	}
	c.M.cache.WithLabelValues("blob", "miss").Inc()
	if src := c.mountSource(repo, o.Ref); src != "" && c.proveBlob(ctx, reg, o, who) {
		if err := c.Zot.Mount(ctx, repo, o.Ref, src); err == nil {
			c.M.cache.WithLabelValues("blob", "mounted").Inc()
			c.holds(repo, o.Ref)
			c.stored(ctx, repo, o.Ref)
			if c.serveBlobFromZot(w, r, repo, o.Ref) {
				return
			}
		}
	}
	if c.DiskFull() {
		c.M.fills.WithLabelValues("blob", "bypass_disk_full").Inc()
		c.streamThrough(w, r, reg, up, o.Ref, who)
		return
	}
	rangeReq := r.Header.Get("Range") != ""
	client := w
	if rangeReq {
		client = nil // a range is answered from zot, after the blob is stored
	}
	sent := false // the leader has begun the response to its own client
	err, leader := c.fills.Do(repo+"|"+o.Ref, func() error {
		return c.fillBlob(ctx, reg, o, who, client, r, &sent)
	})
	if leader && sent {
		return // streamed to this client while stored (or cut: the connection tells the client)
	}
	if leader && err != nil && !errors.Is(err, errFillFailed) {
		c.refuseFill(w, err, who.hash == "") // upstream said no, or could not be reached, before anything was sent
		return
	}
	if c.serveBlobFromZot(w, r, repo, o.Ref) {
		return
	}
	// the fill failed on the storage side, or it was another request's: this client is served by its own fetch, and nothing is stored
	c.streamThrough(w, r, reg, up, o.Ref, who)
}

func (c *Cache) refuseUpstream(w http.ResponseWriter, out Outcome, anonymous bool) {
	switch statusOf(out, anonymous) {
	case http.StatusUnauthorized:
		refuse(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required", true)
	case http.StatusNotFound:
		refuse(w, http.StatusNotFound, "NAME_UNKNOWN", "the object is not in the upstream repository", false)
	default:
		refuse(w, http.StatusServiceUnavailable, "UNAVAILABLE", reasonUnavailable, false)
	}
}

type fillRefused struct{ out Outcome }

func (e fillRefused) Error() string { return "upstream refused the blob: " + outcomeName(e.out) }

func (c *Cache) refuseFill(w http.ResponseWriter, err error, anonymous bool) {
	var fr fillRefused
	if errors.As(err, &fr) {
		c.refuseUpstream(w, fr.out, anonymous)
		return
	}
	refuse(w, http.StatusServiceUnavailable, "UNAVAILABLE", reasonUnavailable, false)
}

// serveBlobFromZot answers from the stored copy (a Range included) and says whether it did.
func (c *Cache) serveBlobFromZot(w http.ResponseWriter, r *http.Request, repo, digest string) bool {
	ctx := r.Context()
	if r.Method == http.MethodHead {
		hdr, ok, err := c.Zot.Head(ctx, repo, kindBlobs, digest)
		if err != nil || !ok {
			return false
		}
		copyHeaders(w, hdr, "Content-Length", "Content-Type")
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set(imagecache.HeaderGateCache, imagecache.GateCacheHit) // stored: the prewarm asks for it
		w.WriteHeader(http.StatusOK)
		return true
	}
	resp, err := c.Zot.Get(ctx, repo, kindBlobs, digest, r.Header.Get("Range"), "")
	if err != nil || resp == nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	c.M.cache.WithLabelValues("blob", "hit").Inc()
	c.holds(repo, digest)
	w.Header().Set(imagecache.HeaderGateCache, imagecache.GateCacheHit)
	streamFrom(w, r, resp, digest)
	return true
}

// fillBlob downloads the blob from upstream with the pull's credentials and stores it in zot, streaming it to the client (when there is
// one) while it does. A client that is gone does not stop the store, and a store that fails does not stop the client. *sent says that the
// response to the client was begun.
func (c *Cache) fillBlob(ctx context.Context, reg Registry, o objectRef, who caller, client http.ResponseWriter, r *http.Request, sent *bool) error {
	// the fill belongs to everyone waiting for the blob: it outlives the request that started it
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
	defer cancel()
	fc, fk := c.fillCreds(who, o.Registry)
	resp, out, _ := c.Up.Fetch(ctx, reg, Object{Repo: o.Repo, Kind: kindBlobs, Ref: o.Ref}, fc, fk)
	if resp == nil {
		c.M.fills.WithLabelValues("blob", "upstream_"+outcomeName(out)).Inc()
		return fillRefused{out}
	}
	defer func() { _ = resp.Body.Close() }()
	size := resp.ContentLength
	if size < 0 {
		// no length, no monolithic upload: this one is not stored
		c.M.fills.WithLabelValues("blob", "no_length").Inc()
		if client != nil {
			*sent = true
			streamFrom(client, r, resp, o.Ref)
			return nil
		}
		return errFillFailed
	}
	pr, pw := io.Pipe()
	stored := make(chan error, 1)
	go func() {
		err := c.Zot.PutBlob(ctx, o.zotRepo(), o.Ref, pr, size)
		_ = pr.CloseWithError(err) // a failed store must not leave the copy below blocked on the pipe
		stored <- err
	}()
	sinks := []io.Writer{&lenient{w: pw}}
	if client != nil {
		copyHeaders(client, resp.Header, "Content-Type")
		client.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		client.Header().Set("Docker-Content-Digest", o.Ref)
		client.WriteHeader(http.StatusOK)
		*sent = true
		sinks = append(sinks, &lenient{w: client})
	}
	n, copyErr := io.Copy(io.MultiWriter(sinks...), resp.Body)
	if copyErr != nil || n != size {
		_ = pw.CloseWithError(errors.New("the upstream stream was cut"))
		<-stored //nolint:errcheck // the error of a cut upload is the cut itself
		c.M.fills.WithLabelValues("blob", "cut").Inc()
		return errFillFailed
	}
	_ = pw.Close()
	if err := <-stored; err != nil {
		c.M.fills.WithLabelValues("blob", "store_failed").Inc()
		return errFillFailed
	}
	c.holds(o.zotRepo(), o.Ref)
	c.M.fills.WithLabelValues("blob", "stored").Inc()
	c.M.filledBytes.Add(float64(size))
	c.stored(ctx, o.zotRepo(), o.Ref)
	return nil
}

// lenient is a writer whose first error is remembered and later writes are dropped, so one broken sink does not stop the others.
type lenient struct {
	w   io.Writer
	err error
}

func (l *lenient) Write(p []byte) (int, error) {
	if l.err == nil {
		if _, err := l.w.Write(p); err != nil {
			l.err = err
		}
	}
	return len(p), nil
}

// streamThrough serves the blob from upstream with the pull's credentials and keeps nothing (the disk is full, or the store failed).
func (c *Cache) streamThrough(w http.ResponseWriter, r *http.Request, reg Registry, up Object, digest string, who caller) {
	up.Range = r.Header.Get("Range")
	fc, fk := c.fillCreds(who, reg.Name)
	resp, out, _ := c.Up.Fetch(r.Context(), reg, up, fc, fk)
	if resp == nil {
		c.refuseUpstream(w, out, who.hash == "")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	streamFrom(w, r, resp, digest)
}

// registryAllowed says whether the registry is on the laboratory's allowed list (the same canonical spelling as the operator and the agent).
func (c *Cache) registryAllowed(name string) bool {
	if len(c.Allowed) == 0 {
		return true
	}
	want := imagepolicy.CanonicalRegistry(name)
	for _, a := range c.Allowed {
		if a = strings.TrimSpace(a); a == imagepolicy.AnyRegistry || imagepolicy.CanonicalRegistry(a) == want {
			return true
		}
	}
	return false
}
