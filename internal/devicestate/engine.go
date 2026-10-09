package devicestate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagecache"
	"github.com/cybericebox/laboratory/internal/snapshot"
)

// ErrStale is returned by Cluster writes for a pod that is no longer the
// current one of its device (reset, or a newer incarnation): its state is obsolete.
var ErrStale = errors.New("pod is not the current incarnation of its device")

const (
	defaultResync      = 5 * time.Second
	defaultExitTimeout = 25 * time.Second
	defaultLiveTimeout = 3 * time.Minute
	// maxWaitFactor: a layer written without pause is snapshotted at the latest
	// after this many debounce periods.
	maxWaitFactor = 6
	minMaxWait    = 30 * time.Second
	// A failed snapshot is retried by itself: after defaultRetryBase, then twice as late
	// each time, at most defaultRetryMax apart.
	defaultRetryBase = 10 * time.Second
	defaultRetryMax  = 5 * time.Minute
)

// retryDelay is the wait before retry number n (1-based) of a failed snapshot.
func retryDelay(base, max time.Duration, n int) time.Duration {
	d := base
	for i := 1; i < n && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// Engine follows the snapshot-backed device containers of one node.
type Engine struct {
	Runtime        Runtime
	NodeAgentEpoch string
	Cluster        Cluster
	Pusher         Pusher
	// RegistryHost is host:port under which this node reaches the registry; an
	// image reference starting with it is a snapshot image.
	RegistryHost string
	// WorkDir holds the temporary layer files of a snapshot.
	WorkDir string
	// MaxWatchDirs is how many directories of one writable layer are watched with inotify (the kernel's watch limit is shared
	// by everything on the node); past it the layer is only polled. Zero means DefaultMaxWatchDirs.
	MaxWatchDirs int
	// Poll is the writable-layer scan interval (DefaultPollInterval when zero).
	Poll time.Duration
	// Resync is how often the pods are re-listed (5s when zero).
	Resync time.Duration
	// ExitTimeout bounds the exit snapshot so it ends before the controller
	// stops waiting for it (25s when zero).
	ExitTimeout time.Duration
	// RetryBase and RetryMax bound the retry of a failed snapshot (10s doubling to 5m when zero).
	RetryBase, RetryMax time.Duration

	// The registry is shared by every tenant, so what a device may push is bounded (zero values switch the bound off):
	// MinPushInterval is the least time between two pushes of one device (the exit snapshot is exempt: it must not be lost),
	// PushBudget the most state it may push within PushBudgetWindow (an hour when zero). A change that comes sooner or over the
	// budget waits; nothing is lost, the next snapshot has it.
	MinPushInterval  time.Duration
	PushBudget       int64
	PushBudgetWindow time.Duration
	// SupersededGrace is how long the manifest a new snapshot replaced is kept before it and its blobs are deleted from the
	// registry (a pod being created from it a moment ago must still be able to pull it). Zero deletes at once.
	SupersededGrace time.Duration
	// Space refuses a push when the registry is nearly full; Retained adds the state the tenant still has in repositories of labs that
	// are gone to the tenant's quota. Both are optional.
	Space    SpaceGuard
	Retained RetainedCounter
	Log      logr.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	captureWorkers sync.WaitGroup
	holdWorkers    sync.WaitGroup
	mu             sync.Mutex
	tracked        map[string]*tracked // by container id
	warned         map[string]bool
}

// tracked is one followed container.
type tracked struct {
	e   *Engine
	pod PodInfo
	c   Container

	cancel context.CancelFunc

	mu                sync.Mutex // one snapshot at a time
	lastPublishedDiff string     // only a successfully recorded digest; lastDiff also remembers legacy refusals
	lastDiff          string     // digest of the last layer published or refused
	pushed            bool       // a snapshot of this run was published
	lastWarn          string
	exited            bool
	required          *requiredHold
	// Scheduling only, guarded by e.mu. This is never exit/hold authority.
	pendingCapture *api.DeviceCaptureRequest
	captureAttempt *api.DeviceCaptureRequest
	retry          chan struct{}
	watching       bool // a normal persistence watcher exists, rather than a recovery placeholder
	lastSnapshot   Snapshot
	failures       int // consecutive failed live snapshots (touched by the live loop only)
	// prevRef is the manifest this device currently has in the registry (the one it was restored from, or its last snapshot); a
	// new snapshot supersedes it. pushes are the recent ones, for the rate and the budget.
	prevRef string
	pushes  []pushRecord
	// holdUntil: no live snapshot before it (the layer was over the write quota, a diff that is bound to fail again).
	holdUntil time.Time
}

// quotaHold is how long a layer over the write quota waits before the device is diffed again.
const quotaHold = 5 * time.Minute

type pushRecord struct {
	at    time.Time
	bytes int64
}

// errDeferred is a snapshot that was not taken now, for a reason that is not a failure: it waits for after. A deferred snapshot is
// retried then and does not count as a failed one.
type errDeferred struct {
	after  time.Duration
	reason string
}

func (d *errDeferred) Error() string { return "snapshot deferred: " + d.reason }

// allow says whether the device may push now; when it may not, the time to wait. Called with t.mu held.
func (t *tracked) allow() *errDeferred {
	e := t.e
	now := e.now()
	if now.Before(t.holdUntil) {
		return &errDeferred{after: t.holdUntil.Sub(now), reason: "the layer is over the write quota"}
	}
	if e.MinPushInterval > 0 && len(t.pushes) > 0 {
		last := t.pushes[len(t.pushes)-1].at
		if wait := e.MinPushInterval - now.Sub(last); wait > 0 {
			return &errDeferred{after: wait, reason: "the device pushed a snapshot a moment ago"}
		}
	}
	if e.PushBudget > 0 {
		window := e.PushBudgetWindow
		if window <= 0 {
			window = time.Hour
		}
		var sum int64
		kept := t.pushes[:0]
		for _, p := range t.pushes {
			if now.Sub(p.at) < window {
				kept = append(kept, p)
				sum += p.bytes
			}
		}
		t.pushes = kept
		if sum >= e.PushBudget && len(kept) > 0 {
			wait := window - now.Sub(kept[0].at)
			if wait < time.Second {
				wait = time.Second
			}
			return &errDeferred{after: wait, reason: fmt.Sprintf("the device has pushed %d bytes in the last %s, its budget is %d", sum, window, e.PushBudget)}
		}
	}
	return nil
}

// snapshotRef is ref when it names a snapshot of this registry (what a device restored from), "" for any other image.
func (e *Engine) snapshotRef(ref string) string {
	if e.RegistryHost != "" && strings.HasPrefix(ref, e.RegistryHost+"/") && strings.Contains(ref, "@") {
		return ref
	}
	return ""
}

// repoOfRef is "repo" of "host/repo@digest".
func repoOfRef(ref string) string {
	_, rest, _ := strings.Cut(ref, "/")
	repo, _, _ := strings.Cut(rest, "@")
	return repo
}

func (e *Engine) retryBase() time.Duration {
	if e.RetryBase > 0 {
		return e.RetryBase
	}
	return defaultRetryBase
}

func (e *Engine) retryMax() time.Duration {
	if e.RetryMax > 0 {
		return e.RetryMax
	}
	return defaultRetryMax
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Run follows the node until ctx ends.
func (e *Engine) Run(ctx context.Context) error {
	e.mu.Lock()
	if e.tracked == nil {
		e.tracked = map[string]*tracked{}
	}
	e.mu.Unlock()

	exits, err := e.Runtime.Exits(ctx)
	if err != nil {
		e.Log.Error(err, "subscribe to container exits; exit snapshots rely on the pod resync")
	}
	resync := e.Resync
	if resync <= 0 {
		resync = defaultResync
	}
	tick := time.NewTicker(resync)
	defer tick.Stop()
	e.Sync(ctx)
	for {
		select {
		case <-ctx.Done():
			e.stopAll()
			e.captureWorkers.Wait()
			e.holdWorkers.Wait()
			return nil
		case id, ok := <-exits:
			if !ok {
				exits = nil
				continue
			}
			go e.onExit(ctx, id)
		case <-tick.C:
			e.Sync(ctx)
		}
	}
}

func (e *Engine) stopAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, t := range e.tracked {
		t.cancel()
		delete(e.tracked, id)
	}
}

// Sync aligns the followed containers with the pods of the node: starts
// following new running ones, drops the gone, and finishes the exit snapshot of
// pods that ended without one (the container exited while nobody was watching).
func (e *Engine) Sync(ctx context.Context) {
	pods, err := e.Cluster.Pods(ctx)
	if err != nil {
		e.Log.Error(err, "list device pods")
		return
	}
	live := map[string]bool{}
	for _, p := range pods {
		if p.Epoch != p.DeviceEpoch {
			continue
		}
		live[p.ContainerID] = true
		var pending *api.DeviceCaptureRequest
		if p.CaptureRequest != nil && e.currentCaptureRequest(p, *p.CaptureRequest) {
			pending = p.CaptureRequest
		}
		e.setPendingCapture(p.ContainerID, pending)
		if p.CaptureRequest != nil {
			req := *p.CaptureRequest
			e.captureWorkers.Add(1)
			go func() {
				defer e.captureWorkers.Done()
				if _, err := e.captureRequired(ctx, p, req, false); err != nil {
					e.Log.Error(err, "required capture", "pod", p.Pod)
				}
			}()
			continue
		}
		if p.ContainerID == "" {
			continue
		}
		switch {
		case p.Running && !p.Ended:
			e.ensureTracked(ctx, p)
		case p.Ended && !p.ExitDone:
			e.endOf(ctx, p)
		}
	}
	e.mu.Lock()
	for id, t := range e.tracked {
		if !live[id] {
			t.cancel()
			delete(e.tracked, id)
		}
	}
	e.mu.Unlock()
}

func (e *Engine) setPendingCapture(id string, req *api.DeviceCaptureRequest) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if t := e.tracked[id]; t != nil {
		wasPending := t.pendingCapture != nil || t.captureAttempt != nil
		t.pendingCapture = req
		if req == nil {
			t.captureAttempt = nil // Fresh API observation supersedes an old attempt.
		}
		if wasPending && req == nil && t.retry != nil {
			select {
			case t.retry <- struct{}{}:
			default:
			}
		}
	}
}

