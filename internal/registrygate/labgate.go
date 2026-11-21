package registrygate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cybericebox/laboratory/internal/names"
)

// The stable error codes of a refused push (the OCI error "code"); node-state maps them onto its deferrals and the lab status warning.
const (
	CodePushInterval = "CIB_PUSH_INTERVAL"
	CodePushBudget   = "CIB_PUSH_BUDGET"
	CodeTenantQuota  = "CIB_TENANT_QUOTA"
	CodeRegistryFull = "CIB_REGISTRY_FULL"
	CodeExitLimit    = "CIB_EXIT_LIMIT"
	CodeUploadLimit  = "CIB_UPLOAD_LIMIT"
)

// PushLimits are the limits of snapshot pushes (the chart's devices.statePersistence.*, rendered into the gate).
type PushLimits struct {
	// MinInterval is the least time between two accepted snapshots (manifest PUT) of a device (minPushInterval, 60s). 0: none.
	MinInterval time.Duration
	// Budget is the most blob bytes accepted into one device repository within BudgetWindow (pushBudget 2Gi, pushBudgetWindow 1h). 0: none.
	Budget       int64
	BudgetWindow time.Duration
	// ExitMaxBytes is the most one exit push (a pod that ended pushes outside the interval and the budget) may carry
	// (GATE_EXIT_MAX_BYTES); 0: the device's write quota, and without one the Budget. ExitPushesPerHour is how many exit pushes a device
	// gets in a sliding hour whatever the pod UIDs (GATE_EXIT_PUSHES_PER_HOUR, default 6). Past either: 429 CIB_EXIT_LIMIT.
	ExitMaxBytes      int64
	ExitPushesPerHour int
	// UploadsPerDevice and UploadsPerNode bound the open upload sessions (GATE_UPLOADS_PER_DEVICE 4, GATE_UPLOADS_PER_NODE 64): 429
	// CIB_UPLOAD_LIMIT.
	UploadsPerDevice int
	UploadsPerNode   int
	// Reserve is the fraction of the registry volume that must stay free (registryReserve, 0.1). 0: none.
	Reserve float64
	// TenantQuota is the platform's quota of the registry bytes of one tenant's snapshots (tenantQuota, 10Gi), used when neither the
	// Tenant nor the Device names one. 0: none.
	TenantQuota int64
}

// BlobMeter says which blobs a tenant's snapshots hold in the registry (digest -> size), compressed registry bytes, each counted once and
// without the blobs that the shared base image or the image cache hold too. exceptRepo is left out (the repository being replaced).
type BlobMeter interface {
	TenantBlobs(ctx context.Context, tenant, exceptRepo string) (map[string]int64, error)
}

// LabGate serves the snapshot repositories (lab/**) for an authenticated principal: who may read and write which repository, and how much
// a node may push. Everything the node says is checked against the view of the cluster (the schedule): a node writes only the repository
// of a snapshot-backed device that runs on it, and never deletes.
type LabGate struct {
	Schedule *Schedule
	Limits   PushLimits
	Meter    BlobMeter
	// Store is zot as the gate's own client (the previous snapshot is read from it); the proxy forwards the checked request with the
	// gate's account.
	Store SnapshotStore
	Super *Superseder
	// Cache holds the grants and reads the image references of pods: a blob may be mounted into a snapshot only from the image the device
	// itself runs on the node (nil: no mounts).
	Cache *Cache
	// Free returns the free fraction of the registry volume (nil: not measured, never refuses).
	Free func() float64
	// MaxStale: past this age of the cluster view nothing that needs it is answered (503).
	MaxStale time.Duration
	Now      func() time.Time
	M        *Metrics

	proxy      *httputil.ReverseProxy
	zot        *url.URL
	user, pass string
	mu         sync.Mutex
	state      map[string]*pushState // by repository
	sess       map[string]uploadSession
}

type charge struct {
	at    time.Time
	bytes int64
	exit  bool // charged to the pool of exit snapshots
}

type pushState struct {
	lastManifest time.Time
	charges      []*charge
	exitPushes   []time.Time // accepted exit snapshots within the last hour, whatever the pod
	// prev is the digest "latest" had when the snapshot being pushed was admitted; the plan to delete it starts when zot accepts the push.
	prev string
}

type uploadSession struct {
	node, repo string
	started    time.Time
	lastUsed   time.Time
}

