package devicestate

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

func TestRequiredCaptureRecoveryThawsAndResumesAfterAPIReturns(t *testing.T) {
	r := newRig(t, 30*time.Millisecond, 1<<20)
	req := captureRequest(r)
	r.e.NodeAgentEpoch = "boot-b"
	c, err := r.rt.Inspect(context.Background(), r.pod.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	h := &requiredHold{Container: c, Pod: r.pod, Result: captureResult(req, "boot-a")}
	if err := r.e.writeHold(h); err != nil {
		t.Fatal(err)
	}
	r.rt.mu.Lock()
	r.rt.held = true
	r.rt.mu.Unlock()
	r.cl.guard = h.Result
	r.cl.checkFailures = 1
	r.rt.setDiff(tarOf(map[string]string{"work": "resumed"}))
	protected, err := r.e.RecoverCaptureHolds(context.Background())
	if err != nil || !protected[c.Cgroup] {
		t.Fatalf("recovery: %v %v", protected, err)
	}
	done := make(chan struct{})
	go func() { r.e.holdWorkers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("recovered hold did not invalidate")
	}
	r.rt.mu.Lock()
	held := r.rt.held
	r.rt.mu.Unlock()
	if held {
		t.Fatal("API invalidated and journal removed, but recovered runtime is still frozen")
	}
	if _, err := os.Stat(h.journal); !os.IsNotExist(err) {
		t.Fatalf("released journal retained: %v", err)
	}
	// Simulate the next periodic reconciliation after an explicit start/clear.
	r.e.Sync(context.Background())
	defer r.e.stopAll()
	deadline := time.Now().Add(3 * time.Second)
	for {
		records, _, _ := r.cl.snapshot()
		if len(records) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("normal persistence never resumed after recovered hold release")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRequiredCaptureFreezeCancellationRetainsOwnership(t *testing.T) {
	dir := fakeCgroup(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	thaw, err := FreezeRequired(ctx, dir)
	if err == nil || thaw == nil {
		t.Fatalf("cancelled freeze lost owned request: thaw=%v err=%v", thaw != nil, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "cgroup.freeze"))
	if strings.TrimSpace(string(b)) != "1" {
		t.Fatal("strict freezer thawed before its caller fenced the API")
	}
	if err := thaw(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "cgroup.freeze"))
	if strings.TrimSpace(string(b)) != "0" {
		t.Fatal("owned thaw did not release freeze request")
	}
}

type confirmationFailRuntime struct {
	*fakeRuntime
	dir string
}

func (r *confirmationFailRuntime) Quiesce(context.Context, Container) (func() error, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	return FreezeRequired(ctx, r.dir)
}
func TestRequiredCaptureFreezeFailureFencesBeforeThaw(t *testing.T) {
	for _, mode := range []string{"healthy", "api-unavailable", "deleting"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t, time.Hour, 1<<20)
			req := captureRequest(r)
			dir := fakeCgroup(t, false)
			r.e.Runtime = &confirmationFailRuntime{fakeRuntime: r.rt, dir: dir}
			switch mode {
			case "api-unavailable":
				r.cl.invalidateErr = errors.New("API unavailable")
			case "deleting":
				r.cl.deleting = true
			}
			var premature atomic.Bool
			r.cl.onInvalidate = func() {
				b, _ := os.ReadFile(filepath.Join(dir, "cgroup.freeze"))
				if strings.TrimSpace(string(b)) != "1" {
					premature.Store(true)
				}
			}
			result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
			if err == nil || result.Result != "Failed" || premature.Load() {
				t.Fatalf("failed freeze escaped barrier: %+v err=%v premature=%v", result, err, premature.Load())
			}
			b, _ := os.ReadFile(filepath.Join(dir, "cgroup.freeze"))
			want := "1"
			if mode == "healthy" {
				want = "0"
			}
			if strings.TrimSpace(string(b)) != want {
				t.Fatalf("freeze=%q want=%s", b, want)
			}
			if mode != "healthy" {
				time.Sleep(250 * time.Millisecond)
				b, _ = os.ReadFile(filepath.Join(dir, "cgroup.freeze"))
				if strings.TrimSpace(string(b)) != "1" {
					t.Fatal("pending/API-failed deletion thawed")
				}
			}
			r.rt.mu.Lock()
			r.rt.gone = true
			r.rt.mu.Unlock()
		})
	}
}