func (e *Engine) setCaptureAttempt(id string, req *api.DeviceCaptureRequest) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if t := e.tracked[id]; t != nil {
		t.captureAttempt = req
	}
}

func (e *Engine) clearPendingCapture(id string, req *api.DeviceCaptureRequest) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if t := e.tracked[id]; t != nil && t.captureAttempt == req {
		t.captureAttempt = nil
		if t.pendingCapture == nil && t.retry != nil {
			select {
			case t.retry <- struct{}{}:
			default:
			}
		}
	}
}

func (e *Engine) ensureTracked(ctx context.Context, p PodInfo) {
	e.mu.Lock()
	if t, ok := e.tracked[p.ContainerID]; ok {
		e.mu.Unlock()
		t.startWatching(ctx)
		return
	}
	e.mu.Unlock()
	c, err := e.Runtime.Inspect(ctx, p.ContainerID)
	if err != nil {
		e.Log.Error(err, "inspect device container", "pod", p.Pod, "container", p.ContainerID)
		e.warnOnce(ctx, p, "state persistence unavailable: "+err.Error())
		return
	}
	t := e.track(ctx, p, c)
	if t == nil {
		return
	}
	e.Log.Info("following device", "device", p.Device, "pod", p.Pod, "container", p.ContainerID, "upper", c.UpperDir)
}

