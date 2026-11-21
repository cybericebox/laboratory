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
	// MaxWatchDirs is how many directories of ALL the writable layers of the node are watched with inotify together (the kernel's watch
	// limit is shared by everything on the node, and one budget is shared by every device); past it a layer is only polled. Zero means
	// DefaultMaxWatchDirs.
	MaxWatchDirs int
	// Poll is the writable-layer scan interval (DefaultPollInterval when zero).
	Poll time.Duration
	// Resync is how often the pods are re-listed (5s when zero).
	Resync time.Duration
	// ExitTimeout bounds the exit snapshot so it ends before the controller
	// stops waiting for it (25s when zero).
	ExitTimeout time.Duration
	// MaxConcurrentDiffs is how many layers the node diffs at once (DefaultMaxConcurrentDiffs when zero); TempBudgetBytes is how many bytes
	// of temporary files in WorkDir all the running diffs may hold together (each reserves the write quota of its device; zero: no budget
	// beyond the number of diffs). PrescanTime caps the look at a layer before its diff (DefaultPrescanTime when zero).
	MaxConcurrentDiffs int
	TempBudgetBytes    int64
	PrescanTime        time.Duration
	// RetryBase and RetryMax bound the retry of a failed snapshot (10s doubling to 5m when zero).
	RetryBase, RetryMax time.Duration

	Log logr.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// SnapshotsBlocked, when set, is why no snapshot can be taken on this node (an AppArmor profile that denies mount): devices are
	// still followed, but each gets it as its warning and no snapshot is attempted.
	SnapshotsBlocked string

	watchOnce   sync.Once
	watchBudget *WatchBudget
	diffOnce    sync.Once
	diffs       *diffLimiter

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
	// holdUntil: no live snapshot before it (the layer was over the write quota, a diff that is bound to fail again).
	holdUntil time.Time
	// broken is why this device is not snapshotted any more in this incarnation: a goroutine of its snapshot machinery panicked (guarded by
	// e.mu). Another pod of the device, or a restart of the node-state pod, starts clean.
	broken string
}

// quotaHold is how long a layer over the write quota waits before the device is diffed again.
const quotaHold = 5 * time.Minute

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
		return &errDeferred{after: t.holdUntil.Sub(now), reason: "the layer is over the write quota or the entry limit"}
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

// errDeviceFailed is the answer of a device that stopped being snapshotted after a panic in its machinery (see Engine.panicked).
var errDeviceFailed = errors.New("snapshots of this device stopped after an internal error")

// goSafe runs fn in a goroutine of the engine for the container. A panic in it does not take the node-state pod down with the snapshots
// of every other device: it is logged with its stack and marks the device failed (panicked).
func (e *Engine) goSafe(name, containerID string, fn func()) {
	go safeGo(name, e.panicked(containerID), fn)
}

// panicked is the handler of a recovered panic of a goroutine that works for the container: the error and the stack are logged, and the
// device stops being snapshotted in this incarnation (its status says so); the engine goes on with every other device.
func (e *Engine) panicked(containerID string) func(error) {
	return func(err error) {
		var stack []byte
		var pe *PanicError
		if errors.As(err, &pe) {
			stack = pe.Stack
		}
		e.Log.Error(err, "recovered from a panic: the snapshots of this device stop for this incarnation", "container", containerID, "stack", string(stack))
		e.failDevice(containerID, err)
	}
}

// failDevice marks the tracked container broken and says so in the device status. A container the engine does not follow has nothing to mark.
func (e *Engine) failDevice(containerID string, cause error) {
	e.mu.Lock()
	t := e.tracked[containerID]
	if t != nil {
		t.broken = cause.Error()
	}
	e.mu.Unlock()
	if t == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg := snapshot.TruncateWarning("snapshots of this device are stopped after an internal error of the node: " + cause.Error())
	if err := e.Cluster.Warn(ctx, t.pod, msg); err != nil && !errors.Is(err, ErrStale) {
		e.Log.Error(err, "report the stopped device", "pod", t.pod.Pod)
	}
}

// failedWith is why the device is stopped, "" when it is not.
func (t *tracked) failedWith() string {
	t.e.mu.Lock()
	defer t.e.mu.Unlock()
	return t.broken
}