const (
	maxSessions = 20000
	// the sessions one device and one node may have open at once
	maxSessionsPerDevice = 32
	maxSessionsPerNode   = 512
	maxManifestBlobs     = 512
)

// NewLabGate makes the gate for snapshot repositories forwarding to zot with the account user:pass.
func NewLabGate(zot *url.URL, user, pass string) *LabGate {
	g := &LabGate{state: map[string]*pushState{}, sess: map[string]uploadSession{}, zot: zot, user: user, pass: pass}
	g.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(zot)
			r.Out.Host = r.In.Host
			StripPlatformHeaders(r.Out.Header)
			r.Out.Header.Del("Authorization")
			r.Out.SetBasicAuth(user, pass)
		},
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			g.afterZot(resp)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "the registry store is not reachable", http.StatusBadGateway)
		},
	}
	return g
}

func (g *LabGate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *LabGate) maxStale() time.Duration {
	if g.MaxStale > 0 {
		return g.MaxStale
	}
	return 15 * time.Minute
}

func (g *LabGate) metric(result string) {
	if g.M != nil {
		g.M.lab.WithLabelValues(result).Inc()
	}
}

// refusal is why a request is not forwarded.
type refusal struct {
	status int
	code   string
	msg    string
	retry  time.Duration
}

func (g *LabGate) deny(w http.ResponseWriter, f refusal) {
	g.metric(f.code)
	if f.retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(f.retry/time.Second)+1))
	}
	refuse(w, f.status, f.code, f.msg, false)
}

var (
	denied      = refusal{http.StatusForbidden, "DENIED", "not allowed", 0}
	unavailable = refusal{http.StatusServiceUnavailable, "UNAVAILABLE", "the cluster view is not available, try again", 30 * time.Second}
	fullRefusal = refusal{http.StatusInsufficientStorage, CodeRegistryFull, "the snapshot registry is nearly full", 0}
)

// stateLocked is the push state of a repository (g.mu held).
func (g *LabGate) stateLocked(repo string) *pushState {
	s := g.state[repo]
	if s == nil {
		s = &pushState{}
		g.state[repo] = s
	}
	return s
}

// Serve handles one request of principal p on a path under /v2/lab/, /v2/_catalog or /v2/base/ (the router has checked the path and who
// the caller is).
func (g *LabGate) Serve(w http.ResponseWriter, r *http.Request, p Principal) {
	r = WithNode(r, p.Node)
	r = r.WithContext(context.WithValue(r.Context(), bodyKey, &bodyRef{}))
	escaped := r.URL.EscapedPath()
	if p.Kind == KindOperator || p.Kind == KindAgent {
		if f, ok := g.authorizeStaff(r, p, escaped); !ok {
			g.deny(w, f)
			return
		}
		g.metric("allowed_" + p.Kind.String())
		g.proxy.ServeHTTP(w, r)
		return
	}
	lp, ok := parseLabPath(escaped)
	if !ok {
		g.deny(w, denied)
		return
	}
	if !g.Schedule.Fresh(g.maxStale()) {
		g.deny(w, unavailable)
		return
	}
	dev, ok := g.Schedule.Device(lp.NS, lp.Lab, lp.Dev)
	if !ok || !dev.StateEnabled || !g.deviceOnNode(dev, p.Node) {
		if lp.Kind == kindUploads && lp.Ref != "" && g.dropSession(lp, p) {
			// the device is gone or moved: the session it had open is unknown from now on, and zot is told to forget it
			g.deny(w, refusal{http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "unknown upload session", 0})
			return
		}
		g.deny(w, denied) // not snapshot-backed, or not on the calling node: the node has no right to this repository
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if lp.Kind == kindUploads {
			if f, ok := g.checkSession(lp, p); !ok {
				g.deny(w, f)
				return
			}
		}
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		if f, ok := g.authorizeWrite(r, lp, p, dev); !ok {
			g.deny(w, f)
			return
		}
	default: // DELETE and the rest: a node never deletes
		g.deny(w, denied)
		return
	}
	g.metric("allowed_node")
	g.proxy.ServeHTTP(w, r)
}

// deviceOnNode: the Device says it runs on the node, or a pod of the device is bound to it (the pod exists before the status is written).
func (g *LabGate) deviceOnNode(dev DeviceInfo, node string) bool {
	if node == "" {
		return false
	}
	if dev.Node == node {
		return true
	}
	for _, pod := range g.Schedule.PodsOnNode(node) {
		if pod.Namespace == dev.Namespace && pod.Labels[names.LabelLab] == dev.Lab && pod.Labels[names.LabelDevice] == dev.Name {
			return true
		}
	}
	return false
}

