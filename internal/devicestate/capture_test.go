package devicestate

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/snapshot"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func captureRequest(r *rig) api.DeviceCaptureRequest {
	r.pod.UID = "pod-u"
	r.pod.ResourceVersion = "1"
	r.e.NodeAgentEpoch = "boot-a"
	return api.DeviceCaptureRequest{OperationID: "op", LifecycleRevision: 1, PodUID: "pod-u", PodResourceVersion: "1", Incarnation: 1, DeadlineSeconds: 30}
}

// A required quota refusal must fail rather than inheriting live snapshot's nil refusal.
func TestRequiredCaptureQuotaNeverAcknowledgesSuccess(t *testing.T) {
	r := newRig(t, time.Hour, 1)
	req := captureRequest(r)
	r.rt.setDiff(tarOf(map[string]string{"work": "too large"}))
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if err == nil || result.Result != "Failed" || result.Image != "" {
		t.Fatalf("quota refusal: %+v %v", result, err)
	}
	if r.rt.held {
		t.Fatal("failed capture was not thawed after invalidation")
	}
	if _, _, exits := r.cl.snapshot(); len(exits) != 0 {
		t.Fatal("required failure emitted legacy exit acknowledgement")
	}
}

func TestRequiredCaptureUnchangedExplicitlyHoldsBase(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	r.rt.setDiff(tarOf(nil))
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if err != nil || result.Result != "Succeeded" || result.Image != "docker.io/library/app:1" || !result.Quiesced || result.GuardState != "Held" {
		t.Fatalf("unchanged capture: %+v %v", result, err)
	}
	if !r.rt.held {
		t.Fatal("successful capture thawed before stop")
	}
	r.rt.mu.Lock()
	r.rt.gone = true
	r.rt.mu.Unlock()
}

func TestRequiredCaptureFailuresRetainRuntime(t *testing.T) {
	for _, tc := range []string{"diff", "push", "file", "entries", "missing", "cancel", "incarnation"} {
		t.Run(tc, func(t *testing.T) {
			r := newRig(t, time.Hour, 1<<20)
			req := captureRequest(r)
			r.rt.setDiff(tarOf(map[string]string{"work": "changed"}))
			ctx := context.Background()
			switch tc {
			case "diff":
				r.rt.failDiff = 1
			case "push":
				r.e.Pusher = failingPusher{}
			case "file":
				r.pod.Policy.MaxFileSize = 1
			case "entries":
				r.pod.Policy.MaxEntries = 1
				r.rt.setDiff(tarOf(map[string]string{"a": "a", "b": "b"}))
			case "missing":
				r.rt.gone = true
			case "cancel":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			case "incarnation":
				req.Incarnation = 2
			}
			result, err := r.e.CaptureRequired(ctx, r.pod, req)
			if err == nil || result.Result == "Succeeded" {
				t.Fatalf("failure acknowledged: %+v %v", result, err)
			}
			recs, _, exits := r.cl.snapshot()
			if len(recs) != 0 || len(exits) != 0 {
				t.Fatalf("failure changed latest state/exit: %v %v", recs, exits)
			}
		})
	}
}

func TestRequiredCaptureInvalidationFailureNeverThaws(t *testing.T) {
	r := newRig(t, time.Hour, 1)
	req := captureRequest(r)
	r.rt.setDiff(tarOf(map[string]string{"x": "too large"}))
	r.cl.invalidateErr = errors.New("api unavailable")
	_, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if err == nil || !r.rt.held {
		t.Fatalf("API failure thawed capture: held=%v err=%v", r.rt.held, err)
	}
}

type failingPusher struct{}

func (failingPusher) Push(context.Context, string, v1.Image, int, string) (string, v1.Hash, error) {
	return "", v1.Hash{}, errors.New("push refused")
}

