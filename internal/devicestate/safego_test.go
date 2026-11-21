package devicestate

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/snapshot"
)

func TestSafeGoTurnsAPanicIntoAnError(t *testing.T) {
	got := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		safeGo("test body", func(err error) { got <- err }, func() { panic("boom") })
	}()
	<-done // the goroutine ended normally: the panic did not escape
	err := <-got
	var pe *PanicError
	if !errors.As(err, &pe) || pe.Name != "test body" || pe.Value != "boom" || !strings.Contains(string(pe.Stack), "TestSafeGoTurnsAPanicIntoAnError") {
		t.Fatalf("%v %+v", err, pe)
	}
	if !strings.Contains(err.Error(), "panic in test body: boom") {
		t.Fatal(err)
	}
	safeGo("quiet", nil, func() { panic("no handler") }) // a nil handler is fine
	ran := false
	safeGo("plain", nil, func() { ran = true })
	if !ran {
		t.Fatal("the body did not run")
	}
}

// A panic in a goroutine of one device's machinery marks that device failed, says so in its status, and stops its snapshots in this
// incarnation; every other device and the engine go on.
func TestAPanicInAGoroutineMarksTheDeviceFailedAndTheEngineContinues(t *testing.T) {
	r, _ := rigWithClock(t)
	tr := r.track(t)
	other := &tracked{e: r.e, pod: r.pod, c: Container{ID: "c2", UpperDir: r.rt.upper, ImageRef: "docker.io/library/app:1", OwnershipKnown: true}, cancel: func() {}}
	other.pod.ContainerID = "c2"
	r.e.mu.Lock()
	r.e.tracked["c2"] = other
	r.e.mu.Unlock()

	done := make(chan struct{})
	r.e.goSafe("test goroutine", "c1", func() { defer close(done); panic("a bug in the snapshot machinery") })
	<-done
	deadline := time.Now().Add(3 * time.Second)
	for tr.failedWith() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(tr.failedWith(), "a bug in the snapshot machinery") {
		t.Fatalf("the device was not marked failed: %q", tr.failedWith())
	}
	if _, warns, _ := r.cl.snapshot(); len(warns) != 1 || !strings.Contains(warns[0], "stopped after an internal error") {
		t.Fatalf("the status must say it: %v", warns)
	}
	r.rt.setDiff(tarOf(map[string]string{"a": "1"}))
	before := r.rt.diffs
	if err := tr.snapshot(context.Background(), true); !errors.Is(err, errDeviceFailed) {
		t.Fatalf("a failed device is not snapshotted: %v", err)
	}
	if err := tr.snapshot(context.Background(), false); !errors.Is(err, errDeviceFailed) {
		t.Fatalf("not even at exit: %v", err)
	}
	if r.rt.diffs != before {
		t.Fatal("a diff was started for a failed device")
	}
	// the other device is untouched
	if other.failedWith() != "" {
		t.Fatal("another device was marked")
	}
	if err := other.snapshot(context.Background(), true); err != nil {
		t.Fatalf("the engine does not go on for the other devices: %v", err)
	}
	r.e.Sync(context.Background()) // and the engine still syncs
}

type panicDiffRuntime struct{ *fakeRuntime }

func (panicDiffRuntime) Diff(context.Context, Container, bool, snapshot.Policy) (io.ReadCloser, error) {
	panic("diff blew up")
}

// The panic of a live snapshot (the goroutine of the debounce) is caught too: the node-state process survives and the device is failed.
func TestAPanicInALiveSnapshotFailsThatDeviceOnly(t *testing.T) {
	r := newRig(t, 20*time.Millisecond, 1<<20)
	r.e.Runtime = panicDiffRuntime{r.rt}
	r.e.Sync(context.Background())
	defer r.e.stopAll()
	r.e.mu.Lock()
	tr := r.e.tracked["c1"]
	r.e.mu.Unlock()
	if tr == nil {
		t.Fatal("not followed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for tr.failedWith() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(tr.failedWith(), "diff blew up") {
		t.Fatalf("the panic of the snapshot goroutine was not caught: %q", tr.failedWith())
	}
	if _, warns, _ := r.cl.snapshot(); len(warns) == 0 {
		t.Fatal("no warning on the device")
	}
}