// authorizeStaff is the rule of the operator and the agent.
func (g *LabGate) authorizeStaff(r *http.Request, p Principal, escaped string) (refusal, bool) {
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case strings.HasPrefix(escaped, "/v2/lab/") && read:
		return refusal{}, true
	case p.Kind == KindOperator && strings.HasPrefix(escaped, "/v2/lab/") && r.Method == http.MethodDelete:
		if lp, ok := parseLabPath(escaped); ok && (lp.Kind == kindManifests || lp.Kind == kindBlobs) {
			return refusal{}, true
		}
	case p.Kind == KindOperator && read && (escaped == "/v2/_catalog" || strings.HasSuffix(escaped, "/tags/list") || isBasePath(escaped)):
		return refusal{}, true
	}
	return denied, false
}

// authorizeWrite is the push rule of a node for the repository of its own device.
func (g *LabGate) authorizeWrite(r *http.Request, lp labPath, p Principal, dev DeviceInfo) (refusal, bool) {
	switch lp.Kind {
	case kindManifests:
		if r.Method != http.MethodPut || lp.Ref != tagLatest {
			return denied, false // only the tag "latest" is written
		}
		return g.authorizeManifestPut(r, lp, p, dev)
	case kindUploads:
		return g.authorizeUpload(r, lp, p, dev)
	}
	return denied, false
}

func (g *LabGate) registryFull() bool {
	return g.Limits.Reserve > 0 && g.Free != nil && g.Free() < g.Limits.Reserve
}

// quotaOf is the registry quota that applies to a device's tenant: the device's own value, else the tenant's, else the platform's.
func (g *LabGate) quotaOf(dev DeviceInfo) int64 {
	if dev.TenantQuotaBytes > 0 {
		return dev.TenantQuotaBytes
	}
	if t, ok := g.Schedule.Tenant(dev.Tenant); ok && t.RegistryQuotaBytes > 0 {
		return t.RegistryQuotaBytes
	}
	return g.Limits.TenantQuota
}

func (g *LabGate) window() time.Duration {
	if g.Limits.BudgetWindow > 0 {
		return g.Limits.BudgetWindow
	}
	return time.Hour
}

// used is the bytes charged in the budget window of one pool (older charges are forgotten); retry is when the oldest charge leaves it.
// g.mu is not held.
func (g *LabGate) used(repo string, exit bool, now time.Time) (used int64, retry time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.usedLocked(repo, exit, now)
}

func (g *LabGate) usedLocked(repo string, exit bool, now time.Time) (used int64, retry time.Duration) {
	s := g.stateLocked(repo)
	kept := s.charges[:0]
	var oldest time.Time
	for _, c := range s.charges {
		if now.Sub(c.at) >= g.window() {
			continue
		}
		kept = append(kept, c)
		if c.exit == exit {
			used += c.bytes
			if oldest.IsZero() || c.at.Before(oldest) {
				oldest = c.at
			}
		}
	}
	s.charges = kept
	retry = time.Minute
	if !oldest.IsZero() {
		retry = oldest.Add(g.window()).Sub(now)
	}
	return used, retry
}

// take reserves up to want bytes of the pool atomically (the check and the charge are one step under the lock, so concurrent uploads
// share what is left and never each get all of it) and returns how many it granted (0: the pool is used up). The caller gives back what it
// did not use with refund. limit is the size of the pool.
func (g *LabGate) take(repo string, exit bool, want, limit int64) (*charge, int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	used, _ := g.usedLocked(repo, exit, now)
	left := limit - used
	if left <= 0 {
		return nil, 0
	}
	if want > left {
		want = left
	}
	s := g.stateLocked(repo)
	if n := len(s.charges); n > 0 && s.charges[n-1].exit == exit && now.Sub(s.charges[n-1].at) < time.Second {
		c := s.charges[n-1]
		c.bytes += want
		return c, want
	}
	c := &charge{at: now, bytes: want, exit: exit}
	s.charges = append(s.charges, c)
	return c, want
}

func (g *LabGate) refund(c *charge, n int64) {
	if c == nil || n <= 0 {
		return
	}
	g.mu.Lock()
	c.bytes -= n
	g.mu.Unlock()
}