// Snapshot publishing must not permit writes between filesystem capture and upload.
func TestRequiredCaptureHoldsThroughPushAndRetainsSavedImage(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	r.rt.setDiff(tarOf(map[string]string{"work": "saved"}))
	r.e.Pusher = holdCheckingPusher{r: r}
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if err != nil || result.Result != "Succeeded" || result.SizeBytes != 5 || result.Image == "" {
		t.Fatalf("capture: %+v %v", result, err)
	}
	if r.cl.capture.Image != result.Image {
		t.Fatal("capture did not record the saved image")
	}
	recs, _, exits := r.cl.snapshot()
	if len(recs) != 0 || len(exits) != 0 {
		t.Fatal("required capture escaped its operation fence through legacy writes")
	}
}

type holdCheckingPusher struct{ r *rig }

func (p holdCheckingPusher) Push(ctx context.Context, repo string, img v1.Image, base int, source string) (string, v1.Hash, error) {
	p.r.rt.mu.Lock()
	held := p.r.rt.held
	p.r.rt.mu.Unlock()
	if !held {
		return "", v1.Hash{}, errors.New("runtime thawed before push")
	}
	return p.r.reg.Push(ctx, repo, img, base, source)
}

type waitingPusher struct{}

func (waitingPusher) Push(ctx context.Context, _ string, _ v1.Image, _ int, _ string) (string, v1.Hash, error) {
	<-ctx.Done()
	return "", v1.Hash{}, ctx.Err()
}
func TestRequiredCaptureTimeoutInvalidatesBeforeThaw(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	req.DeadlineSeconds = 1
	r.rt.setDiff(tarOf(map[string]string{"x": "x"}))
	r.e.Pusher = waitingPusher{}
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if !errors.Is(err, context.DeadlineExceeded) || result.Result != "Failed" || r.rt.held {
		t.Fatalf("timeout: %+v %v held=%v", result, err, r.rt.held)
	}
}

func TestRequiredCaptureAcceptedDeletionNeverThaws(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	req.DeadlineSeconds = 1
	r.rt.setDiff(tarOf(nil))
	r.cl.deleting = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := r.e.CaptureRequired(ctx, r.pod, req); err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(1200 * time.Millisecond)
	r.rt.mu.Lock()
	held := r.rt.held
	r.rt.gone = true
	r.rt.mu.Unlock()
	if !held {
		t.Fatal("deleting workload thawed on cancellation/deadline")
	}
}

func TestRequiredCaptureRestartInvalidatesOrProtectsHold(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "deleting"}[deleting], func(t *testing.T) {
			r := newRig(t, time.Hour, 1<<20)
			req := captureRequest(r)
			r.rt.setDiff(tarOf(nil))
			r.cl.deleting = deleting
			if _, err := r.e.CaptureRequired(context.Background(), r.pod, req); err != nil {
				t.Fatal(err)
			}
			restarted := Engine{NodeAgentEpoch: "boot-b", Runtime: r.rt, Cluster: r.cl, WorkDir: r.e.WorkDir}
			protected, err := restarted.RecoverCaptureHolds(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if protected[r.rt.upper] != deleting {
				t.Fatalf("restart protection=%v, deletion=%v", protected, deleting)
			}
		})
	}
}

