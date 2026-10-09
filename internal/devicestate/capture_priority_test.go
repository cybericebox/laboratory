package devicestate

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Inspect is before CaptureRequired's shared tracked mutex. Holding it here
// gives the existing Watch -> Debounce pipeline an actual pending-capture window.
type priorityInspectRuntime struct {
	*fakeRuntime
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *priorityInspectRuntime) Inspect(ctx context.Context, id string) (Container, error) {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-ctx.Done():
		return Container{}, ctx.Err()
	case <-r.release:
		return r.fakeRuntime.Inspect(ctx, id)
	}
}

func TestRequiredCaptureSyncPrioritizesPendingCurrentOverExistingDebounce(t *testing.T) {
	for _, dispatch := range []string{"sync", "direct"} {
		t.Run(dispatch, func(t *testing.T) { requiredCapturePriority(t, dispatch) })
	}
}

func requiredCapturePriority(t *testing.T, dispatch string) {
	r := newRig(t, 20*time.Millisecond, 1<<20)
	r.e.Poll = 5 * time.Millisecond
	clock := &testClock{t: time.Date(2026, 10, 9, 4, 8, 22, 987654321, time.UTC)}
	r.e.Now = clock.now
	r.e.MinPushInterval = time.Minute
	req := captureRequest(r)
	r.cl.mu.Lock()
	r.cl.pods = []PodInfo{r.pod}
	r.cl.mu.Unlock()
	r.rt.setDiff(tarOf(map[string]string{"work": "prior valid"}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.e.Sync(ctx)
	defer r.e.stopAll()
	deadline := time.Now().Add(3 * time.Second)
	for {
		records, _, _ := r.cl.snapshot()
		if len(records) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("existing Watch/Debounce did not publish its initial snapshot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.e.mu.Lock()
	tr := r.e.tracked[r.pod.ContainerID]
	r.e.mu.Unlock()
	tr.mu.Lock() // The prior push record is complete before moving exact time.
	priorPushes := len(tr.pushes)
	tr.mu.Unlock()
	if priorPushes != 1 {
		t.Fatalf("initial snapshot push record is incomplete: %d", priorPushes)
	}
	// This is exact native push eligibility, including its fractional timestamp.
	clock.advance(time.Minute)
	gate := &priorityInspectRuntime{fakeRuntime: r.rt, entered: make(chan struct{}), release: make(chan struct{})}
	tr.mu.Lock()
	r.e.Runtime = gate
	tr.mu.Unlock()
	released := false
	defer func() {
		if !released {
			close(gate.release)
		}
		r.e.captureWorkers.Wait()
		r.rt.mu.Lock()
		r.rt.gone = true
		r.rt.mu.Unlock()
		r.e.holdWorkers.Wait()
	}()
	p := r.pod
	p.CaptureRequest = &req
	r.cl.mu.Lock()
	r.cl.pods = []PodInfo{p}
	r.cl.mu.Unlock()
	if dispatch == "direct" {
		r.e.captureWorkers.Add(1)
		go func() {
			defer r.e.captureWorkers.Done()
			_, _ = r.e.CaptureRequired(ctx, p, req)
		}()
	} else {
		r.e.Sync(ctx)
	}
	select {
	case <-gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("Sync did not dispatch the exact current Required capture")
	}
	r.rt.setDiff(tarOf(map[string]string{"work": "final required"}))
	if err := os.WriteFile(filepath.Join(r.rt.upper, "work"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	// The already attached real watcher/debounce gets a bounded opportunity to
	// consume the eligible budget while Required Inspect is explicitly blocked.
	deadline = time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		records, _, _ := r.cl.snapshot()
		if len(records) > 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The task is still alive and writable before Required quiescence. A later
	// write needs a new strict image, rather than inheriting the ordinary digest.
	r.rt.setDiff(tarOf(map[string]string{"work": "last required write"}))
	close(gate.release)
	released = true
	r.e.captureWorkers.Wait()
	r.cl.mu.Lock()
	result := r.cl.capture
	r.cl.mu.Unlock()
	records, _, exits := r.cl.snapshot()
	if result.Result != "Succeeded" || result.Image == "" || len(records) != 1 || len(exits) != 0 {
		t.Fatalf("ordinary watcher consumed pending Required budget: capture=%+v ordinaryRecords=%d exits=%v", result, len(records), exits)
	}
}

func TestRequiredCapturePendingClearStaleCancelAndExitResumeExistingWatcher(t *testing.T) {
	for _, scenario := range []string{"cleared", "new-start", "cancelled", "foreign-pod", "missing-pod-uid", "epoch", "incarnation", "not-running", "ended", "exit"} {
		t.Run(scenario, func(t *testing.T) {
			r := newRig(t, 20*time.Millisecond, 1<<20)
			r.e.Poll = 5 * time.Millisecond
			req := captureRequest(r)
			r.rt.setDiff(tarOf(nil))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.e.Sync(context.Background())
			defer r.e.stopAll()
			r.e.mu.Lock()
			tr := r.e.tracked[r.pod.ContainerID]
			r.e.mu.Unlock()
			tr.mu.Lock()
			initiallyPushed := tr.pushed
			tr.mu.Unlock()
			if initiallyPushed {
				t.Fatal("empty initial layer published a snapshot")
			}
			gate := &priorityInspectRuntime{fakeRuntime: r.rt, entered: make(chan struct{}), release: make(chan struct{})}
			tr.mu.Lock()
			r.e.Runtime = gate
			tr.mu.Unlock()
			defer func() { close(gate.release); r.e.captureWorkers.Wait() }()
			p := r.pod
			p.CaptureRequest = &req
			r.cl.mu.Lock()
			r.cl.pods = []PodInfo{p}
			r.cl.mu.Unlock()
			r.e.Sync(ctx)
			select {
			case <-gate.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("exact current capture not dispatched")
			}
			r.rt.setDiff(tarOf(map[string]string{"work": "ordinary after clear"}))
			if err := os.WriteFile(filepath.Join(r.rt.upper, "work"), []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
			// Let this actual change be debounced while scheduling is pending.
			time.Sleep(60 * time.Millisecond)
			switch scenario {
			case "exit":
				r.e.onExit(context.Background(), p.ContainerID)
				r.rt.mu.Lock()
				r.rt.gone = true
				r.rt.mu.Unlock()
			case "cancelled":
				cancel()
				r.e.captureWorkers.Wait()
				r.cl.mu.Lock()
				r.cl.pods = []PodInfo{r.pod}
				r.cl.mu.Unlock()
				r.e.Sync(context.Background())
			default:
				next := p
				changed := req
				next.CaptureRequest = &changed
				switch scenario {
				case "cleared", "new-start":
					next.CaptureRequest = nil
					next.Capture = nil
				case "foreign-pod":
					changed.PodUID = "foreign"
				case "missing-pod-uid":
					changed.PodUID = ""
				case "epoch":
					changed.Epoch++
				case "incarnation":
					changed.Incarnation++
				case "not-running":
					next.Running = false
				case "ended":
					next.Running = false
					next.Ended = true
				}
				r.cl.mu.Lock()
				r.cl.pods = []PodInfo{next}
				r.cl.mu.Unlock()
				r.e.Sync(context.Background())
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				records, _, exits := r.cl.snapshot()
				if len(records) == 1 {
					if scenario == "exit" && len(exits) != 1 {
						t.Fatal("scheduling marker became exit hold authority")
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("ordinary watcher did not resume after %s: records=%d exits=%v", scenario, len(records), exits)
				}
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
		})
	}
}

func TestRequiredCapturePendingPriorityPreservesBudgetFailure(t *testing.T) {
	for _, limit := range []string{"interval", "budget"} {
		t.Run(limit, func(t *testing.T) {
			r, clock := rigWithClock(t)
			req := captureRequest(r)
			tr := r.track(t)
			defer r.e.stopAll()
			if err := push(t, r, tr, "work", "prior good"); err != nil {
				t.Fatal(err)
			}
			if limit == "interval" {
				r.e.MinPushInterval = time.Minute
				clock.advance(29 * time.Second) // Residual cannot fit the 30s capture deadline.
			} else {
				r.e.PushBudget = 1
				r.e.PushBudgetWindow = time.Hour
			}
			r.rt.setDiff(tarOf(map[string]string{"work": "required changed"}))
			result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
			if err == nil || result.Result != "Failed" || result.Image != "" {
				t.Fatalf("priority bypassed %s: %+v %v", limit, result, err)
			}
			if records, _, exits := r.cl.snapshot(); len(records) != 1 || len(exits) != 0 {
				t.Fatal("budget refusal replaced prior valid state or authorized an exit")
			}
			// Failed attempts clear only their own scheduling token.
			r.e.mu.Lock()
			pending := tr.captureAttempt
			r.e.mu.Unlock()
			if pending != nil {
				t.Fatal("completed failure retained pending scheduling authority")
			}
		})
	}
}

func TestRequiredCapturePendingFailedCurrentRequestKeepsPriorityUntilFreshClear(t *testing.T) {
	for _, failure := range []string{"interval", "quota"} {
		t.Run(failure, func(t *testing.T) {
			r, clock := rigWithClock(t)
			r.pod.Policy.Debounce = 20 * time.Millisecond
			normalPolicy := r.pod.Policy
			req := captureRequest(r)
			tr := r.track(t)
			defer r.e.stopAll()
			if err := push(t, r, tr, "work", "prior good"); err != nil {
				t.Fatal(err)
			}
			r.e.MinPushInterval = time.Minute
			if failure == "quota" {
				r.pod.Policy.WriteQuota = 1
			} else {
				clock.advance(29 * time.Second) // Residual cannot fit the 30s capture deadline.
			}
			r.rt.setDiff(tarOf(map[string]string{"work": "required changed"}))
			p := r.pod
			p.CaptureRequest = &req
			r.cl.mu.Lock()
			r.cl.pods = []PodInfo{p}
			r.cl.mu.Unlock()
			r.e.Sync(context.Background())
			r.e.captureWorkers.Wait()
			r.cl.mu.Lock()
			result := r.cl.capture
			r.cl.mu.Unlock()
			if result.Result != "Failed" || result.Image != "" {
				t.Fatalf("expected current %s failure: %+v", failure, result)
			}
			// Make rate and quota eligible again without clearing the actual API
			// request. A completed failed worker must not reopen ordinary pushing.
			clock.advance(10 * time.Minute)
			tr.mu.Lock()
			tr.pod.Policy = normalPolicy
			tr.mu.Unlock()
			r.rt.setDiff(tarOf(map[string]string{"work": "ordinary changed"}))
			if err := tr.snapshot(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if records, _, _ := r.cl.snapshot(); len(records) != 1 {
				t.Fatal("failed worker reopened ordinary budget while exact current API request remains")
			}
			r.cl.mu.Lock()
			r.cl.pods = []PodInfo{r.pod}
			r.cl.mu.Unlock()
			r.e.Sync(context.Background())
			if err := tr.snapshot(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if records, _, _ := r.cl.snapshot(); len(records) != 2 {
				t.Fatal("fresh current request clear did not restore ordinary persistence")
			}
		})
	}
}

func TestRequiredCapturePendingDelayedSyncWorkerCannotRestoreClearedPriority(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	tr := r.track(t)
	defer r.e.stopAll()
	p := r.pod
	p.CaptureRequest = &req
	// Model an already dispatched old Sync worker which has not entered capture.
	r.e.setPendingCapture(p.ContainerID, &req)
	r.cl.mu.Lock()
	r.cl.pods = []PodInfo{r.pod}
	r.cl.mu.Unlock()
	r.e.Sync(context.Background())
	gate := &priorityInspectRuntime{fakeRuntime: r.rt, entered: make(chan struct{}), release: make(chan struct{})}
	tr.mu.Lock()
	r.e.Runtime = gate
	tr.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = r.e.captureRequired(ctx, p, req, false) }()
	select {
	case <-gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("delayed worker did not enter Inspect")
	}
	r.rt.setDiff(tarOf(map[string]string{"work": "ordinary new start"}))
	if err := tr.snapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if records, _, _ := r.cl.snapshot(); len(records) != 1 {
		t.Fatal("old copied request restored scheduling priority after fresh clear")
	}
	cancel()
	close(gate.release)
	<-done
}