// budgetBody takes every read of an upload body from the pool as it happens and cuts the upload when the pool is used up. What it read stays
// charged even when zot then refuses the request: a rejected body is still traffic the node sent (only what was reserved and not sent comes back).
type budgetBody struct {
	rc    io.ReadCloser
	g     *LabGate
	repo  string
	exit  bool
	limit int64
}

var errBudget = errors.New("the push budget of the device is used up")

func (b *budgetBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c, got := b.g.take(b.repo, b.exit, int64(len(p)), b.limit)
	if got == 0 {
		// the pool is used up: a body that ends exactly here is fine, one that goes on is cut
		var one [1]byte
		if n, err := b.rc.Read(one[:]); n == 0 && err != nil {
			return 0, err
		}
		return 0, errBudget
	}
	n, err := b.rc.Read(p[:got])
	b.g.refund(c, got-int64(n))
	return n, err
}

func (b *budgetBody) Close() error { return b.rc.Close() }

// bodyRef lets the response hook find the budgeted body of the request it is answering.
type bodyRef struct {
	b *budgetBody
	// exitSlot is the time of the exit-push slot this request reserved (zero: none); the response hook gives it back when zot refuses.
	exitSlot time.Time
}

const bodyKey ctxKey = 2

// authorizeUpload: POST starts an upload (or mounts a blob), PATCH and PUT go on with it. A session belongs to the node and repository
// that started it; every later request re-checks that the device is still on the node (done by Serve before this).
func (g *LabGate) authorizeUpload(r *http.Request, lp labPath, p Principal, dev DeviceInfo) (refusal, bool) {
	exit := g.exitPod(lp, p)
	if exit {
		if f, _, ok := g.exitAllowed(lp, false); !ok {
			return f, false
		}
	}
	if r.Method == http.MethodPost {
		q := r.URL.Query()
		if q.Get("mount") != "" {
			// the gate computes the source itself: the image of this very device on the calling node whose live proven grant holds the digest.
			// The "from" the node sent is ignored, and nothing is ever mounted from base or from another repository.
			from, _, _, ok := g.mountSource(lp, p, dev, q.Get("mount"), exit)
			if !ok {
				return denied, false
			}
			q.Set("from", from)
			r.URL.RawQuery = q.Encode()
		} else if q.Has("from") || q.Has("mount") {
			q.Del("from")
			q.Del("mount")
			r.URL.RawQuery = q.Encode()
		}
		if g.registryFull() {
			return fullRefusal, false
		}
		if f, ok := g.quotaStillOpen(r.Context(), dev, lp); !ok {
			return f, false
		}
		// a monolithic upload carries its bytes in this request: they are budgeted like a PATCH
		return g.budgetBodyOf(r, lp, p, dev, exit)
	}
	if f, ok := g.checkSession(lp, p); !ok {
		return f, false
	}
	if g.registryFull() {
		return fullRefusal, false
	}
	return g.budgetBodyOf(r, lp, p, dev, exit)
}

// budgetBodyOf puts the body of r under the budget: refused at once when it declares more than is left, otherwise taken from the pool read
// by read. A body that carries a digest of the image the device runs (the grant) is the image's layer, not the snapshot's: not budgeted.
func (g *LabGate) budgetBodyOf(r *http.Request, lp labPath, p Principal, dev DeviceInfo, exit bool) (refusal, bool) {
	limit := g.poolLimit(dev, exit)
	if limit <= 0 || r.ContentLength == 0 || r.Body == nil || r.Body == http.NoBody {
		return refusal{}, true
	}
	// the image's own layer is not the snapshot's: exempt only when the body declares its length and it is not larger than the descriptor
	// size the proven manifest gave; anything else is budgeted
	if d := r.URL.Query().Get("digest"); d != "" && r.ContentLength > 0 {
		if _, size, sized, ok := g.mountSource(lp, p, dev, d, exit); ok && sized && r.ContentLength <= size {
			return refusal{}, true
		}
	}
	used, retry := g.used(lp.Repo, exit, g.now())
	if r.ContentLength > 0 && used+r.ContentLength > limit {
		if exit {
			return refusal{http.StatusTooManyRequests, CodeExitLimit, "an exit snapshot of this device may not be this big", retry}, false
		}
		return refusal{http.StatusTooManyRequests, CodePushBudget, "the push budget of the device is used up", retry}, false
	}
	b := &budgetBody{rc: r.Body, g: g, repo: lp.Repo, exit: exit, limit: limit}
	r.Body = b
	if br, ok := r.Context().Value(bodyKey).(*bodyRef); ok {
		br.b = b
	}
	return refusal{}, true
}