func TestRequiredCaptureCorruptRecoveryFailsClosed(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	captureRequest(r)
	if err := os.MkdirAll(filepath.Join(r.e.WorkDir, "capture-holds"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.e.WorkDir, "capture-holds", "c1.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.RecoverCaptureHolds(context.Background()); err == nil {
		t.Fatal("incomplete hold permitted orphan thaw")
	}
}

func TestRequiredCaptureOldBootCannotThawNewGuard(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	r.rt.setDiff(tarOf(nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := r.e.CaptureRequired(ctx, r.pod, req); err != nil {
		t.Fatal(err)
	}
	// Emulate the new current API guard and durable journal while the old worker
	// still has its thaw handle. Process-level ownership also prevents this overlap.
	r.cl.mu.Lock()
	r.cl.guard = captureResult(r.pod, req, "boot-b")
	r.cl.mu.Unlock()
	c, err := r.rt.Inspect(context.Background(), r.pod.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	newer := &requiredHold{Result: captureResult(r.pod, req, "boot-b"), Pod: r.pod, Container: c}
	if err := r.e.writeHold(newer); err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(450 * time.Millisecond)
	r.rt.mu.Lock()
	held := r.rt.held
	r.rt.mu.Unlock()
	if !held {
		t.Fatal("old boot thawed new boot's guard")
	}
	b, err := os.ReadFile(newer.journal)
	if err != nil {
		t.Fatal(err)
	}
	var stored requiredHold
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Result.NodeAgentEpoch != "boot-b" {
		t.Fatal("old worker erased the newer journal")
	}
}

func TestRequiredCaptureUnsupportedDataIsFailure(t *testing.T) {
	for _, kind := range []byte{tar.TypeFifo, tar.TypeReg} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			r := newRig(t, time.Hour, 1<<20)
			req := captureRequest(r)
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			h := &tar.Header{Name: "data", Mode: 0600, Typeflag: kind}
			if kind == tar.TypeReg {
				h.PAXRecords = map[string]string{"large": strings.Repeat("x", 9000)}
			}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			r.rt.setDiff(buf.Bytes())
			result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
			if err == nil || result.Result == "Succeeded" {
				t.Fatalf("unsupported required data silently omitted: %+v %v", result, err)
			}
		})
	}
}

func TestRequiredCaptureUnchangedLatestAcknowledgesExactImage(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	r.rt.setDiff(tarOf(map[string]string{"work": "saved"}))
	tr := r.track(t)
	if err := tr.snapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	recs, _, _ := r.cl.snapshot()
	if len(recs) != 1 {
		t.Fatal("latest fixture was not saved")
	}
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if err != nil || result.Image != recs[0].Image || result.SizeBytes != 5 || result.Result != "Succeeded" || !result.Quiesced {
		t.Fatalf("unchanged latest: %+v %v", result, err)
	}
}

func TestRequiredCaptureFailureKeepsLatestValidState(t *testing.T) {
	r := newRig(t, time.Hour, 10)
	req := captureRequest(r)
	r.rt.setDiff(tarOf(map[string]string{"work": "saved"}))
	tr := r.track(t)
	if err := tr.snapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	recs, _, _ := r.cl.snapshot()
	latest := recs[0].Image
	r.rt.setDiff(tarOf(map[string]string{"work": strings.Repeat("x", 20)}))
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if err == nil || result.Result != "Failed" {
		t.Fatalf("quota failure: %+v %v", result, err)
	}
	after, _, exits := r.cl.snapshot()
	if len(after) != 1 || after[0].Image != latest || len(exits) != 0 || tr.lastSnapshot.Image != latest {
		t.Fatal("failure overwrote latest valid snapshot")
	}
}

// Live snapshots cache a refused digest; it is not proof that data was published.
func TestRequiredCaptureDoesNotAcceptLegacyRefusedDigest(t *testing.T) {
	r := newRig(t, time.Hour, 10)
	req := captureRequest(r)
	tr := r.track(t)
	path := filepath.Join(t.TempDir(), "saved.tar")
	if err := os.WriteFile(path, tarOf(map[string]string{"saved": "12345678"}), 0600); err != nil {
		t.Fatal(err)
	}
	base := r.rt.images[tr.c.ImageRef]
	img, _, err := snapshot.Build(base, path, 8, r.pod.Policy, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	saved := r.reg.Host + "/" + r.pod.Repo + "@sha256:saved"
	r.rt.images[saved] = img
	r.rt.imageRef = saved
	tr.c.ImageRef = saved
	tr.lastSnapshot = Snapshot{Image: saved, SizeBytes: 8, Layers: 1}
	tr.pushed = true
	r.rt.setDiff(tarOf(map[string]string{"work": "12345"}))
	if err := tr.snapshot(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if err == nil || result.Result == "Succeeded" {
		t.Fatalf("legacy refusal became success: %+v %v", result, err)
	}
}

func TestRequiredCaptureDeferredPushIsFailure(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	r.rt.setDiff(tarOf(map[string]string{"work": "saved"}))
	tr := r.track(t)
	if err := tr.snapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	r.e.MinPushInterval = time.Hour
	r.rt.setDiff(tarOf(map[string]string{"work": "new data"}))
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if err == nil || result.Result == "Succeeded" {
		t.Fatalf("deferred push allowed stop: %+v %v", result, err)
	}
}
