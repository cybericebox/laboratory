package devicestate

import (
	"context"
	"testing"
	"time"
)

// Both a refreshed existing watcher and a newly constructed request-bearing
// watcher must resume ordinary and final persistence once the hold is released.
func TestRequiredCaptureResumeRestoresOrdinaryExit(t *testing.T) {
	for _, initial := range []bool{false, true} {
		for _, outcome := range []string{"failed", "cancelled"} {
			for _, exit := range []string{"event", "resync"} {
				t.Run(testName(initial, outcome, exit), func(t *testing.T) {
					r := newRig(t, time.Hour, 1<<20)
					req := captureRequest(r)
					p := r.pod
					p.CaptureRequest = &req
					var tr *tracked
					if initial {
						c, err := r.rt.Inspect(context.Background(), p.ContainerID)
						if err != nil {
							t.Fatal(err)
						}
						tr = r.e.track(context.Background(), p, c)
					} else {
						tr = r.track(t)
					}
					if tr == nil {
						t.Fatal("ordinary watcher not created")
					}
					defer r.e.stopAll()
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					r.rt.setDiff(tarOf(nil))
					if outcome == "failed" {
						r.rt.failDiff = 1
					}
					result, err := r.e.CaptureRequired(ctx, p, req)
					if outcome == "failed" {
						if err == nil || result.Result != "Failed" {
							t.Fatalf("capture: %+v %v", result, err)
						}
					} else {
						if err != nil || result.Result != "Succeeded" {
							t.Fatalf("capture: %+v %v", result, err)
						}
						r.e.onExit(context.Background(), p.ContainerID)
						records, _, exits := r.cl.snapshot()
						if len(records) != 0 || len(exits) != 0 {
							t.Fatal("active required hold allowed ordinary exit persistence")
						}
						cancel()
						done := make(chan struct{})
						go func() { r.e.holdWorkers.Wait(); close(done) }()
						select {
						case <-done:
						case <-time.After(3 * time.Second):
							t.Fatal("cancelled hold did not invalidate/thaw")
						}
					}
					r.cl.mu.Lock()
					r.cl.pods = []PodInfo{r.pod}
					r.cl.mu.Unlock()
					r.e.Sync(context.Background())
					r.rt.setDiff(tarOf(map[string]string{"work": "ordinary"}))
					if err := tr.snapshot(context.Background(), true); err != nil {
						t.Fatal(err)
					}
					records, _, exits := r.cl.snapshot()
					if len(records) != 1 || len(exits) != 0 {
						t.Fatalf("ordinary persistence did not resume: %v %v", records, exits)
					}
					r.rt.setDiff(tarOf(map[string]string{"work": "final"}))
					if exit == "event" {
						r.e.onExit(context.Background(), p.ContainerID)
					} else {
						ended := r.pod
						ended.Ended = true
						ended.Running = false
						r.cl.mu.Lock()
						r.cl.pods = []PodInfo{ended}
						r.cl.mu.Unlock()
						r.e.Sync(context.Background())
					}
					records, _, exits = r.cl.snapshot()
					if len(records) != 2 || records[1].SizeBytes != 5 || records[1].Image == records[0].Image || len(exits) != 1 || exits[0] != p.Pod {
						t.Fatalf("cleared required request suppressed ordinary final capture: records=%d exits=%v", len(records), exits)
					}
				})
			}
		}
	}
}

func testName(initial bool, outcome, exit string) string {
	if initial {
		return "initial-request/" + outcome + "/" + exit
	}
	return "existing-watcher/" + outcome + "/" + exit
}

func TestRequiredCaptureDeletingHoldStillSuppressesOrdinaryExit(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	req := captureRequest(r)
	p := r.pod
	p.CaptureRequest = &req
	c, err := r.rt.Inspect(context.Background(), p.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	if r.e.track(context.Background(), p, c) == nil {
		t.Fatal("watcher unavailable")
	}
	defer r.e.stopAll()
	r.rt.setDiff(tarOf(nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := r.e.CaptureRequired(ctx, p, req); err != nil {
		t.Fatal(err)
	}
	r.cl.mu.Lock()
	r.cl.deleting = true
	r.cl.mu.Unlock()
	cancel()
	r.e.onExit(context.Background(), p.ContainerID)
	time.Sleep(250 * time.Millisecond)
	records, _, exits := r.cl.snapshot()
	r.rt.mu.Lock()
	held := r.rt.held
	r.rt.gone = true
	r.rt.mu.Unlock()
	if !held || len(records) != 0 || len(exits) != 0 {
		t.Fatalf("deleting hold escaped suppression: held=%v records=%v exits=%v", held, records, exits)
	}
	done := make(chan struct{})
	go func() { r.e.holdWorkers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("dead task did not release bookkeeping")
	}
}

func TestRequiredCaptureCreatedTrackingResumesPersistenceAndExit(t *testing.T) {
	for _, outcome := range []string{"failed", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			r := newRig(t, 30*time.Millisecond, 1<<20)
			req := captureRequest(r)
			p := r.pod
			p.CaptureRequest = &req
			r.rt.setDiff(tarOf(nil))
			if outcome == "failed" {
				r.rt.failDiff = 1
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result, err := r.e.CaptureRequired(ctx, p, req)
			if outcome == "failed" {
				if err == nil || result.Result != "Failed" {
					t.Fatalf("capture: %+v %v", result, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				cancel()
				done := make(chan struct{})
				go func() { r.e.holdWorkers.Wait(); close(done) }()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("hold did not release")
				}
			}
			defer r.e.stopAll()
			r.rt.setDiff(tarOf(map[string]string{"work": "ordinary"}))
			r.cl.mu.Lock()
			r.cl.pods = []PodInfo{r.pod}
			r.cl.mu.Unlock()
			r.e.Sync(context.Background())
			deadline := time.Now().Add(2 * time.Second)
			for {
				records, _, _ := r.cl.snapshot()
				if len(records) == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("initial required-capture placeholder never resumed normal persistence after request clear")
				}
				time.Sleep(10 * time.Millisecond)
			}
			r.rt.setDiff(tarOf(map[string]string{"work": "final"}))
			r.e.onExit(context.Background(), p.ContainerID)
			records, _, exits := r.cl.snapshot()
			if len(records) != 2 || records[1].SizeBytes != 5 || len(exits) != 1 || exits[0] != p.Pod {
				t.Fatalf("initial capture tracking failed final snapshot: %v %v", records, exits)
			}
		})
	}
}