// poolLimit is the size of the pool a push is taken from: the window budget, or for an exit push its own ceiling.
func (g *LabGate) poolLimit(dev DeviceInfo, exit bool) int64 {
	if !exit {
		return g.Limits.Budget
	}
	switch {
	case g.Limits.ExitMaxBytes > 0:
		return g.Limits.ExitMaxBytes
	case dev.WriteQuotaBytes > 0:
		return dev.WriteQuotaBytes
	}
	return g.Limits.Budget
}

// exitAllowed: at most ExitPushesPerHour exit pushes of a device in a sliding hour, whatever the pod UIDs. With reserve the slot is taken in
// the same step as the check, under the device's lock, so concurrent exit manifests cannot all pass before any is recorded; the response
// hook returns it when zot refuses the manifest. It returns the time of the reserved slot.
func (g *LabGate) exitAllowed(lp labPath, reserve bool) (refusal, time.Time, bool) {
	n := g.Limits.ExitPushesPerHour
	if n <= 0 {
		n = 6
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.stateLocked(lp.Repo)
	kept := s.exitPushes[:0]
	for _, at := range s.exitPushes {
		if now.Sub(at) < time.Hour {
			kept = append(kept, at)
		}
	}
	s.exitPushes = kept
	if len(kept) >= n {
		return refusal{http.StatusTooManyRequests, CodeExitLimit, "this device has used its exit snapshots for the hour", kept[0].Add(time.Hour).Sub(now)}, time.Time{}, false
	}
	if reserve {
		s.exitPushes = append(s.exitPushes, now)
		return refusal{}, now, true
	}
	return refusal{}, time.Time{}, true
}

func (g *LabGate) releaseExitSlot(escaped string, slot time.Time) {
	lp, ok := parseLabPath(escaped)
	if !ok {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.stateLocked(lp.Repo)
	for i, at := range s.exitPushes {
		if at.Equal(slot) {
			s.exitPushes = append(s.exitPushes[:i], s.exitPushes[i+1:]...)
			return
		}
	}
}

// checkSession: an upload session continues only for the node and repository that started it.
func (g *LabGate) checkSession(lp labPath, p Principal) (refusal, bool) {
	g.mu.Lock()
	s, ok := g.sess[lp.Ref]
	if ok && lp.Ref != "" && s.node == p.Node && s.repo == lp.Repo {
		s.lastUsed = g.now()
		g.sess[lp.Ref] = s
	}
	g.mu.Unlock()
	if !ok || lp.Ref == "" || s.node != p.Node || s.repo != lp.Repo {
		return refusal{http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "unknown upload session", 0}, false
	}
	return refusal{}, true
}

func (g *LabGate) tenantBytes(ctx context.Context, tenant, except string) (map[string]int64, int64, error) {
	blobs, err := g.Meter.TenantBlobs(ctx, tenant, except)
	if err != nil {
		return nil, 0, err
	}
	var sum int64
	for _, n := range blobs {
		sum += n
	}
	return blobs, sum, nil
}

// quotaStillOpen refuses the start of an upload when the tenant is already at or over its quota (the exact check is at the manifest).
func (g *LabGate) quotaStillOpen(ctx context.Context, dev DeviceInfo, lp labPath) (refusal, bool) {
	q := g.quotaOf(dev)
	if q <= 0 || g.Meter == nil {
		return refusal{}, true
	}
	_, sum, err := g.tenantBytes(ctx, dev.Tenant, lp.Repo)
	if err != nil {
		return unavailable, false
	}
	if sum >= q {
		return refusal{http.StatusForbidden, CodeTenantQuota, "the registry quota of the tenant is used up", 0}, false
	}
	return refusal{}, true
}

// exitPod finds the device pod on the calling node that has ended (terminating, Succeeded or Failed), judged from the pod the gate sees and
// never from what the node says. A push while one exists is an exit push: held to its own limits (exitAllowed, poolLimit), not to the
// interval and the window budget. A participant can crash the pod for a new UID, so the limits are per device, never per pod.
func (g *LabGate) exitPod(lp labPath, p Principal) bool {
	for _, pod := range g.Schedule.PodsOnNode(p.Node) {
		if pod.Namespace == lp.NS && pod.Labels[names.LabelLab] == lp.Lab && pod.Labels[names.LabelDevice] == lp.Dev && pod.Ended() {
			return true
		}
	}
	return false
}

// authorizeManifestPut is the snapshot itself: the min interval (not for an exit snapshot), the registry reserve, and the tenant quota with
// the size of what the manifest adds.
func (g *LabGate) authorizeManifestPut(r *http.Request, lp labPath, p Principal, dev DeviceInfo) (refusal, bool) {
	if g.registryFull() {
		return fullRefusal, false
	}
	now := g.now()
	exitPush := g.exitPod(lp, p)
	if exitPush {
		f, slot, ok := g.exitAllowed(lp, true)
		if !ok {
			return f, false
		}
		if br, isRef := r.Context().Value(bodyKey).(*bodyRef); isRef {
			br.exitSlot = slot
		}
	}
	if !exitPush && g.Limits.MinInterval > 0 {
		g.mu.Lock()
		last := g.stateLocked(lp.Repo).lastManifest
		g.mu.Unlock()
		if !last.IsZero() && now.Sub(last) < g.Limits.MinInterval {
			return refusal{http.StatusTooManyRequests, CodePushInterval, "a snapshot of this device was accepted a moment ago", g.Limits.MinInterval - now.Sub(last)}, false
		}
	}
	if q := g.quotaOf(dev); q > 0 && g.Meter != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxManifestSize+1))
		if err != nil || len(body) > maxManifestSize {
			return refusal{http.StatusBadRequest, "MANIFEST_INVALID", "the manifest cannot be read", 0}, false
		}
		r.Body = io.NopCloser(strings.NewReader(string(body))) // the same bytes go on to zot
		r.ContentLength = int64(len(body))
		layers, ok := manifestBlobs(body)
		if !ok {
			return refusal{http.StatusBadRequest, "MANIFEST_INVALID", "the manifest cannot be read", 0}, false
		}
		have, sum, err := g.tenantBytes(r.Context(), dev.Tenant, lp.Repo)
		if err != nil {
			return unavailable, false
		}
		if len(layers) > maxManifestBlobs {
			return refusal{http.StatusBadRequest, "MANIFEST_INVALID", "the manifest lists too many blobs", 0}, false
		}
		for d := range layers {
			if _, counted := have[d]; counted {
				continue
			}
			// the size the registry holds, never the one the node wrote into the manifest
			n, ok, err := g.blobSize(r.Context(), lp.Repo, d)
			if err != nil {
				return unavailable, false
			}
			if !ok {
				return refusal{http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", "a blob of the manifest is not in the registry", 0}, false
			}
			sum += n
		}
		if sum > q {
			return refusal{http.StatusForbidden, CodeTenantQuota, "the registry quota of the tenant would be passed", 0}, false
		}
	}
	// what "latest" is now: when zot accepts the new manifest this one is superseded
	if g.Store != nil {
		if h, ok, err := g.Store.Head(r.Context(), lp.Repo, kindManifests, tagLatest); err == nil && ok {
			g.mu.Lock()
			g.stateLocked(lp.Repo).prev = h.Get("Docker-Content-Digest")
			g.mu.Unlock()
		}
	}
	return refusal{}, true
}

