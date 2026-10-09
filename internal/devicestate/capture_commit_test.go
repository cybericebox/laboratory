package devicestate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type committingCluster struct {
	*fakeCluster
	commitMu  sync.Mutex
	requested bool
	stale     bool
	ackErr    error
	acks      []api.DeviceCaptureResult
	beforeAck func()
}

func (c *committingCluster) CaptureCommitRequested(_ context.Context, _ PodInfo, result api.DeviceCaptureResult) (bool, error) {
	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	if c.stale {
		return false, ErrStale
	}
	return c.requested && result.Result == "Succeeded", nil
}
func (c *committingCluster) AcknowledgeCaptureCommit(_ context.Context, _ PodInfo, result api.DeviceCaptureResult) error {
	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	if c.beforeAck != nil {
		c.beforeAck()
	}
	if c.ackErr != nil {
		return c.ackErr
	}
	c.acks = append(c.acks, result)
	return nil
}
func waitCommitted(t *testing.T, c *committingCluster) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for {
		c.commitMu.Lock()
		n := len(c.acks)
		c.commitMu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(until) {
			t.Fatal("node-agent never acknowledged durable committed hold")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestRequiredCaptureCommitPersistsBeforeACKAndOutlivesDeadline(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	req.DeadlineSeconds = 1
	r.rt.setDiff(tarOf(nil))
	cl := &committingCluster{fakeCluster: r.cl}
	r.e.Cluster = cl
	if _, err := r.e.CaptureRequired(context.Background(), r.pod, req); err != nil {
		t.Fatal(err)
	}
	cl.beforeAck = func() {
		raw, err := os.ReadFile(filepath.Join(r.e.WorkDir, "capture-holds", "c1.json"))
		if err != nil {
			t.Error(err)
			return
		}
		var h requiredHold
		if err := json.Unmarshal(raw, &h); err != nil || !h.Result.Committed {
			t.Error("commit API ACK preceded durable journal", err)
		}
	}
	cl.commitMu.Lock()
	cl.requested = true
	cl.commitMu.Unlock()
	waitCommitted(t, cl)
	time.Sleep(1100 * time.Millisecond)
	r.rt.mu.Lock()
	held := r.rt.held
	r.rt.gone = true
	r.rt.mu.Unlock()
	if !held {
		t.Fatal("committed live task thawed merely because capture deadline expired")
	}
}
func TestRequiredCaptureCommitAPIOutageKeepsDurableHoldThenACKs(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	req.DeadlineSeconds = 1
	r.rt.setDiff(tarOf(nil))
	cl := &committingCluster{fakeCluster: r.cl, requested: true, ackErr: errors.New("API unavailable")}
	r.e.Cluster = cl
	if _, err := r.e.CaptureRequired(context.Background(), r.pod, req); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	r.rt.mu.Lock()
	held := r.rt.held
	r.rt.mu.Unlock()
	if !held {
		t.Fatal("unpublished durable commit thawed during API outage")
	}
	cl.commitMu.Lock()
	if len(cl.acks) != 0 {
		t.Fatal("outage falsely ACKed")
	}
	cl.ackErr = nil
	cl.commitMu.Unlock()
	waitCommitted(t, cl)
	r.rt.mu.Lock()
	r.rt.gone = true
	r.rt.mu.Unlock()
}
func TestRequiredCaptureCommittedRestartPreservesLiveHold(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	cl := &committingCluster{fakeCluster: r.cl, requested: true}
	r.e.Cluster = cl
	c, err := r.rt.Inspect(context.Background(), r.pod.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	result := captureResult(req, "prior-boot")
	result.Result = "Succeeded"
	result.Quiesced = true
	result.Committed = true
	result.Image = c.ImageRef
	h := &requiredHold{Container: c, Pod: r.pod, Result: result}
	if err := r.e.writeHold(h); err != nil {
		t.Fatal(err)
	}
	r.rt.mu.Lock()
	r.rt.held = true
	r.rt.mu.Unlock()
	r.cl.guard = result
	r.e.NodeAgentEpoch = "new-boot"
	protected, err := r.e.RecoverCaptureHolds(context.Background())
	if err != nil || !protected[c.Cgroup] {
		t.Fatal("committed recovered hold lost ownership", protected, err)
	}
	time.Sleep(300 * time.Millisecond)
	r.rt.mu.Lock()
	held := r.rt.held
	r.rt.gone = true
	r.rt.mu.Unlock()
	if !held {
		t.Fatal("healthy restart thawed committed live task")
	}
}
func TestCaptureCommitKubeRequiresExactBootAndCurrentHeldGuard(t *testing.T) {
	k, c, p, req := captureKubeRig(t)
	ctx := context.Background()
	if err := k.SetCaptureGuard(ctx, p, req, "boot"); err != nil {
		t.Fatal(err)
	}
	result := captureResult(req, "boot")
	result.Result = "Succeeded"
	result.Quiesced = true
	result.Image = "base"
	if err := k.RecordCapture(ctx, p, result); err != nil {
		t.Fatal(err)
	}
	var d api.Device
	if err := c.Get(ctx, p.Device, &d); err != nil {
		t.Fatal(err)
	}
	d.Spec.State.CaptureRequest.CommitNodeAgentEpoch = "wrong-boot"
	if err := c.Update(ctx, &d); err != nil {
		t.Fatal(err)
	}
	if wanted, err := k.CaptureCommitRequested(ctx, p, result); err != nil || wanted {
		t.Fatal("foreign boot commit request accepted", wanted, err)
	}
	d.Spec.State.CaptureRequest.CommitNodeAgentEpoch = "boot"
	if err := c.Update(ctx, &d); err != nil {
		t.Fatal(err)
	}
	if wanted, err := k.CaptureCommitRequested(ctx, p, result); err != nil || !wanted {
		t.Fatal("current exact held commit refused", wanted, err)
	}
	result.Committed = true
	if err := k.AcknowledgeCaptureCommit(ctx, p, result); err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: p.Device.Namespace, Name: p.Pod}, &pod); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, p.Device, &d); err != nil {
		t.Fatal(err)
	}
	var guard api.DeviceCaptureResult
	_ = json.Unmarshal([]byte(pod.Annotations[CaptureGuardAnnotation]), &guard)
	if d.Status.State.Capture == nil || !d.Status.State.Capture.Committed || !guard.Committed {
		t.Fatal("commit acknowledgement did not update exact Pod guard and Device")
	}
}

