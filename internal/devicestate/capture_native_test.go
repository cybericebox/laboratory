//go:build linux

package devicestate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestRequiredCaptureNativeOwnerLock(t *testing.T) {
	dir := t.TempDir()
	owner, err := AcquireCaptureOwner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := AcquireCaptureOwner(dir); err == nil {
		other.Close()
		t.Fatal("overlapping node-agent acquired capture ownership")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := AcquireCaptureOwner(dir)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

// This verifies real kernel freeze/sync, not a containerd capture/restore claim.
func TestRequiredCaptureNativeFreezer(t *testing.T) {
	if os.Getenv("CICE_CAPTURE_NATIVE") != "1" {
		t.Skip("requires owned privileged Linux container")
	}
	root := "/sys/fs/cgroup"
	dir := filepath.Join(root, "cri-containerd-cice-task2-"+strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dir)
	work := t.TempDir()
	path := filepath.Join(work, "ticks")
	child := exec.Command("sh", "-c", "while :; do echo x >> \"$1\"; sleep 0.01; done", "writer", path)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(filepath.Join(dir, "cgroup.freeze"), []byte("0"), 0644)
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(child.Process.Pid)), 0644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	thaw, err := FreezeRequired(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncFilesystem(work); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	after, _ := os.ReadFile(path)
	if len(before) != len(after) || !frozen(dir) {
		t.Fatal("frozen writer continued to mutate filesystem")
	}
	ThawOrphansExcept(root, map[string]bool{dir: true})
	if !frozen(dir) {
		t.Fatal("protected required capture thawed during recovery")
	}
	cancelled, cancelFreeze := context.WithCancel(context.Background())
	cancelFreeze()
	pendingThaw, freezeErr := FreezeRequired(cancelled, dir)
	if freezeErr == nil || pendingThaw == nil || !frozen(dir) {
		t.Fatalf("native cancelled strict freeze lost ownership: %v", freezeErr)
	}
	_ = thaw // same owned freeze request remains; only fenced caller releases it
	if err := pendingThaw(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	after, _ = os.ReadFile(path)
	if len(after) <= len(before) {
		t.Fatal("writer did not resume after owned thaw")
	}
}

type nativeRecoveryRuntime struct {
	*fakeRuntime
	dir string
}

func (r *nativeRecoveryRuntime) Inspect(ctx context.Context, id string) (Container, error) {
	c, err := r.fakeRuntime.Inspect(ctx, id)
	c.Cgroup = r.dir
	return c, err
}
func (r *nativeRecoveryRuntime) Thaw(_ context.Context, c Container) error {
	if c.Cgroup != r.dir {
		return ErrStale
	}
	return os.WriteFile(filepath.Join(r.dir, "cgroup.freeze"), []byte("0"), 0644)
}

func TestRequiredCaptureNativeRecoveryAfterAPIOutage(t *testing.T) {
	if os.Getenv("CICE_CAPTURE_NATIVE") != "1" {
		t.Skip("requires owned privileged Linux container")
	}
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	dir := filepath.Join("/sys/fs/cgroup", "cri-containerd-cice-recovery-"+strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dir)
	path := filepath.Join(t.TempDir(), "ticks")
	child := exec.Command("sh", "-c", "while :; do echo x >> \"$1\"; sleep 0.01; done", "writer", path)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(filepath.Join(dir, "cgroup.freeze"), []byte("0"), 0644)
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(child.Process.Pid)), 0644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	_, err := FreezeRequired(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &nativeRecoveryRuntime{fakeRuntime: r.rt, dir: dir}
	r.e.Runtime = runtime
	r.e.NodeAgentEpoch = "boot-b"
	c, err := runtime.Inspect(context.Background(), r.pod.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	h := &requiredHold{Container: c, Pod: r.pod, Result: captureResult(req, "boot-a")}
	if err := r.e.writeHold(h); err != nil {
		t.Fatal(err)
	}
	r.cl.guard = h.Result
	r.cl.checkFailures = 1
	protected, err := r.e.RecoverCaptureHolds(context.Background())
	if err != nil || !protected[dir] {
		t.Fatalf("native recovery: %v %v", protected, err)
	}
	ThawOrphansExcept("/sys/fs/cgroup", protected)
	if !frozen(dir) {
		t.Fatal("API-outage recovery thawed before invalidation")
	}
	before, _ := os.ReadFile(path)
	deadline := time.Now().Add(3 * time.Second)
	for {
		after, _ := os.ReadFile(path)
		if len(after) > len(before) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("API recovery never resumed the real frozen writer")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.cl.mu.Lock()
	result := r.cl.capture
	r.cl.mu.Unlock()
	if result.GuardState != "Invalidated" || result.Result != "Failed" {
		t.Fatalf("native writer resumed without failed API fence: %+v", result)
	}
	r.e.holdWorkers.Wait()
	r.e.stopAll()
}

func TestRequiredCaptureNativeCommittedRecoveryKeepsRealWriterFrozen(t *testing.T) {
	if os.Getenv("CICE_CAPTURE_NATIVE") != "1" {
		t.Skip("requires owned privileged Linux container")
	}
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	dir := filepath.Join("/sys/fs/cgroup", "cri-containerd-cice-commit-"+strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dir)
	path := filepath.Join(t.TempDir(), "ticks")
	child := exec.Command("sh", "-c", "while :; do echo x >> \"$1\"; sleep 0.01; done", "writer", path)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(filepath.Join(dir, "cgroup.freeze"), []byte("0"), 0644)
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(child.Process.Pid)), 0644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	initial, readErr := os.ReadFile(path)
	if readErr != nil || len(initial) == 0 {
		t.Fatal("native writer never produced control data", readErr)
	}
	if _, err := FreezeRequired(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	native := &nativeRecoveryRuntime{fakeRuntime: r.rt, dir: dir}
	r.e.Runtime = native
	r.e.NodeAgentEpoch = "new-boot"
	cl := &committingCluster{fakeCluster: r.cl, requested: true}
	r.e.Cluster = cl
	c, err := native.Inspect(context.Background(), r.pod.ContainerID)
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
	r.cl.guard = result
	r.cl.checkFailures = 1
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	protected, err := r.e.RecoverCaptureHolds(runCtx)
	if err != nil || !protected[dir] {
		t.Fatal("committed recovery lost native ownership", protected, err)
	}
	ThawOrphansExcept("/sys/fs/cgroup", protected)
	waitCommitted(t, cl)
	cancel()
	before, _ := os.ReadFile(path)
	time.Sleep(1100 * time.Millisecond)
	after, _ := os.ReadFile(path)
	if !frozen(dir) || len(before) != len(after) {
		t.Fatal("committed real writer thawed on restart/deadline/worker cancellation")
	}
}