// afterZot reads zot's answer for what the gate must remember: the session of a new upload, a finished upload, an accepted snapshot.
func (g *LabGate) afterZot(resp *http.Response) {
	req := resp.Request
	if br, ok := req.Context().Value(bodyKey).(*bodyRef); ok && !br.exitSlot.IsZero() && resp.StatusCode != http.StatusCreated {
		g.releaseExitSlot(req.URL.EscapedPath(), br.exitSlot) // the manifest was refused: its slot is free again
	}
	lp, ok := parseLabPath(req.URL.EscapedPath())
	if !ok {
		return
	}
	switch {
	case lp.Kind == kindUploads && req.Method == http.MethodPost && resp.StatusCode == http.StatusAccepted:
		if id := uploadID(resp.Header.Get("Location")); id != "" {
			now := g.now()
			g.mu.Lock()
			evicted := g.admitSessionLocked(id, uploadSession{node: principalNode(req), repo: lp.Repo, started: now, lastUsed: now})
			g.mu.Unlock()
			for _, e := range evicted {
				go g.cancelInZot(e.repo, e.id)
			}
		}
	case lp.Kind == kindUploads && lp.Ref != "" && (resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusNotFound):
		g.mu.Lock()
		delete(g.sess, lp.Ref) // the session is over
		g.mu.Unlock()
	case lp.Kind == kindManifests && req.Method == http.MethodPut && lp.Ref == tagLatest && resp.StatusCode == http.StatusCreated:
		g.snapshotAccepted(req, lp, resp.Header.Get("Docker-Content-Digest"))
	}
}