// warnOnce reports a warning for a pod that cannot be followed, once per message.
func (e *Engine) warnOnce(ctx context.Context, p PodInfo, msg string) {
	key := p.Pod + "\x00" + msg
	e.mu.Lock()
	if e.warned == nil {
		e.warned = map[string]bool{}
	}
	seen := e.warned[key]
	e.warned[key] = true
	e.mu.Unlock()
	if seen {
		return
	}
	if err := e.Cluster.Warn(ctx, p, msg); err != nil && !errors.Is(err, ErrStale) {
		e.Log.Error(err, "report warning", "pod", p.Pod)
	}
}

// track registers the container and starts its change watcher.
func (e *Engine) track(ctx context.Context, p PodInfo, c Container) *tracked {
	t := &tracked{e: e, pod: trackingPodInfo(p), c: c, cancel: func() {}, prevRef: e.snapshotRef(c.ImageRef)}
	e.mu.Lock()
	if old, ok := e.tracked[p.ContainerID]; ok {
		t = old
	} else {
		e.tracked[p.ContainerID] = t
	}
	e.mu.Unlock()
	if !t.startWatching(ctx) {
		return nil
	}
	return t
}

// startWatching attaches the existing persistence pipeline to a released capture
// placeholder without replacing its identity. An active capture owns t.mu or
// t.required, so a normal Sync neither waits on it nor starts a second watcher.
func (t *tracked) startWatching(ctx context.Context) bool {
	if !t.mu.TryLock() {
		return true
	}
	defer t.mu.Unlock()
	if t.watching || t.required != nil || t.exited {
		return true
	}
	e, p, c := t.e, t.pod, t.c
	wctx, cancel := context.WithCancel(ctx)
	changes, err := Watch(wctx, c.UpperDir, p.Policy, e.Poll, e.MaxWatchDirs)
	if err != nil {
		e.Log.Error(err, "watch writable layer", "upper", c.UpperDir)
		cancel()
		return false
	}
	e.mu.Lock()
	t.cancel = cancel
	e.mu.Unlock()
	t.watching = true

	// One snapshot right away: a layer that changed while nobody watched (the
	// node-agent restarted) must not wait for the next change.
	in := make(chan struct{}, 1)
	in <- struct{}{}
	retry := make(chan struct{}, 1)
	e.mu.Lock()
	t.retry = retry
	e.mu.Unlock()
	go func() {
		defer close(in)
		for {
			select {
			case <-wctx.Done():
				return
			case <-retry:
				select {
				case in <- struct{}{}:
				default:
				}
			case _, ok := <-changes:
				if !ok {
					return
				}
				select {
				case in <- struct{}{}:
				default:
				}
			}
		}
	}()
	maxWait := time.Duration(maxWaitFactor) * p.Policy.Debounce
	if maxWait < minMaxWait {
		maxWait = minMaxWait
	}
	go Debounce(wctx, in, p.Policy.Debounce, maxWait, func() {
		lctx, lcancel := context.WithTimeout(wctx, defaultLiveTimeout)
		defer lcancel()
		err := t.snapshot(lctx, true)
		later := func(d time.Duration) {
			go func() {
				select {
				case <-time.After(d):
					select {
					case retry <- struct{}{}:
					default:
					}
				case <-wctx.Done():
				}
			}()
		}
		var def *errDeferred
		switch {
		case err == nil:
			t.failures = 0
		case errors.As(err, &def):
			// Not a failure: the registry is shared and this device has had its share for now.
			e.Log.Info("snapshot deferred", "device", p.Device, "pod", p.Pod, "reason", def.reason, "after", def.after.String())
			later(def.after)
		case errors.Is(err, context.Canceled) || errors.Is(err, ErrStale):
		default:
			e.Log.Error(err, "snapshot", "device", p.Device, "pod", p.Pod)
			t.failures++
			later(retryDelay(e.retryBase(), e.retryMax(), t.failures))
		}
	})
	return true
}