// diffGate is the node-wide limit on diffs, made on first use.
func (e *Engine) diffGate() *diffLimiter {
	e.diffOnce.Do(func() { e.diffs = newDiffLimiter(e.MaxConcurrentDiffs, e.TempBudgetBytes) })
	return e.diffs
}

// watchDirs is the node-wide budget of watched directories, made on first use from MaxWatchDirs.
func (e *Engine) watchDirs() *WatchBudget {
	e.watchOnce.Do(func() { e.watchBudget = NewWatchBudget(e.MaxWatchDirs) })
	return e.watchBudget
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
			e.goSafe("exit snapshot", id, func() { e.onExit(ctx, id) })
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
			e.goSafe("required capture", p.ContainerID, func() {
				defer e.captureWorkers.Done()
				if _, err := e.captureRequired(ctx, p, req, false); err != nil {
					e.Log.Error(err, "required capture", "pod", p.Pod)
				}
			})
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
	if e.SnapshotsBlocked != "" {
		// Shown at once, not only after the first write: the participant must not rely on state that is never saved.
		t.mu.Lock()
		t.warn(ctx, e.SnapshotsBlocked)
		t.mu.Unlock()
	}
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
	t := &tracked{e: e, pod: trackingPodInfo(p), c: c, cancel: func() {}}
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
	changes, err := WatchWith(wctx, c.UpperDir, p.Policy, WatchOptions{Poll: e.Poll, Budget: e.watchDirs(), OnPanic: e.panicked(c.ID), OnOverCap: func() {
		e.Log.Info("the writable layer is over the scan limits: it is followed by filesystem events only", "device", p.Device, "pod", p.Pod)
	}})
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
	e.goSafe("change forwarder", c.ID, func() {
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
	})
	maxWait := time.Duration(maxWaitFactor) * p.Policy.Debounce
	if maxWait < minMaxWait {
		maxWait = minMaxWait
	}
	e.goSafe("debounce", c.ID, func() {
		Debounce(wctx, in, p.Policy.Debounce, maxWait, func() {
			lctx, lcancel := context.WithTimeout(wctx, defaultLiveTimeout)
			defer lcancel()
			err := t.snapshot(lctx, true)
			later := func(d time.Duration) {
				e.goSafe("retry timer", c.ID, func() {
					select {
					case <-time.After(d):
						select {
						case retry <- struct{}{}:
						default:
						}
					case <-wctx.Done():
					}
				})
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
			case errors.Is(err, ErrSnapshotsBlocked), errors.Is(err, errDeviceFailed):
				// Logged once at startup and shown on the device; a retry would fail the same way.
			default:
				e.Log.Error(err, "snapshot", "device", p.Device, "pod", p.Pod)
				t.failures++
				later(retryDelay(e.retryBase(), e.retryMax(), t.failures))
			}
		})
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
		t = &tracked{e: e, pod: trackingPodInfo(p), c: c, cancel: func() {}}
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
	if err := t.exitSnapshot(ctx, sctx); err != nil && !errors.Is(err, ErrSnapshotsBlocked) {
		e.Log.Error(err, "exit snapshot", "device", t.pod.Device, "pod", t.pod.Pod)
	}
	e.markExit(ctx, t.pod)
}

// maxExitAttempts bounds how many times the exit snapshot is tried when the gate says to wait.
const maxExitAttempts = 4

// exitSnapshot takes the exit snapshot, and when the gate answers with a wait (429 or 503 with Retry-After, a restart code) it waits that
// long and tries again, a bounded number of times and never past the exit deadline (sctx): the pod is gone and nothing will ask again. When
// the gate's time is longer than what is left (the hourly limit of exit snapshots, a view that stays out of contact), or the attempts are
// used up, the exit state is given up and the lab status says that it was not saved.
func (t *tracked) exitSnapshot(ctx, sctx context.Context) error {
	for attempt := 1; ; attempt++ {
		err := t.snapshot(sctx, false)
		var def *errDeferred
		if !errors.As(err, &def) {
			return err
		}
		left := time.Duration(-1)
		if dl, ok := sctx.Deadline(); ok {
			left = time.Until(dl)
		}
		if attempt >= maxExitAttempts || (left >= 0 && def.after >= left) {
			t.mu.Lock()
			t.warn(ctx, snapshot.TruncateWarning("the state of the device at its exit was not saved: "+def.reason))
			t.mu.Unlock()
			return nil
		}
		t.e.Log.Info("exit snapshot deferred by the registry", "device", t.pod.Device, "pod", t.pod.Pod, "after", def.after.String(), "attempt", attempt)
		timer := time.NewTimer(def.after)
		select {
		case <-sctx.Done():
			timer.Stop()
			return sctx.Err()
		case <-timer.C:
		}
	}
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
	if t.e.SnapshotsBlocked != "" {
		t.warn(ctx, t.e.SnapshotsBlocked)
		return ErrSnapshotsBlocked
	}
	return t.snapshotLocked(ctx, freeze, false)
}

func (t *tracked) snapshotLocked(ctx context.Context, freeze, required bool) (err error) {
	if t.exited && freeze {
		return nil
	}
	e, pol := t.e, t.pod.Policy
	if t.failedWith() != "" {
		return errDeviceFailed
	}
	started := e.now()
	var pushTook time.Duration
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

	lf, stop, err := t.takeLayer(ctx, freeze, required, pol, started)
	if lf != nil {
		defer lf.cleanup()
	}
	if stop {
		return err
	}
	stats, digest, layerPath, dir, diffTook := lf.stats, lf.digest, lf.path, lf.dir, lf.diffTook

	// Files over the size limit are left out of the layer; the status names them.
	skipMsg := stats.SkippedWarning(pol.MaxFileSize)
	if stats.Entries == 0 {
		return t.snapshotEmptyLayer(ctx, required, skipMsg)
	}
	if t.unchangedSnapshotLayer(required, digest) {
		return nil
	}

	run, err := t.loadImage(ctx)
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
	if required {
		if d := t.allow(); d != nil {
			return d
		}
	}
	pushStart := e.now()
	ref, _, err := e.Pusher.Push(ctx, t.pod.Repo, img, chain.Base, imagecache.Rewriter{Prefix: e.RegistryHost}.RepoOf(t.c.ImageRef))
	if err != nil {
		if handled, herr := t.gateRefusal(ctx, err, required, freeze, digest); handled {
			return herr
		}
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
	t.warnSkipped(ctx, skipMsg)
	e.Log.Info("snapshot taken", "device", t.pod.Device, "pod", t.pod.Pod, "frozen", freeze,
		"diff", diffTook.String(), "push", pushTook.String(), "total", e.now().Sub(started).String(),
		"layers", chain.Layers(), "bytes", chain.Bytes(), "image", ref)
	return nil
}

// layerFile is a snapshot layer taken from a device: the filtered tar in a temporary directory, and what the node reserved for it.
type layerFile struct {
	stats     snapshot.Stats
	digest    string
	path, dir string
	diffTook  time.Duration
	ticket    *diffTicket
}

// cleanup removes the temporary files and gives the node's reservation back.
func (l *layerFile) cleanup() {
	_ = os.RemoveAll(l.dir)
	l.ticket.release()
}

// takeLayer looks at the writable layer, diffs it and filters the diff into a temporary file. The layer is looked at before containerd is
// asked to diff it: a layer over the limits is refused here, with the same errors and the same hold as when the filter refuses it after the
// diff, and costs the node a bounded scan instead of a diff of all of it. The diff and its temporary files are the node's shared resource,
// a few at a time (diffGate). stop says the caller returns err as it is (a refusal, a deferral or a failure), and then the returned layer,
// when not nil, is still to be cleaned up.
func (t *tracked) takeLayer(ctx context.Context, freeze, required bool, pol snapshot.Policy, started time.Time) (lf *layerFile, stop bool, err error) {
	e := t.e
	if refused, rerr := t.refuseFilteredLayer(ctx, required, prescanLayer(t.c.UpperDir, pol, e.PrescanTime)); refused {
		return nil, true, rerr
	}
	ticket, err := e.diffGate().acquire(ctx, reserveFor(pol), !freeze || required)
	if err != nil {
		return nil, true, err
	}
	rc, err := e.Runtime.Diff(ctx, t.c, freeze, pol)
	diffTook := e.now().Sub(started)
	if err != nil {
		ticket.release()
		return nil, true, fmt.Errorf("diff writable layer: %w", err)
	}
	dir, err := os.MkdirTemp(e.WorkDir, "snap-")
	if err != nil {
		_ = rc.Close()
		ticket.release()
		return nil, true, err
	}
	lf = &layerFile{dir: dir, path: filepath.Join(dir, "layer.tar"), diffTook: diffTook, ticket: ticket}
	var refused bool
	lf.stats, lf.digest, refused, err = t.filterSnapshotLayer(ctx, rc, lf.path, pol, required)
	ticket.done() // the layer is read and filtered; what follows (the push) does not hold a diff slot
	if refused {
		return lf, true, err
	}
	return lf, false, nil
}

func (t *tracked) loadImage(ctx context.Context) (v1.Image, error) {
	if runtime, ok := t.e.Runtime.(SourceImageRuntime); ok {
		return runtime.LoadImageForPod(ctx, t.c, t.pod)
	}
	return t.e.Runtime.LoadImage(ctx, t.c.ImageRef)
}

// publishStart records that the writable layer is back to what the container
// started from: the snapshot it started from if that was one, else the base image.
func (t *tracked) publishStart(ctx context.Context) error {
	s := Snapshot{At: t.e.now()}
	if t.e.RegistryHost != "" && strings.HasPrefix(t.c.ImageRef, t.e.RegistryHost+"/") {
		run, err := t.loadImage(ctx)
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

// refuseFilteredLayer preserves the ordinary snapshot's quota refusal and the
// Required capture's strict failure before checking any filtered metadata.
func (t *tracked) refuseFilteredLayer(ctx context.Context, required bool, ferr error) (bool, error) {
	if errors.Is(ferr, snapshot.ErrQuota) {
		// Over the write quota before the layer was even copied (a sparse file counts by its apparent size): the last good snapshot
		// stays, and the same change is not diffed again for a while.
		t.holdUntil = t.e.now().Add(quotaHold)
		t.warn(ctx, ferr.Error())
		if required {
			return true, ferr
		}
		return true, nil
	}
	if errors.Is(ferr, snapshot.ErrEntries) {
		// Too many files: the last good snapshot stays, and the same layer is not diffed again for a while (a layer that big is made
		// again at once by the next change; the warning is cleared by a snapshot that fits).
		t.holdUntil = t.e.now().Add(quotaHold)
		t.warn(ctx, ferr.Error())
		if required {
			return true, ferr
		}
		return true, nil
	}
	if ferr != nil {
		return true, fmt.Errorf("filter layer: %w", ferr)
	}
	return false, nil
}

func (t *tracked) snapshotEmptyLayer(ctx context.Context, required bool, skipMsg string) error {
	if t.pushed && !required {
		if err := t.publishStart(ctx); err != nil {
			return err
		}
	}
	t.lastSnapshot = Snapshot{Image: t.c.ImageRef, At: t.e.now()}
	if required {
		t.lastPublishedDiff = ""
		t.lastDiff = ""
		t.pushed = false
	}
	if required && t.e.snapshotRef(t.c.ImageRef) != "" {
		run, err := t.loadImage(ctx)
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

// filterSnapshotLayer closes both streams before applying the unchanged filter
// refusals and Required ownership/data completeness checks.
func (t *tracked) filterSnapshotLayer(ctx context.Context, rc io.ReadCloser, layerPath string, pol snapshot.Policy, required bool) (snapshot.Stats, string, bool, error) {
	f, err := os.Create(layerPath)
	if err != nil {
		_ = rc.Close()
		return snapshot.Stats{}, "", true, err
	}
	sum := sha256.New()
	stats, ferr := snapshot.FilterLayerMapped(rc, io.MultiWriter(f, sum), pol, t.c.IDs)
	if cerr := rc.Close(); ferr == nil {
		ferr = cerr
	}
	if cerr := f.Close(); ferr == nil {
		ferr = cerr
	}
	if refused, err := t.refuseFilteredLayer(ctx, required, ferr); refused {
		return snapshot.Stats{}, "", true, err
	}
	if required && (stats.Unmapped > 0 || stats.SkippedTotal > 0 || stats.RefusedEntries > 0) {
		return snapshot.Stats{}, "", true, fmt.Errorf("required capture omits file data or ownership")
	}
	if stats.Unmapped > 0 {
		t.e.Log.Info("owner ids outside the user namespace map were written as 0", "device", t.pod.Device, "pod", t.pod.Pod, "ids", stats.Unmapped)
	}
	digest := hex.EncodeToString(sum.Sum(nil))

	return stats, digest, false, nil
}

func (t *tracked) unchangedSnapshotLayer(required bool, digest string) bool {
	return !required && digest == t.lastDiff || required && t.pushed && digest == t.lastPublishedDiff
}