type ctxKey int

const nodeKey ctxKey = 1

// WithNode carries the node of the authenticated principal to the response hook.
func WithNode(r *http.Request, node string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), nodeKey, node))
}

func principalNode(r *http.Request) string {
	n, _ := r.Context().Value(nodeKey).(string)
	return n
}

// evictedSession is a session that made room for a new one.
type evictedSession struct{ repo, id string }

// admitSessionLocked records a session. The table is bounded per device (UploadsPerDevice, 4) and per node (UploadsPerNode, 64), and what
// is evicted to make room is always the OLDEST-USED session of the same owner (the same device, the same node), which is cancelled in zot:
// a crashed push leaves abandoned sessions behind, and they must never make the next push, an exit snapshot above all, fail. One node can
// never push another node's sessions out.
func (g *LabGate) admitSessionLocked(id string, s uploadSession) []evictedSession {
	perDev, perNode := g.Limits.UploadsPerDevice, g.Limits.UploadsPerNode
	if perDev <= 0 {
		perDev = 4
	}
	if perNode <= 0 {
		perNode = 64
	}
	var out []evictedSession
	evict := func(match func(uploadSession) bool, limit int) {
		for {
			n := 0
			var oldID string
			var oldAt time.Time
			for k, o := range g.sess {
				if !match(o) {
					continue
				}
				n++
				if oldID == "" || o.lastUsed.Before(oldAt) {
					oldID, oldAt = k, o.lastUsed
				}
			}
			if n < limit {
				return
			}
			out = append(out, evictedSession{g.sess[oldID].repo, oldID})
			delete(g.sess, oldID)
		}
	}
	evict(func(o uploadSession) bool { return o.repo == s.repo }, perDev)
	evict(func(o uploadSession) bool { return o.node == s.node }, perNode)
	if len(g.sess) < maxSessions {
		g.sess[id] = s
	}
	return out
}

// SweepSessions forgets (and cancels in zot) the sessions nobody has touched for idle, so abandoned uploads do not sit in the table for an hour.
func (g *LabGate) SweepSessions(idle time.Duration) int {
	cutoff := g.now().Add(-idle)
	var gone []evictedSession
	g.mu.Lock()
	for id, s := range g.sess {
		if s.lastUsed.Before(cutoff) {
			gone = append(gone, evictedSession{s.repo, id})
			delete(g.sess, id)
		}
	}
	g.mu.Unlock()
	for _, e := range gone {
		g.cancelInZot(e.repo, e.id)
	}
	return len(gone)
}

// Run sweeps abandoned sessions every minute (idle for 10 minutes) until ctx ends.
func (g *LabGate) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.SweepSessions(10 * time.Minute)
		}
	}
}

func uploadID(location string) string {
	u, err := url.Parse(location)
	if err != nil {
		return ""
	}
	i := strings.LastIndex(u.Path, "/uploads/")
	if i < 0 {
		return ""
	}
	id := u.Path[i+len("/uploads/"):]
	if id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}

// snapshotAccepted records an accepted snapshot: its time (the min interval), the exit of a pod when this was one, and the plan to delete
// what it replaced.
func (g *LabGate) snapshotAccepted(req *http.Request, lp labPath, digest string) {
	now := g.now()
	isExit := g.exitPod(lp, Principal{Node: principalNode(req)})
	g.mu.Lock()
	s := g.stateLocked(lp.Repo)
	s.lastManifest = now
	if isExit {
		if br, ok := req.Context().Value(bodyKey).(*bodyRef); !ok || br.exitSlot.IsZero() {
			s.exitPushes = append(s.exitPushes, now)
		}
		kept := s.charges[:0] // the next exit push starts with its whole ceiling
		for _, c := range s.charges {
			if !c.exit {
				kept = append(kept, c)
			}
		}
		s.charges = kept
	}
	prev := s.prev
	s.prev = ""
	g.mu.Unlock()
	if g.Super != nil && prev != "" {
		g.Super.Plan(lp.Repo, prev, digest)
	}
	g.metric("snapshot_accepted")
}