// onExit handles a runtime exit event.
func (e *Engine) onExit(ctx context.Context, containerID string) {
	e.mu.Lock()
	t := e.tracked[containerID]
	e.mu.Unlock()
	if t == nil {
		return // not a followed container, or not seen yet: Sync picks the pod up when it ends
	}
	e.finish(ctx, t)
}

// endOf takes the exit snapshot of a pod that ended while it was not followed.
func (e *Engine) endOf(ctx context.Context, p PodInfo) {
	e.mu.Lock()
	t := e.tracked[p.ContainerID]
	e.mu.Unlock()
	if t == nil {
		c, err := e.Runtime.Inspect(ctx, p.ContainerID)
		if err != nil {
			// The container is gone (removed before we got here): nothing to snapshot.
			e.Log.Info("ended device container not found; giving up its exit snapshot", "pod", p.Pod, "err", err.Error())
			e.markExit(ctx, p)
			return
		}
		t = &tracked{e: e, pod: trackingPodInfo(p), c: c, cancel: func() {}, prevRef: e.snapshotRef(c.ImageRef)}
		e.mu.Lock()
		e.tracked[p.ContainerID] = t
		e.mu.Unlock()
	}
	e.finish(ctx, t)
}

// finish takes the exit snapshot of a container: its processes are gone, so
// nothing is frozen, and the layer on disk is final.
func (e *Engine) finish(ctx context.Context, t *tracked) {
	t.mu.Lock()
	// CaptureRequired holds this same lock through capture, and t.required owns
	// any quiescence retained afterward. A copied API request is not a live hold.
	if t.exited || t.required != nil {
		t.mu.Unlock()
		return
	}
	t.exited = true
	t.mu.Unlock()
	t.cancel() // no more live snapshots

	timeout := e.ExitTimeout
	if timeout <= 0 {
		timeout = defaultExitTimeout
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := t.snapshot(sctx, false); err != nil {
		e.Log.Error(err, "exit snapshot", "device", t.pod.Device, "pod", t.pod.Pod)
	}
	e.markExit(ctx, t.pod)
}

func (e *Engine) markExit(ctx context.Context, p PodInfo) {
	if err := e.Cluster.MarkExit(ctx, p); err != nil && !errors.Is(err, ErrStale) {
		e.Log.Error(err, "mark exit snapshot", "pod", p.Pod)
	}
}

// snapshot publishes the state of the writable layer. freeze is true for a
// running container and false for one that exited. A failed or refused snapshot
// keeps the last good one and reports a warning.
//
//nolint:gocyclo // one decision over many cases; splitting it would scatter the rule
func (t *tracked) snapshot(ctx context.Context, freeze bool) (err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.required != nil {
		return nil
	}
	if freeze {
		t.e.mu.Lock()
		pending := t.pendingCapture != nil || t.captureAttempt != nil
		t.e.mu.Unlock()
		if pending {
			return nil
		}
	}
	return t.snapshotLocked(ctx, freeze, false)
}

func (t *tracked) snapshotLocked(ctx context.Context, freeze, required bool) (err error) {
	if t.exited && freeze {
		return nil
	}
	e, pol := t.e, t.pod.Policy
	started := e.now()
	var diffTook, pushTook time.Duration
	if freeze {
		if d := t.allow(); d != nil {
			return d
		}
	}

	defer func() {
		var def *errDeferred
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrStale) && !errors.As(err, &def) {
			t.warn(ctx, "snapshot failed: "+err.Error())
		}
	}()

	rc, err := e.Runtime.Diff(ctx, t.c, freeze, pol)
	diffTook = e.now().Sub(started)
	if err != nil {
		return fmt.Errorf("diff writable layer: %w", err)
	}
	dir, err := os.MkdirTemp(e.WorkDir, "snap-")
	if err != nil {
		_ = rc.Close()
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	layerPath := filepath.Join(dir, "layer.tar")
	f, err := os.Create(layerPath)
	if err != nil {
		_ = rc.Close()
		return err
	}
	sum := sha256.New()
	stats, ferr := snapshot.FilterLayerMapped(rc, io.MultiWriter(f, sum), pol, t.c.IDs)
	if cerr := rc.Close(); ferr == nil {
		ferr = cerr
	}
	if cerr := f.Close(); ferr == nil {
		ferr = cerr
	}
	if errors.Is(ferr, snapshot.ErrQuota) {
		// Over the write quota before the layer was even copied (a sparse file counts by its apparent size): the last good snapshot
		// stays, and the same change is not diffed again for a while.
		t.holdUntil = e.now().Add(quotaHold)
		t.warn(ctx, ferr.Error())
		if required {
			return ferr
		}
		return nil
	}
	if errors.Is(ferr, snapshot.ErrEntries) {
		// Too many files: the last good snapshot stays; the next change is tried again (the layer only grows, so it will
		// most likely be refused again, but the warning is cleared by a snapshot that fits).
		t.warn(ctx, ferr.Error())
		if required {
			return ferr
		}
		return nil
	}
	if ferr != nil {
		return fmt.Errorf("filter layer: %w", ferr)
	}
	if required && (stats.Unmapped > 0 || stats.SkippedTotal > 0 || stats.RefusedEntries > 0) {
		return fmt.Errorf("required capture omits file data or ownership")
	}
	if stats.Unmapped > 0 {
		e.Log.Info("owner ids outside the user namespace map were written as 0", "device", t.pod.Device, "pod", t.pod.Pod, "ids", stats.Unmapped)
	}
	digest := hex.EncodeToString(sum.Sum(nil))

	// Files over the size limit are left out of the layer; the status names them.
	skipMsg := stats.SkippedWarning(pol.MaxFileSize)
	if stats.Entries == 0 {
		if t.pushed && !required {
			if err := t.publishStart(ctx); err != nil {
				return err
			}
		}
		t.lastSnapshot = Snapshot{Image: t.c.ImageRef, At: e.now()}
		if required {
			t.lastPublishedDiff = ""
			t.lastDiff = ""
			t.pushed = false
		}
		if required && e.snapshotRef(t.c.ImageRef) != "" {
			run, err := e.Runtime.LoadImage(ctx, t.c.ImageRef)
			if err != nil {
				return err
			}
			chain, err := snapshot.ChainOf(run)
			if err != nil {
				return err
			}
			t.lastSnapshot.SizeBytes = chain.Bytes()
			t.lastSnapshot.Layers = int32(chain.Layers())
		}
		t.warnSkipped(ctx, skipMsg)
		return nil // nothing (else) changed since the device started
	}
	if !required && digest == t.lastDiff || required && t.pushed && digest == t.lastPublishedDiff {
		return nil
	}

	run, err := e.Runtime.LoadImage(ctx, t.c.ImageRef)
	if err != nil {
		return fmt.Errorf("load image %s: %w", t.c.ImageRef, err)
	}
	img, chain, err := snapshot.Build(run, layerPath, stats.Bytes, pol, dir)
	if errors.Is(err, snapshot.ErrQuota) || errors.Is(err, snapshot.ErrEntries) {
		if required {
			return err
		}
		t.lastDiff = digest // do not retry the same layer
		t.warn(ctx, err.Error())
		return nil
	}
	if err != nil {
		return err
	}
	// The registry is shared by every tenant: all the snapshots of one tenant together stay under its quota.
	if t.pod.TenantQuota > 0 {
		others, qerr := e.Cluster.TenantBytes(ctx, t.pod.Tenant, t.pod.Device)
		if qerr != nil {
			return fmt.Errorf("tenant registry usage: %w", qerr)
		}
		// What the tenant still has in the repositories of labs that are gone counts too (it takes the volume until the retention ends).
		if e.Retained != nil {
			if live, lerr := e.Cluster.LiveRepos(ctx); lerr == nil {
				if ret, rerr := e.Retained.RetainedBytes(ctx, t.pod.Tenant, live); rerr == nil {
					others += ret
				} else if required {
					return rerr
				}
			} else if required {
				return lerr
			}
		}
		if others+chain.Bytes() > t.pod.TenantQuota {
			if required {
				return fmt.Errorf("%w: tenant quota exceeded", snapshot.ErrQuota)
			}
			t.lastDiff = digest
			t.warn(ctx, fmt.Sprintf("%v: the snapshots of the tenant would take %d bytes of the registry, the tenant's quota is %d", snapshot.ErrQuota, others+chain.Bytes(), t.pod.TenantQuota))
			return nil
		}
	}
	if required {
		if d := t.allow(); d != nil {
			return d
		}
	}
	// The registry volume is shared by every tenant and the platform: no push while it is nearly full.
	if e.Space != nil {
		if serr := e.Space.Check(ctx, chain.Bytes()); serr != nil {
			if required {
				return serr
			}
			t.warn(ctx, serr.Error())
			if freeze {
				return &errDeferred{after: 5 * time.Minute, reason: serr.Error()}
			}
			return nil // the exit snapshot is not retried: the last good one stays
		}
	}
	img = snapshot.Annotated(img, t.pod.Tenant, chain.Bytes())
	pushStart := e.now()
	ref, _, err := e.Pusher.Push(ctx, t.pod.Repo, img, chain.Base, imagecache.Rewriter{Prefix: e.RegistryHost}.RepoOf(t.c.ImageRef))
	if err != nil {
		return fmt.Errorf("push snapshot: %w", err)
	}
	pushTook = e.now().Sub(pushStart)
	if !required {
		if err := e.Cluster.Record(ctx, t.pod, Snapshot{Image: ref, At: e.now(), SizeBytes: chain.Bytes(), Layers: int32(chain.Layers())}); err != nil {
			return err
		}
	}
	t.lastSnapshot = Snapshot{Image: ref, At: e.now(), SizeBytes: chain.Bytes(), Layers: int32(chain.Layers())}
	t.lastPublishedDiff = digest
	t.lastDiff, t.pushed, t.lastWarn = digest, true, ""
	t.pushes = append(t.pushes, pushRecord{at: e.now(), bytes: chain.Bytes()})
	if !required {
		t.supersede(ref, img)
	}
	t.warnSkipped(ctx, skipMsg)
	e.Log.Info("snapshot taken", "device", t.pod.Device, "pod", t.pod.Pod, "frozen", freeze,
		"diff", diffTook.String(), "push", pushTook.String(), "total", e.now().Sub(started).String(),
		"layers", chain.Layers(), "bytes", chain.Bytes(), "image", ref)
	return nil
}

// supersede deletes the manifest the new snapshot replaced (and the blobs only it used) from the registry, after the grace period.
func (t *tracked) supersede(newRef string, keep v1.Image) {
	old := t.prevRef
	t.prevRef = newRef
	e := t.e
	s, ok := e.Pusher.(Superseder)
	if !ok || old == "" || old == newRef || repoOfRef(old) != t.pod.Repo {
		return
	}
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.Supersede(ctx, t.pod.Repo, old, keep); err != nil {
			e.Log.Error(err, "delete the superseded snapshot", "device", t.pod.Device, "ref", old)
		}
	}
	if e.SupersededGrace <= 0 {
		run()
		return
	}
	time.AfterFunc(e.SupersededGrace, run)
}

