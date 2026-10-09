package devicestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const CaptureGuardAnnotation = "laboratory.cybericebox.com/capture-guard"

var ErrDeleting = errors.New("capture Pod deletion has been accepted; quiescence must remain held")

type requiredHold struct {
	Result    api.DeviceCaptureResult
	Container Container
	Pod       PodInfo
	thaw      func() error
	journal   string
}

func captureResult(p PodInfo, req api.DeviceCaptureRequest, boot string) api.DeviceCaptureResult {
	return api.DeviceCaptureResult{OperationID: req.OperationID, LifecycleRevision: req.LifecycleRevision, PodUID: req.PodUID, PodResourceVersion: req.PodResourceVersion, Epoch: req.Epoch, Incarnation: req.Incarnation, NodeAgentEpoch: boot, Result: "Pending", GuardState: "Held"}
}

func (e *Engine) currentCaptureRequest(p PodInfo, req api.DeviceCaptureRequest) bool {
	return e.NodeAgentEpoch != "" && req.OperationID != "" && req.LifecycleRevision > 0 && req.PodUID != "" && req.PodResourceVersion != "" && req.DeadlineSeconds >= 1 && req.DeadlineSeconds <= 3600 && p.UID == req.PodUID && p.Epoch == req.Epoch && p.DeviceEpoch == req.Epoch && p.Incarnation == req.Incarnation && p.ContainerID != "" && p.Running && !p.Ended && !p.Deleting
}

// CaptureRequired explicitly captures a live incarnation and keeps it frozen until
// actual task death or API-fenced invalidation. It never uses ExitSnapshotPod.
func (e *Engine) CaptureRequired(ctx context.Context, p PodInfo, req api.DeviceCaptureRequest) (result api.DeviceCaptureResult, err error) {
	return e.captureRequired(ctx, p, req, true)
}