type metadataRuntime struct {
	*fakeRuntime
	known bool
	ids   snapshot.IDMaps
}

func (r *metadataRuntime) Inspect(ctx context.Context, id string) (Container, error) {
	c, err := r.fakeRuntime.Inspect(ctx, id)
	c.OwnershipKnown = r.known
	c.IDs = r.ids
	return c, err
}

func TestRequiredCaptureUnknownOwnershipFailsClosed(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	r.e.Runtime = &metadataRuntime{fakeRuntime: r.rt, known: false}
	r.rt.setDiff(tarOf(map[string]string{"work": "data"}))
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if err == nil || result.Result == "Succeeded" {
		t.Fatalf("unknown OCI maps acknowledged: %+v %v", result, err)
	}
	records, _, exits := r.cl.snapshot()
	if len(records) != 0 || len(exits) != 0 {
		t.Fatal("unknown ownership changed latest snapshot/exit")
	}
}

func TestRequiredCaptureUsesValidatedCurrentOwnership(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	tr := r.track(t)
	tr.c.OwnershipKnown = false
	tr.c.IDs = snapshot.IDMaps{}
	ids := snapshot.IDMaps{UID: snapshot.IDMap{{ContainerID: 0, HostID: 100000, Size: 65536}}, GID: snapshot.IDMap{{ContainerID: 0, HostID: 200000, Size: 65536}}}
	r.e.Runtime = &metadataRuntime{fakeRuntime: r.rt, known: true, ids: ids}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "work", Mode: 0600, Typeflag: tar.TypeReg, Size: 1, Uid: 100123, Gid: 200456}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	r.rt.setDiff(buf.Bytes())
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if err != nil || result.Result != "Succeeded" {
		t.Fatalf("mapped capture: %+v %v", result, err)
	}
	img, err := r.reg.Image(context.Background(), r.pod.Repo)
	if err != nil {
		t.Fatal(err)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	rc, err := layers[len(layers)-1].Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	h, err := tar.NewReader(rc).Next()
	if err != nil {
		t.Fatal(err)
	}
	if h.Uid != 123 || h.Gid != 456 {
		t.Fatalf("saved host ownership from stale tracked metadata: uid=%d gid=%d", h.Uid, h.Gid)
	}
}

func TestRequiredCaptureRecoveryThawFailureKeepsJournal(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	c, err := r.rt.Inspect(context.Background(), r.pod.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	h := &requiredHold{Container: c, Pod: r.pod, Result: captureResult(req, "boot-a")}
	if err := r.e.writeHold(h); err != nil {
		t.Fatal(err)
	}
	r.rt.held = true
	r.rt.thawFailures = 1
	r.cl.guard = h.Result
	protected, err := r.e.RecoverCaptureHolds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !protected[c.Cgroup] {
		t.Fatal("failed owned thaw was passed to generic orphan thaw")
	}
	if _, err := os.Stat(h.journal); err != nil {
		t.Fatalf("failed thaw discarded journal: %v", err)
	}
	done := make(chan struct{})
	go func() { r.e.holdWorkers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("owned thaw never retried")
	}
	r.rt.mu.Lock()
	held := r.rt.held
	r.rt.mu.Unlock()
	if held {
		t.Fatal("retried owned thaw left runtime frozen")
	}
}

type noFreezerRuntime struct{ *fakeRuntime }

func (r noFreezerRuntime) Quiesce(context.Context, Container) (func() error, error) {
	return nil, ErrNoFreezer
}
func TestRequiredCaptureFailureBeforeFreezeCleansOnlyAfterFence(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	r.e.Runtime = noFreezerRuntime{r.rt}
	r.cl.invalidateErr = errors.New("API unavailable")
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if err == nil || result.Result != "Failed" {
		t.Fatalf("capture: %+v %v", result, err)
	}
	journal := filepath.Join(r.e.WorkDir, "capture-holds", "c1.json")
	if _, err := os.Stat(journal); err != nil {
		t.Fatal(err)
	}
	r.cl.mu.Lock()
	r.cl.invalidateErr = nil
	r.cl.mu.Unlock()
	done := make(chan struct{})
	go func() { r.e.holdWorkers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pre-freeze failure never cleaned its API-invalidated hold")
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("fenced pre-freeze journal retained: %v", err)
	}
}
