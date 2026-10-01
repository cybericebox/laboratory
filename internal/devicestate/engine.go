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
)

// Engine follows the snapshot-backed device containers of one node.
type Engine struct {
	Runtime Runtime
	Cluster Cluster
	Pusher  Pusher
	// RegistryHost is host:port under which this node reaches the registry; an
	// image reference starting with it is a snapshot image.
	RegistryHost string
	// WorkDir holds the temporary layer files of a snapshot.
	WorkDir string
	// Poll is the writable-layer scan interval (DefaultPollInterval when zero).
	Poll time.Duration
	// Resync is how often the pods are re-listed (5s when zero).
	Resync time.Duration
	// ExitTimeout bounds the exit snapshot so it ends before the controller
	// stops waiting for it (25s when zero).
	ExitTimeout time.Duration
	Log         logr.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	tracked map[string]*tracked // by container id
	warned  map[string]bool
}

// tracked is one followed container.
type tracked struct {
	e   *Engine
	pod PodInfo
	c   Container

	cancel context.CancelFunc

	mu       sync.Mutex // one snapshot at a time
	lastDiff string     // digest of the last layer published or refused
	pushed   bool       // a snapshot of this run was published
	lastWarn string
	exited   bool
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
	e.tracked = map[string]*tracked{}
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
		if p.ContainerID == "" || p.Epoch != p.DeviceEpoch {
			continue
		}
		live[p.ContainerID] = true
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

func (e *Engine) ensureTracked(ctx context.Context, p PodInfo) {
	e.mu.Lock()
	if _, ok := e.tracked[p.ContainerID]; ok {
		e.mu.Unlock()
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
	wctx, cancel := context.WithCancel(ctx)
	t := &tracked{e: e, pod: p, c: c, cancel: cancel}
	e.mu.Lock()
	if old, ok := e.tracked[p.ContainerID]; ok {
		e.mu.Unlock()
		cancel()
		return old
	}
	e.tracked[p.ContainerID] = t
	e.mu.Unlock()

	changes, err := Watch(wctx, c.UpperDir, p.Policy, e.Poll)
	if err != nil {
		e.Log.Error(err, "watch writable layer", "upper", c.UpperDir)
		cancel()
		e.mu.Lock()
		delete(e.tracked, p.ContainerID)
		e.mu.Unlock()
		return nil
	}
	// One snapshot right away: a layer that changed while nobody watched (the
	// node-agent restarted) must not wait for the next change.
	in := make(chan struct{}, 1)
	in <- struct{}{}
	go func() {
		defer close(in)
		for {
			select {
			case <-wctx.Done():
				return
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
		if err := t.snapshot(lctx, true); err != nil && !errors.Is(err, context.Canceled) {
			e.Log.Error(err, "snapshot", "device", p.Device, "pod", p.Pod)
		}
	})
	return t
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
		t = &tracked{e: e, pod: p, c: c, cancel: func() {}}
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
	if t.exited {
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
func (t *tracked) snapshot(ctx context.Context, freeze bool) (err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exited && freeze {
		return nil
	}
	e, pol := t.e, t.pod.Policy
	started := e.now()
	var diffTook, pushTook time.Duration

	defer func() {
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrStale) {
			t.warn(ctx, "snapshot failed: "+err.Error())
		}
	}()

	rc, err := e.Runtime.Diff(ctx, t.c, freeze)
	diffTook = e.now().Sub(started)
	if err != nil {
		return fmt.Errorf("diff writable layer: %w", err)
	}
	dir, err := os.MkdirTemp(e.WorkDir, "snap-")
	if err != nil {
		rc.Close()
		return err
	}
	defer os.RemoveAll(dir)

	layerPath := filepath.Join(dir, "layer.tar")
	f, err := os.Create(layerPath)
	if err != nil {
		rc.Close()
		return err
	}
	sum := sha256.New()
	stats, ferr := snapshot.FilterLayer(rc, io.MultiWriter(f, sum), pol)
	rc.Close()
	if cerr := f.Close(); ferr == nil {
		ferr = cerr
	}
	if ferr != nil {
		return fmt.Errorf("filter layer: %w", ferr)
	}
	digest := hex.EncodeToString(sum.Sum(nil))

	// Files over the size limit are left out of the layer; the status names them.
	skipMsg := stats.SkippedWarning(pol.MaxFileSize)
	if stats.Entries == 0 {
		if t.pushed {
			if err := t.publishStart(ctx); err != nil {
				return err
			}
		}
		t.warnSkipped(ctx, skipMsg)
		return nil // nothing (else) changed since the device started
	}
	if digest == t.lastDiff {
		return nil
	}

	run, err := e.Runtime.LoadImage(ctx, t.c.ImageRef)
	if err != nil {
		return fmt.Errorf("load image %s: %w", t.c.ImageRef, err)
	}
	img, chain, err := snapshot.Build(run, layerPath, stats.Bytes, pol, dir)
	if errors.Is(err, snapshot.ErrQuota) {
		t.lastDiff = digest // do not retry the same layer
		t.warn(ctx, err.Error())
		return nil
	}
	if err != nil {
		return err
	}
	pushStart := e.now()
	ref, _, err := e.Pusher.Push(ctx, t.pod.Repo, img, chain.Base, imagecache.Rewriter{Prefix: e.RegistryHost}.RepoOf(t.c.ImageRef))
	if err != nil {
		return fmt.Errorf("push snapshot: %w", err)
	}
	pushTook = e.now().Sub(pushStart)
	if err := e.Cluster.Record(ctx, t.pod, Snapshot{Image: ref, At: e.now(), SizeBytes: chain.Bytes(), Layers: int32(chain.Layers())}); err != nil {
		return err
	}
	t.lastDiff, t.pushed, t.lastWarn = digest, true, ""
	t.warnSkipped(ctx, skipMsg)
	e.Log.Info("snapshot taken", "device", t.pod.Device, "pod", t.pod.Pod, "frozen", freeze,
		"diff", diffTook.String(), "push", pushTook.String(), "total", e.now().Sub(started).String(),
		"layers", chain.Layers(), "bytes", chain.Bytes(), "image", ref)
	return nil
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