// Sync owns the API-observed scheduling marker. A delayed Sync worker cannot
// recreate it after a newer Sync cleared the request or started another intent.
func (e *Engine) captureRequired(ctx context.Context, p PodInfo, req api.DeviceCaptureRequest, direct bool) (result api.DeviceCaptureResult, err error) {
	result = captureResult(p, req, e.NodeAgentEpoch)
	fail := func(err error) (api.DeviceCaptureResult, error) {
		result.Result = "Failed"
		result.GuardState = "Invalidated"
		result.Error = err.Error()
		result.Quiesced = false
		result.Image = ""
		result.SnapshotAt = nil
		result.SizeBytes = 0
		return result, err
	}
	if !e.currentCaptureRequest(p, req) {
		return fail(ErrStale)
	}
	rt, ok := e.Runtime.(RequiredRuntime)
	if !ok {
		return fail(ErrNoFreezer)
	}
	cl, ok := e.Cluster.(CaptureCluster)
	if !ok {
		return fail(errors.New("required capture API unavailable"))
	}
	if direct {
		e.setCaptureAttempt(p.ContainerID, &req)
		defer e.clearPendingCapture(p.ContainerID, &req)
	}
	sctx, cancel := context.WithTimeout(ctx, time.Duration(req.DeadlineSeconds)*time.Second)
	defer cancel()
	deadline, _ := sctx.Deadline()
	c, err := e.Runtime.Inspect(sctx, p.ContainerID)
	if err != nil {
		result, _ = fail(err)
		failureCtx, failureCancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = cl.RecordCapture(failureCtx, p, result)
		failureCancel()
		return result, err
	}
	if !c.OwnershipKnown {
		result, _ = fail(errors.New("required capture OCI ownership mappings unavailable"))
		failureCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = cl.RecordCapture(failureCtx, p, result)
		return result, errors.New(result.Error)
	}
	e.mu.Lock()
	if e.tracked == nil {
		e.tracked = map[string]*tracked{}
	}
	t := e.tracked[p.ContainerID]
	if t == nil {
		t = &tracked{e: e, pod: trackingPodInfo(p), c: c, cancel: func() {}, prevRef: e.snapshotRef(c.ImageRef)}
		if direct {
			t.captureAttempt = &req
		}
		e.tracked[p.ContainerID] = t
	}
	e.mu.Unlock()
	for !t.mu.TryLock() {
		select {
		case <-sctx.Done():
			return fail(sctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	defer t.mu.Unlock()
	if t.required != nil {
		h := t.required
		if h.Result.OperationID == req.OperationID && h.Result.LifecycleRevision == req.LifecycleRevision && h.Result.NodeAgentEpoch == e.NodeAgentEpoch && h.Result.Result == "Succeeded" {
			return h.Result, nil
		}
		return fail(errors.New("prior capture is still held"))
	}
	// A live push can finish during lifecycle preparation, before Sync observes
	// this request. Reserve its remaining interval under the same mutex as live
	// snapshots, but do not freeze or publish a hold until the wait is over.
	if err = t.waitPushInterval(sctx); err != nil {
		result, _ = fail(err)
		failureCtx, failureCancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = cl.RecordCapture(failureCtx, p, result)
		failureCancel()
		return result, err
	}
	// Strict capture always uses the successfully inspected current OCI metadata.
	t.c = c
	t.pod = trackingPodInfo(p)
	h := &requiredHold{Result: result, Container: c, Pod: p}
	previousDiff, previousPublishedDiff, previousPushed, previousSnapshot := t.lastDiff, t.lastPublishedDiff, t.pushed, t.lastSnapshot
	// Persist before freezing, so even a force-deleted Pod cannot erase crash recovery's hold identity.
	if err = e.writeHold(h); err != nil {
		return fail(err)
	}
	if err = cl.SetCaptureGuard(sctx, p, req, e.NodeAgentEpoch); err != nil {
		_ = os.Remove(h.journal)
		return fail(err)
	}
	alive, aliveErr := rt.TaskAlive(sctx, c)
	if aliveErr != nil {
		err = aliveErr
	} else if !alive {
		err = errors.New("required capture task is not alive")
	} else {
		h.thaw, err = rt.Quiesce(sctx, c)
	}
	if err == nil {
		result.Quiesced = true
		h.Result = result
		err = t.snapshotLocked(sctx, false, true)
	}
	if err == nil {
		err = sctx.Err()
	}
	if err == nil {
		result.Result = "Succeeded"
		result.Image = t.lastSnapshot.Image
		if result.Image == "" {
			result.Image = c.ImageRef
		}
		at := metav1.NewTime(e.now())
		result.SnapshotAt = &at
		result.ObservedAt = &at
		result.SizeBytes = t.lastSnapshot.SizeBytes
		h.Result = result
		p.capturedLayers = t.lastSnapshot.Layers
		err = cl.RecordCapture(sctx, p, result)
	}
	if err != nil {
		result, _ = fail(err)
		t.lastDiff, t.lastPublishedDiff, t.pushed, t.lastSnapshot = previousDiff, previousPublishedDiff, previousPushed, previousSnapshot
		h.Result = result
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		invErr := cl.InvalidateCapture(cleanup, p, result)
		if invErr == nil {
			if h.thaw != nil {
				if thawErr := h.thaw(); thawErr != nil {
					t.required = h
					e.holdWorkers.Add(1)
					go e.watchHold(ctx, t, h, rt, cl, time.Now())
					return result, errors.Join(err, thawErr)
				}
			}
			_ = os.Remove(h.journal)
			return result, err
		}
		t.required = h
		e.holdWorkers.Add(1)
		go e.watchHold(ctx, t, h, rt, cl, time.Now())
		return result, errors.Join(err, invErr)
	}
	t.required = h
	e.holdWorkers.Add(1)
	go e.watchHold(ctx, t, h, rt, cl, deadline)
	return result, nil
}

// waitPushInterval holds t.mu; no ordinary push can renew the interval while a
// Required request waits. All other push refusals remain in snapshotLocked.
func (t *tracked) waitPushInterval(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if t.e.MinPushInterval <= 0 || len(t.pushes) == 0 {
			return nil
		}
		last := t.pushes[len(t.pushes)-1].at
		wait := t.e.MinPushInterval - t.e.now().Sub(last)
		if wait <= 0 {
			return nil
		}
		if deadline, ok := ctx.Deadline(); ok && wait >= time.Until(deadline) {
			// Empty or unchanged Required layers need no push. Preserve that
			// decision in the frozen diff; an actual push still fails allow().
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (e *Engine) writeHold(h *requiredHold) error {
	if err := os.MkdirAll(filepath.Join(e.WorkDir, "capture-holds"), 0700); err != nil {
		return err
	}
	if h.Container.ID == "" || filepath.Base(h.Container.ID) != h.Container.ID {
		return errors.New("invalid required hold container id")
	}
	h.journal = filepath.Join(e.WorkDir, "capture-holds", h.Container.ID+".json")
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}

	f, err := os.CreateTemp(filepath.Dir(h.journal), ".hold-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, h.journal); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(h.journal))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()

}

// watchHold deliberately survives engine shutdown: cancellation requires an API
// fence, whereas an accepted deletion waits for SIGKILL/task death without thaw.
func (e *Engine) watchHold(parent context.Context, t *tracked, h *requiredHold, rt RequiredRuntime, cl CaptureCluster, deadline time.Time) {
	defer e.holdWorkers.Done()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for range tick.C {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		alive, err := rt.TaskAlive(ctx, h.Container)
		if err == nil && !alive {
			cancel()
			t.mu.Lock()
			if t.required == h {
				t.required = nil
			}
			t.mu.Unlock()
			_ = os.Remove(h.journal)
			return
		}
		t.mu.Lock()
		result := h.Result
		t.mu.Unlock()
		deleting, checkErr := cl.CheckCapture(ctx, h.Pod, result)
		if err == nil && checkErr == nil && !deleting && result.Result == "Succeeded" && (result.Committed || !time.Now().After(deadline)) {
			if commits, ok := cl.(CaptureCommitCluster); ok {
				wanted, commitErr := commits.CaptureCommitRequested(ctx, h.Pod, result)
				if commitErr != nil {
					checkErr = commitErr
				} else if wanted {
					if !result.Committed {
						t.mu.Lock()
						previous := h.Result
						h.Result.Committed = true
						if journalErr := e.writeHold(h); journalErr != nil {
							h.Result = previous
							checkErr = ErrStale
						} else {
							result = h.Result
						}
						t.mu.Unlock()
					}
					if result.Committed {
						_ = commits.AcknowledgeCaptureCommit(ctx, h.Pod, result)
					}
				}
			}
		}
		if err == nil && (checkErr == nil || errors.Is(checkErr, ErrStale)) && !deleting && (!result.Committed && (time.Now().After(deadline) || result.Result == "Failed" || parent.Err() != nil) || errors.Is(checkErr, ErrStale)) {
			failed := result
			failed.Committed = false
			failed.Result = "Failed"
			failed.GuardState = "Invalidated"
			failed.Quiesced = false
			failed.Error = "required capture expired or was superseded"
			if cl.InvalidateCapture(ctx, h.Pod, failed) == nil {
				// A nil handle means Quiesce failed before requesting a freeze.
				if h.thaw != nil {
					if thawErr := h.thaw(); thawErr != nil {
						cancel()
						continue
					}
				}
				cancel()
				t.mu.Lock()
				if t.required == h {
					t.required = nil
				}
				t.mu.Unlock()
				_ = os.Remove(h.journal)
				// Keep the tracked identity; next Sync attaches ordinary persistence to
				// released placeholders, while existing watchers continue unchanged.
				return
			}
		}
		cancel()
	}
}

// RecoverCaptureHolds invalidates prior live acknowledgements before startup
// thaw and preserves deleting/missing Pod holds until actual runtime task death.
func (e *Engine) RecoverCaptureHolds(ctx context.Context) (map[string]bool, error) {
	cl, ok := e.Cluster.(CaptureCluster)
	if !ok {
		return nil, fmt.Errorf("capture recovery API unavailable")
	}
	rt, ok := e.Runtime.(RequiredRuntime)
	if !ok {
		return nil, ErrNoFreezer
	}
	var pods []PodInfo
	var err error
	if direct, ok := e.Cluster.(interface {
		CapturePods(context.Context) ([]PodInfo, error)
	}); ok {
		pods, err = direct.CapturePods(ctx)
	} else {
		pods, err = e.Cluster.Pods(ctx)
	}
	if err != nil {
		return nil, err
	}
	holds := map[string]*requiredHold{}
	entries, err := os.ReadDir(filepath.Join(e.WorkDir, "capture-holds"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(entries) > 10000 {
		return nil, errors.New("too many required hold journals")
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hold-") {
			if err := os.Remove(filepath.Join(e.WorkDir, "capture-holds", entry.Name())); err != nil {
				return nil, err
			}
			continue
		}
		if filepath.Ext(entry.Name()) != ".json" {
			return nil, errors.New("unknown required hold journal entry")
		}
		b, err := os.ReadFile(filepath.Join(e.WorkDir, "capture-holds", entry.Name()))
		if err != nil {
			return nil, err
		}
		h := &requiredHold{}
		if err = json.Unmarshal(b, h); err != nil {
			return nil, err
		}
		if h.Container.ID == "" || h.Container.Cgroup == "" || h.Pod.UID == "" || h.Result.PodUID != h.Pod.UID || h.Result.NodeAgentEpoch == "" {
			return nil, errors.New("incomplete required hold journal")
		}
		h.journal = filepath.Join(e.WorkDir, "capture-holds", entry.Name())
		holds[h.Container.ID] = h
	}
	for _, p := range pods {
		if p.Guard == "" {
			continue
		}
		h := &requiredHold{Pod: p}
		if err = json.Unmarshal([]byte(p.Guard), &h.Result); err != nil {
			return nil, err
		}
		c, err := e.Runtime.Inspect(ctx, p.ContainerID)
		if err != nil {
			return nil, err
		}
		h.Container = c
		if old := holds[c.ID]; old != nil {
			h.journal = old.journal
			if old.Result.Committed && sameCapture(old.Result, h.Result) {
				h.Result = old.Result
			}
		}
		holds[c.ID] = h
	}
	protected := map[string]bool{}
	for _, h := range holds {
		alive, err := rt.TaskAlive(ctx, h.Container)
		if err != nil {
			return nil, err
		}
		if !alive {
			_ = os.Remove(h.journal)
			continue
		}

		if h.Container.Cgroup == "" {
			return nil, errors.New("required hold has no recoverable cgroup")
		}
		h.thaw = func() error {
			thawCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return rt.Thaw(thawCtx, h.Container)
		}
		preserve := func() {
			protected[h.Container.Cgroup] = true
			e.mu.Lock()
			if e.tracked == nil {
				e.tracked = map[string]*tracked{}
			}
			t := &tracked{e: e, pod: trackingPodInfo(h.Pod), c: h.Container, cancel: func() {}, required: h}
			e.tracked[h.Container.ID] = t
			e.mu.Unlock()
			e.holdWorkers.Add(1)
			go e.watchHold(ctx, t, h, rt, cl, time.Now())
		}
		deleting, checkErr := cl.CheckCapture(ctx, h.Pod, h.Result)
		if checkErr != nil && !errors.Is(checkErr, ErrStale) || deleting || h.Result.Committed && checkErr == nil {
			preserve()
			continue
		}
		failed := h.Result
		failed.Result = "Failed"
		failed.GuardState = "Invalidated"
		failed.Quiesced = false
		failed.Committed = false
		failed.Error = "node-agent restarted"
		if err = cl.InvalidateCapture(ctx, h.Pod, failed); err != nil {
			if errors.Is(err, ErrDeleting) {
				preserve()
				continue
			}
			return nil, err
		}
		if err = h.thaw(); err != nil {
			preserve()
			continue
		}
		_ = os.Remove(h.journal)

	}
	return protected, nil
}