// publishStart records that the writable layer is back to what the container
// started from: the snapshot it started from if that was one, else the base image.
func (t *tracked) publishStart(ctx context.Context) error {
	s := Snapshot{At: t.e.now()}
	if t.e.RegistryHost != "" && strings.HasPrefix(t.c.ImageRef, t.e.RegistryHost+"/") {
		run, err := t.e.Runtime.LoadImage(ctx, t.c.ImageRef)
		if err != nil {
			return fmt.Errorf("load image %s: %w", t.c.ImageRef, err)
		}
		chain, err := snapshot.ChainOf(run)
		if err != nil {
			return err
		}
		s.Image, s.SizeBytes, s.Layers = t.c.ImageRef, chain.Bytes(), int32(chain.Layers())
	}
	if err := t.e.Cluster.Record(ctx, t.pod, s); err != nil {
		return err
	}
	t.lastPublishedDiff = ""
	t.lastDiff, t.pushed, t.lastWarn = "", false, ""
	return nil
}

// warnSkipped reports the files skipped for size, if any.
func (t *tracked) warnSkipped(ctx context.Context, msg string) {
	if msg != "" {
		t.warn(ctx, msg)
	}
}

// warn reports a warning once per distinct message.
func (t *tracked) warn(ctx context.Context, msg string) {
	if msg == t.lastWarn {
		return
	}
	t.lastWarn = msg
	// The snapshot's own context may already be over (timeout); the warning still goes out.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := t.e.Cluster.Warn(wctx, t.pod, msg); err != nil && !errors.Is(err, ErrStale) {
		t.e.Log.Error(err, "report snapshot warning", "device", t.pod.Device)
	}
}

// trackingPodInfo stores stable persistence metadata, not ephemeral operator
// intent. Sync handles current requests and requiredHold owns active quiescence.
func trackingPodInfo(p PodInfo) PodInfo {
	p.CaptureRequest = nil
	return p
}