func TestRequiredCaptureLateCommitAfterExpiredAPIOutageCannotACK(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	req.DeadlineSeconds = 1
	r.rt.setDiff(tarOf(nil))
	cl := &committingCluster{fakeCluster: r.cl}
	r.e.Cluster = cl
	if _, err := r.e.CaptureRequired(context.Background(), r.pod, req); err != nil {
		t.Fatal(err)
	}
	r.cl.mu.Lock()
	r.cl.checkFailures = 20
	r.cl.mu.Unlock()
	time.Sleep(1200 * time.Millisecond)
	cl.commitMu.Lock()
	cl.requested = true
	cl.commitMu.Unlock()
	r.cl.mu.Lock()
	r.cl.checkFailures = 0
	r.cl.mu.Unlock()
	until := time.Now().Add(2 * time.Second)
	for {
		r.rt.mu.Lock()
		held := r.rt.held
		r.rt.mu.Unlock()
		if !held {
			break
		}
		if time.Now().After(until) {
			t.Fatal("expired precommit request was promoted to committed hold")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cl.commitMu.Lock()
	defer cl.commitMu.Unlock()
	if len(cl.acks) != 0 {
		t.Fatal("expired precommit capture ACKed durable commit")
	}
}

func TestRequiredCaptureCommittedHoldCancelsOnlyAfterAPIInvalidation(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	r.rt.setDiff(tarOf(nil))
	cl := &committingCluster{fakeCluster: r.cl, requested: true}
	r.e.Cluster = cl
	if _, err := r.e.CaptureRequired(context.Background(), r.pod, req); err != nil {
		t.Fatal(err)
	}
	waitCommitted(t, cl)
	// A newer explicit intent makes the old capture stale; guard invalidation must
	// still precede thaw, and a failed API fence retains the committed hold.
	r.cl.mu.Lock()
	r.cl.invalidateErr = errors.New("API outage")
	r.cl.mu.Unlock()
	cl.commitMu.Lock()
	cl.requested = false
	cl.commitMu.Unlock()
	// The fake exposes the same ErrStale observation returned for a newer intent.
	cl.commitMu.Lock()
	cl.stale = true
	cl.commitMu.Unlock()
	time.Sleep(300 * time.Millisecond)
	r.rt.mu.Lock()
	held := r.rt.held
	r.rt.mu.Unlock()
	if !held {
		t.Fatal("committed hold thawed before API fence")
	}
	r.cl.mu.Lock()
	r.cl.onInvalidate = func() {
		r.rt.mu.Lock()
		defer r.rt.mu.Unlock()
		if !r.rt.held {
			t.Error("thaw preceded API invalidation")
		}
	}
	r.cl.invalidateErr = nil
	r.cl.mu.Unlock()
	until := time.Now().Add(2 * time.Second)
	for {
		r.rt.mu.Lock()
		held := r.rt.held
		r.rt.mu.Unlock()
		if !held {
			break
		}
		if time.Now().After(until) {
			t.Fatal("newer fenced intent did not release committed hold")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