// ForgetDevice drops what the gate remembers of a device repository (the device is gone): its budget and its sessions.
func (g *LabGate) ForgetDevice(repo string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.state, repo)
	for id, s := range g.sess {
		if s.repo == repo {
			delete(g.sess, id)
		}
	}
}

// grantedRefs are the references (and their images) a pod of this very device on the calling node names, bound to the device's tenant: the
// segment of the image must be the device tenant's own, the pod's namespace must belong to that tenant, and "platform" is never a device
// image. Each is a candidate for a mount or an exempt upload.
func (g *LabGate) deviceImages(lp labPath, p Principal, dev DeviceInfo, includeEnded bool) []imageRef {
	if g.Cache == nil || g.Cache.Grants == nil || g.Schedule == nil {
		return nil
	}
	seg := names.TenantSegment(dev.Tenant)
	var out []imageRef
	for _, pod := range g.Schedule.PodsOnNode(p.Node) {
		if (pod.Ended() && !includeEnded) || pod.Namespace != lp.NS || pod.Labels[names.LabelLab] != lp.Lab || pod.Labels[names.LabelDevice] != lp.Dev {
			continue
		}
		for _, img := range pod.Images {
			ref, ok := g.Cache.parseImage(img)
			if !ok || ref.Seg != seg || ref.Seg == names.SegmentPlatform || !g.Cache.podOfTenant(pod, ref.Seg, dev.Tenant) {
				continue
			}
			out = append(out, ref)
		}
	}
	return out
}

func (r imageRef) pinned() string {
	if r.Digest != "" {
		return r.Digest
	}
	if r.Tag != "" {
		return r.Tag
	}
	return tagLatest
}

// mountSource finds the cache repository (c/<registry>/<repo>) a blob may be mounted from into the snapshot repository of this device:
// the image of that very device on the calling node whose live, proven grant covers the digest (the pod's own pinned digest or tag, and
// what its manifests list, never a digest granted for another image of the repository). For an exit push the image of the device's
// just-ended pod counts too, with the same tenant and coverage checks: without it every exit snapshot would upload its base layers again
// against the exit ceiling. It also returns the image and the descriptor size the grant knows for the digest.
func (g *LabGate) mountSource(lp labPath, p Principal, dev DeviceInfo, digest string, exit bool) (from string, size int64, sized, ok bool) {
	for _, ref := range g.deviceImages(lp, p, dev, exit) {
		if g.Cache.Grants.Covers(p.Node, ref.Seg, ref.fullRepo(), ref.pinned(), digest) {
			n, has := g.Cache.Grants.Size(p.Node, ref.Seg, ref.fullRepo(), ref.pinned(), digest)
			return "c/" + ref.Registry + "/" + ref.Repo, n, has, true
		}
	}
	return "", 0, false, false
}

// blobSize is the size of a blob as the registry holds it.
func (g *LabGate) blobSize(ctx context.Context, repo, digest string) (int64, bool, error) {
	if g.Store == nil {
		return 0, false, errors.New("no registry client")
	}
	h, ok, err := g.Store.Head(ctx, repo, kindBlobs, digest)
	if err != nil || !ok {
		return 0, ok, err
	}
	n, err := strconv.ParseInt(h.Get("Content-Length"), 10, 64)
	if err != nil || n < 0 {
		return 0, false, errors.New("the registry gave no blob size")
	}
	return n, true, nil
}

// dropSession ends the upload session lp.Ref if it is the calling node's own for this repository, and cancels it in zot. It reports
// whether there was one.
func (g *LabGate) dropSession(lp labPath, p Principal) bool {
	g.mu.Lock()
	sess, ok := g.sess[lp.Ref]
	if ok && sess.node == p.Node && sess.repo == lp.Repo {
		delete(g.sess, lp.Ref)
	} else {
		ok = false
	}
	g.mu.Unlock()
	if ok {
		g.cancelInZot(lp.Repo, lp.Ref)
	}
	return ok
}

// cancelInZot tells zot to forget an upload session (best effort, with the gate's own account).
func (g *LabGate) cancelInZot(repo, id string) {
	if g.zot == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := *g.zot
	u.Path = "/v2/" + repo + "/blobs/uploads/" + id
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
	if err != nil {
		return
	}
	req.SetBasicAuth(g.user, g.pass)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}
