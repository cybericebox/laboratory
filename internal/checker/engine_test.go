package checker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/labchecks"
)

var t0 = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

type fakeWriter struct {
	mu    sync.Mutex
	log   []string
	fail  error
	saved map[string]laboratoryv1alpha1.CheckResult
	acks  []int64
}

func (w *fakeWriter) PublishCheck(_ context.Context, lab LabKey, r laboratoryv1alpha1.CheckResult) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail != nil {
		return w.fail
	}
	if w.saved == nil {
		w.saved = map[string]laboratoryv1alpha1.CheckResult{}
	}
	w.saved[r.ID] = r
	w.log = append(w.log, fmt.Sprintf("check %s %s", r.ID, r.State))
	return nil
}

func (w *fakeWriter) AckRun(_ context.Context, lab LabKey, token int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail != nil {
		return w.fail
	}
	w.acks = append(w.acks, token)
	w.log = append(w.log, fmt.Sprintf("ack %d", token))
	return nil
}

func (w *fakeWriter) count(prefix string) int {
	n := 0
	for _, l := range w.log {
		if len(l) >= len(prefix) && l[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

type fakeBackend struct {
	mu      sync.Mutex
	obs     *fakeObserver
	openErr error
	opened  int
	devices map[string]int
}

func (b *fakeBackend) Open(_ context.Context, pod PodInfo) (Observer, func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opened++
	if b.devices == nil {
		b.devices = map[string]int{}
	}
	b.devices[pod.Device]++
	if b.openErr != nil {
		return nil, nil, b.openErr
	}
	return b.obs, func() {}, nil
}

var lab1 = LabKey{Namespace: "ns", Name: "lab1"}

type rig struct {
	t   *testing.T
	e   *Engine
	w   *fakeWriter
	b   *fakeBackend
	now time.Time
	// what the observed file contains: the checks below pass when it holds "ok"
	content string
}

func fileCheck(id, device, mode string, schedule int32, manual bool) labchecks.Check {
	return labchecks.Check{ID: id, Device: device, Mode: mode, ScheduleSeconds: schedule, ManualRun: manual, TimeLimitSeconds: 5,
		Parts: []labchecks.Part{{File: &labchecks.FilePart{Path: "/f", ContentContains: sp("ok")}}}}
}

func newRig(t *testing.T, max int, checks ...labchecks.Check) *rig {
	t.Helper()
	r := &rig{t: t, w: &fakeWriter{}, now: t0, content: "ok"}
	r.b = &fakeBackend{obs: &fakeObserver{files: map[string]FileFacts{}}}
	r.setContent("ok")
	r.e = NewEngine(Config{Backend: r.b, Writer: r.w, MaxConcurrent: max, Now: func() time.Time { return r.now }, Log: logr.Discard(), FirstRunWindow: 4 * time.Second})
	info := LabInfo{Key: lab1, UID: "uid1", Results: map[string]Published{}}
	for _, c := range checks {
		info.Refs = append(info.Refs, CheckRef{ID: c.ID, Device: c.Device, ManualRun: c.ManualRun})
	}
	r.e.SetLab(info)
	r.e.SetDefinitions(lab1, "uid1", checks)
	return r
}

func (r *rig) setContent(c string) { r.b.obs.set("/f", present(c)) }

func (r *rig) pod(device string, ready bool) {
	r.e.SetPod(PodInfo{Lab: lab1, Device: device, Name: "p-" + device, UID: "pu-" + device, Ready: ready})
}

// step advances the clock, asks for the due runs, runs them, and flushes.
func (r *rig) step(d time.Duration) []Job {
	r.now = r.now.Add(d)
	jobs := r.e.Tick(r.now)
	for _, j := range jobs {
		r.e.Execute(context.Background(), j)
	}
	r.e.Flush(context.Background())
	return jobs
}

func (r *rig) published(id string) laboratoryv1alpha1.CheckResult { return r.w.saved[id] }

func TestNothingRunsBeforeTheDeviceIsReady(t *testing.T) {
	r := newRig(t, 4, fileCheck("c", "web", "continuous", 10, false))
	if jobs := r.step(time.Second); len(jobs) != 0 {
		t.Fatalf("no pod yet: %v", jobs)
	}
	r.pod("web", false)
	for i := 0; i < 20; i++ {
		if jobs := r.step(time.Second); len(jobs) != 0 {
			t.Fatalf("pod not ready: %v", jobs)
		}
	}
	if r.b.opened != 0 || len(r.w.log) != 0 {
		t.Fatalf("the device was touched: opened %d, log %v", r.b.opened, r.w.log)
	}
	r.pod("web", true)
	got := 0
	for i := 0; i < 6 && got == 0; i++ { // within the first-run window
		got += len(r.step(time.Second))
	}
	if got != 1 {
		t.Fatalf("the check must run within the window after the device is ready, got %d runs", got)
	}
}

func TestBootstrapBarrier(t *testing.T) {
	r := newRig(t, 4, fileCheck("c", "web", "continuous", 10, false))
	r.pod("web", true)
	info := LabInfo{Key: lab1, UID: "uid1", Refs: []CheckRef{{ID: "c", Device: "web"}}, Results: map[string]Published{},
		BootstrapEnabled: map[string]bool{"web": true}, BootstrapPhase: map[string]laboratoryv1alpha1.BootstrapPhase{"web": laboratoryv1alpha1.BootstrapPending}}
	for _, phase := range []laboratoryv1alpha1.BootstrapPhase{laboratoryv1alpha1.BootstrapPending, laboratoryv1alpha1.BootstrapFailed, laboratoryv1alpha1.BootstrapNone} {
		info.BootstrapPhase["web"] = phase
		r.e.SetLab(info)
		for i := 0; i < 8; i++ {
			if jobs := r.step(time.Second); len(jobs) != 0 {
				t.Fatalf("bootstrap %q: a run started", phase)
			}
		}
	}
	info.BootstrapPhase["web"] = laboratoryv1alpha1.BootstrapDone
	r.e.SetLab(info)
	got := 0
	for i := 0; i < 8; i++ {
		got += len(r.step(time.Second))
	}
	if got == 0 {
		t.Fatal("bootstrap done: the check must run")
	}
}

// Only changes are written: many runs with the same result make one write.
func TestOnlyChangesAreWritten(t *testing.T) {
	r := newRig(t, 4, fileCheck("c", "web", "continuous", 5, false))
	r.pod("web", true)
	runs := 0
	for i := 0; i < 60; i++ {
		runs += len(r.step(time.Second))
	}
	if runs < 8 {
		t.Fatalf("expected a run every ~5 s, got %d in 60 s", runs)
	}
	if n := r.w.count("check"); n != 1 {
		t.Fatalf("%d writes for %d identical runs: %v", n, runs, r.w.log)
	}
	first := r.published("c")
	if first.State != laboratoryv1alpha1.CheckPassed || first.FirstPassedMs == 0 || first.ChangedMs != first.FirstPassedMs {
		t.Fatalf("got %+v", first)
	}
	r.setContent("changed")
	for i := 0; i < 15; i++ {
		r.step(time.Second)
	}
	second := r.published("c")
	if second.State != laboratoryv1alpha1.CheckNotPassed || second.FirstPassedMs != first.FirstPassedMs || second.ChangedMs <= first.ChangedMs || second.Detail == "" {
		t.Fatalf("not passed after the change: %+v (was %+v)", second, first)
	}
	r.setContent("ok")
	for i := 0; i < 15; i++ {
		r.step(time.Second)
	}
	third := r.published("c")
	if third.State != laboratoryv1alpha1.CheckPassed || third.FirstPassedMs != first.FirstPassedMs || third.Detail != "" || r.w.count("check") != 3 {
		t.Fatalf("passed again: %+v, writes %v", third, r.w.log)
	}
}

func TestOnceStopsAfterTheFirstPassAndNeverClearsIt(t *testing.T) {
	r := newRig(t, 4, fileCheck("o", "web", "once", 3, false))
	r.pod("web", true)
	r.setContent("not yet")
	for i := 0; i < 12; i++ {
		r.step(time.Second)
	}
	if got := r.published("o"); got.State != laboratoryv1alpha1.CheckNotPassed || got.FirstPassedMs != 0 {
		t.Fatalf("keeps running until it passes: %+v", got)
	}
	r.setContent("ok")
	for i := 0; i < 12; i++ {
		r.step(time.Second)
	}
	passed := r.published("o")
	if passed.State != laboratoryv1alpha1.CheckPassed || passed.FirstPassedMs == 0 {
		t.Fatalf("got %+v", passed)
	}
	opened := r.b.opened
	r.setContent("broken again")
	r.b.openErr = errors.New("device gone")
	for i := 0; i < 30; i++ {
		if jobs := r.step(time.Second); len(jobs) != 0 {
			t.Fatal("a once check that has passed must not run again")
		}
	}
	if r.b.opened != opened || r.published("o") != passed {
		t.Fatalf("changed after the pass: %+v", r.published("o"))
	}
}

func TestOnceThatPassedBeforeARestartIsNotRunAgain(t *testing.T) {
	r := newRig(t, 4)
	c := fileCheck("o", "web", "once", 2, true)
	r.e.SetLab(LabInfo{Key: lab1, UID: "uid1", Refs: []CheckRef{{ID: "o", Device: "web", ManualRun: true}},
		Results: map[string]Published{"o": {State: laboratoryv1alpha1.CheckPassed, FirstPassedMs: 42, ChangedMs: 42}}})
	r.e.SetDefinitions(lab1, "uid1", []labchecks.Check{c})
	r.pod("web", true)
	for i := 0; i < 10; i++ {
		if jobs := r.step(time.Second); len(jobs) != 0 {
			t.Fatal("ran a once check that passed before the restart")
		}
	}
	if len(r.w.log) != 0 {
		t.Fatalf("a restart wrote %v", r.w.log)
	}
}

func TestErrorRuleKeepsWhatWasEarned(t *testing.T) {
	r := newRig(t, 4, fileCheck("c", "web", "continuous", 2, true))
	r.pod("web", true)
	for i := 0; i < 8; i++ {
		r.step(time.Second)
	}
	earned := r.published("c")
	if earned.State != laboratoryv1alpha1.CheckPassed {
		t.Fatalf("got %+v", earned)
	}
	writes := r.w.count("check")
	r.b.openErr = &ObserveError{Reason: "the container is not running"}
	// One and two failed scheduled runs: nothing is written.
	seen := 0
	for i := 0; i < 40 && seen < ErrorsBeforePublish-1; i++ {
		seen += len(r.step(time.Second))
	}
	if r.w.count("check") != writes || r.published("c") != earned {
		t.Fatalf("a hiccup was written: %v", r.w.log)
	}
	// The third one in a row publishes the error, keeps firstPassedMs, and says why.
	for i := 0; i < 40 && r.published("c").State != laboratoryv1alpha1.CheckError; i++ {
		r.step(time.Second)
	}
	got := r.published("c")
	if got.State != laboratoryv1alpha1.CheckError || got.FirstPassedMs != earned.FirstPassedMs || got.Detail != "the container is not running" || got.Value != "" || got.ChangedMs <= earned.ChangedMs {
		t.Fatalf("got %+v", got)
	}
	// More failures with the same reason write nothing more.
	n := r.w.count("check")
	for i := 0; i < 20; i++ {
		r.step(time.Second)
	}
	if r.w.count("check") != n {
		t.Fatal("the same error was written again")
	}
	// Recovery replaces the error.
	r.b.openErr = nil
	for i := 0; i < 10 && r.published("c").State == laboratoryv1alpha1.CheckError; i++ {
		r.step(time.Second)
	}
	if got := r.published("c"); got.State != laboratoryv1alpha1.CheckPassed || got.FirstPassedMs != earned.FirstPassedMs || got.Detail != "" {
		t.Fatalf("recovered: %+v", got)
	}
}

func TestFirstErrorWithNothingPublishedIsWrittenAtOnce(t *testing.T) {
	r := newRig(t, 4, fileCheck("c", "web", "continuous", 2, false))
	r.pod("web", true)
	r.b.openErr = errors.New("boom")
	for i := 0; i < 6 && r.w.count("check") == 0; i++ {
		r.step(time.Second)
	}
	if got := r.published("c"); got.State != laboratoryv1alpha1.CheckError || got.FirstPassedMs != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestAFailedRunAtTheMomentTheDeviceLeavesTheBarrierCountsForNothing(t *testing.T) {
	r := newRig(t, 4, fileCheck("c", "web", "continuous", 2, false))
	r.pod("web", true)
	for i := 0; i < 8; i++ {
		r.step(time.Second)
	}
	earned := r.published("c")
	r.now = r.now.Add(10 * time.Second)
	jobs := r.e.Tick(r.now)
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs", len(jobs))
	}
	r.pod("web", false) // the pod went NotReady while the run was going
	r.b.openErr = errors.New("gone")
	r.e.Execute(context.Background(), jobs[0])
	r.e.Flush(context.Background())
	if r.published("c") != earned {
		t.Fatalf("a run of a device that is not ready was recorded: %+v", r.published("c"))
	}
}

// The node runs at most MaxConcurrent checks at once, and at most one of each lab: five labs with one check each go two by two.
func TestConcurrencyCapPerNode(t *testing.T) {
	r := newRig(t, 2)
	r.e.cfg.FirstRunWindow = time.Nanosecond // everything is due at the first look
	for i := 0; i < 5; i++ {
		key := LabKey{Namespace: "ns", Name: fmt.Sprintf("lab%d", i)}
		c := fileCheck("c", "web", "continuous", 100, false)
		r.e.SetLab(LabInfo{Key: key, UID: "u" + key.Name, Refs: []CheckRef{{ID: "c", Device: "web"}}, Results: map[string]Published{}})
		r.e.SetDefinitions(key, "u"+key.Name, []labchecks.Check{c})
		r.e.SetPod(PodInfo{Lab: key, Device: "web", Name: "p", UID: "pu", Ready: true})
	}
	first := r.e.Tick(r.now)
	if len(first) != 2 {
		t.Fatalf("cap 2, got %d runs", len(first))
	}
	if more := r.e.Tick(r.now); len(more) != 0 {
		t.Fatalf("the two are still running: %d more", len(more))
	}
	for _, j := range first {
		r.e.Execute(context.Background(), j)
	}
	second := r.e.Tick(r.now)
	for _, j := range second {
		r.e.Execute(context.Background(), j)
	}
	third := len(r.e.Tick(r.now))
	if len(second) != 2 || third != 1 {
		t.Fatalf("the rest follows two by two: %d then %d", len(second), third)
	}
}

// Five checks of one lab run one after another on a node, whatever the node's room: a lab cannot take the room of the others.
func TestOneRunPerLabAtATime(t *testing.T) {
	var cs []labchecks.Check
	for i := 0; i < 5; i++ {
		cs = append(cs, fileCheck(fmt.Sprintf("c%d", i), "web", "continuous", 100, false))
	}
	r := newRig(t, 8, cs...)
	r.e.cfg.FirstRunWindow = time.Nanosecond
	r.pod("web", true)
	for i := 0; i < 5; i++ {
		jobs := r.e.Tick(r.now)
		if len(jobs) != 1 {
			t.Fatalf("round %d: %d runs of one lab at once", i, len(jobs))
		}
		if more := r.e.Tick(r.now); len(more) != 0 {
			t.Fatalf("round %d: a second run of the lab started while the first goes on: %d", i, len(more))
		}
		r.e.Execute(context.Background(), jobs[0])
	}
}

// A check that asks for more time than the ceiling is run with the ceiling.
func TestTimeLimitCeiling(t *testing.T) {
	c := fileCheck("c", "web", "continuous", 100, false)
	c.TimeLimitSeconds = 600
	r := newRig(t, 4, c)
	r.e.cfg.MaxTimeLimit = 20 * time.Second
	r.e.cfg.FirstRunWindow = time.Nanosecond
	r.pod("web", true)
	jobs := r.e.Tick(r.now)
	if len(jobs) != 1 {
		t.Fatal(len(jobs))
	}
	var deadline time.Duration
	r.b.obs = &fakeObserver{files: map[string]FileFacts{}, onRead: func(ctx context.Context) {
		if d, ok := ctx.Deadline(); ok {
			deadline = time.Until(d)
		}
	}}
	r.setContent("ok")
	r.e.Execute(context.Background(), jobs[0])
	if deadline <= 0 || deadline > 21*time.Second {
		t.Fatalf("the run had %v, the ceiling is 20 s", deadline)
	}
}

func TestSpreadOfFirstRunsAndNextRuns(t *testing.T) {
	e := NewEngine(Config{FirstRunWindow: 10 * time.Second})
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		d := e.firstDelay(fmt.Sprintf("lab-%d", i), "check", 60*time.Second)
		if d < 0 || d > 10*time.Second {
			t.Fatalf("first delay %v outside the window", d)
		}
		seen[d] = true
	}
	if len(seen) < 25 {
		t.Fatalf("only %d distinct first delays for 50 labs", len(seen))
	}
	if d := e.firstDelay("lab", "check", 3*time.Second); d > 3*time.Second {
		t.Fatalf("the window is capped by the schedule: %v", d)
	}
	if a, b := e.firstDelay("lab", "check", 60*time.Second), e.firstDelay("lab", "check", 60*time.Second); a != b {
		t.Fatal("the spread must be a function of the check, not of chance")
	}
	distinct := map[time.Time]bool{}
	for n := uint64(1); n <= 40; n++ {
		next := nextAfter(t0, 100*time.Second, "lab", "check", n)
		if next.Before(t0.Add(90*time.Second)) || next.After(t0.Add(110*time.Second)) {
			t.Fatalf("next %v outside +-10%% of the schedule", next.Sub(t0))
		}
		distinct[next] = true
	}
	if len(distinct) < 20 {
		t.Fatalf("the phase must drift: %d distinct", len(distinct))
	}
}

func TestManualRunTakesTheTokenAndAcknowledgesAfterTheResult(t *testing.T) {
	r := newRig(t, 4, fileCheck("m", "web", "continuous", 0, true), fileCheck("auto", "web", "continuous", 0, false))
	r.pod("web", true)
	if jobs := r.step(time.Second); len(jobs) != 0 {
		t.Fatalf("schedule 0 means manual only: %v", jobs)
	}
	refs := []CheckRef{{ID: "m", Device: "web", ManualRun: true}, {ID: "auto", Device: "web"}}
	r.e.SetLab(LabInfo{Key: lab1, UID: "uid1", Refs: refs, Results: map[string]Published{}, Request: RunRequest{Token: 7}})
	jobs := r.step(time.Second)
	if len(jobs) != 1 || jobs[0].Check.ID != "m" || !jobs[0].Manual {
		t.Fatalf("all manual-run checks, and no other: %+v", jobs)
	}
	if len(r.w.log) != 2 || r.w.log[0] != "check m passed" || r.w.log[1] != "ack 7" {
		t.Fatalf("the result is written first, then the token: %v", r.w.log)
	}
	// The same annotation seen again, with the token acknowledged in the status, runs nothing.
	r.e.SetLab(LabInfo{Key: lab1, UID: "uid1", Refs: refs, Results: map[string]Published{}, Ack: 7, Request: RunRequest{Token: 7}})
	if jobs := r.step(time.Second); len(jobs) != 0 {
		t.Fatalf("the same token again: %v", jobs)
	}
	// A new request (only the named check).
	r.e.SetLab(LabInfo{Key: lab1, UID: "uid1", Refs: refs, Results: map[string]Published{}, Ack: 7, Request: RunRequest{Token: 8, IDs: []string{"m"}}})
	if jobs := r.step(time.Second); len(jobs) != 1 {
		t.Fatalf("token 8: %v", jobs)
	}
	if r.w.acks[len(r.w.acks)-1] != 8 {
		t.Fatalf("acks %v", r.w.acks)
	}
}

func TestManualRunsAreNotLostWhenRequestsOvertakeEachOther(t *testing.T) {
	var q requests
	q.offer(RunRequest{Token: 3, IDs: []string{"a"}})
	q.offer(RunRequest{Token: 5, IDs: []string{"c", "b"}})
	q.offer(RunRequest{Token: 4, IDs: []string{"z"}}) // older than the pending one: dropped
	got, ok := q.take()
	if !ok || got.Token != 5 || fmt.Sprint(got.IDs) != "[a b c]" {
		t.Fatalf("united and the highest token: %+v", got)
	}
	if _, ok := q.take(); ok {
		t.Fatal("taken twice")
	}
	q.offer(RunRequest{Token: 5}) // already taken
	q.offer(RunRequest{Token: 2})
	if _, ok := q.take(); ok {
		t.Fatal("an old token came back")
	}
	q.offer(RunRequest{Token: 6, IDs: []string{"a"}})
	q.offer(RunRequest{Token: 7}) // all
	if got, _ := q.take(); got.Token != 7 || len(got.IDs) != 0 {
		t.Fatalf("all wins over a list: %+v", got)
	}
	q.offer(RunRequest{Token: 8})
	q.offer(RunRequest{Token: 9, IDs: []string{"a"}})
	if got, _ := q.take(); got.Token != 9 || len(got.IDs) != 0 {
		t.Fatalf("all wins over a later list: %+v", got)
	}
	var fresh requests
	fresh.startFrom(12)
	fresh.offer(RunRequest{Token: 12})
	fresh.offer(RunRequest{Token: 11})
	if _, ok := fresh.take(); ok {
		t.Fatal("tokens up to the acknowledged one are done")
	}
}

func TestParseRunRequest(t *testing.T) {
	if r, ok := ParseRunRequest(`{"token":4,"ids":["a","b"]}`); !ok || r.Token != 4 || len(r.IDs) != 2 {
		t.Fatalf("got %+v %v", r, ok)
	}
	for _, bad := range []string{"", "x", `{"token":0}`, `{"token":-1}`, `{"ids":["a"]}`} {
		if _, ok := ParseRunRequest(bad); ok {
			t.Errorf("%q must be ignored", bad)
		}
	}
}

// A request names a check whose device is not ready: nothing runs, the previous result stays, and the token is still taken.
func TestManualRunOfADeviceThatIsNotReady(t *testing.T) {
	r := newRig(t, 4, fileCheck("m", "web", "continuous", 0, true))
	r.pod("web", false)
	r.e.SetLab(LabInfo{Key: lab1, UID: "uid1", Refs: []CheckRef{{ID: "m", Device: "web", ManualRun: true}},
		Results: map[string]Published{"m": {State: laboratoryv1alpha1.CheckPassed, FirstPassedMs: 1, ChangedMs: 1}}, Request: RunRequest{Token: 3}})
	if jobs := r.step(time.Second); len(jobs) != 0 {
		t.Fatalf("not ready: %v", jobs)
	}
	if fmt.Sprint(r.w.log) != "[ack 3]" || r.b.opened != 0 {
		t.Fatalf("log %v, opened %d", r.w.log, r.b.opened)
	}
}

// With devices on several nodes only this node's checks are run, and a request that names none of them is not acknowledged here.
func TestManualRunForChecksOfAnotherNode(t *testing.T) {
	r := newRig(t, 4, fileCheck("here", "web", "continuous", 0, true), fileCheck("there", "db", "continuous", 0, true))
	r.pod("web", true) // db runs on another node
	refs := []CheckRef{{ID: "here", Device: "web", ManualRun: true}, {ID: "there", Device: "db", ManualRun: true}}
	r.e.SetLab(LabInfo{Key: lab1, UID: "uid1", Refs: refs, Results: map[string]Published{}, Request: RunRequest{Token: 2, IDs: []string{"there"}}})
	if jobs := r.step(time.Second); len(jobs) != 0 || len(r.w.log) != 0 {
		t.Fatalf("not ours: %v %v", jobs, r.w.log)
	}
	r.e.SetLab(LabInfo{Key: lab1, UID: "uid1", Refs: refs, Results: map[string]Published{}, Ack: 2, Request: RunRequest{Token: 3}})
	jobs := r.step(time.Second)
	if len(jobs) != 1 || jobs[0].Check.ID != "here" || fmt.Sprint(r.w.acks) != "[3]" {
		t.Fatalf("jobs %v acks %v", jobs, r.w.acks)
	}
}

// Another node writes the acknowledgement first: this node still runs its part of the same request.
func TestAnotherNodesAcknowledgementDoesNotCancelOurPart(t *testing.T) {
	r := newRig(t, 4, fileCheck("here", "web", "continuous", 0, true))
	r.pod("web", true)
	refs := []CheckRef{{ID: "here", Device: "web", ManualRun: true}}
	r.e.SetLab(LabInfo{Key: lab1, UID: "uid1", Refs: refs, Results: map[string]Published{}, Request: RunRequest{Token: 9}})
	r.e.SetLab(LabInfo{Key: lab1, UID: "uid1", Refs: refs, Results: map[string]Published{}, Ack: 9, Request: RunRequest{Token: 9}})
	if jobs := r.step(time.Second); len(jobs) != 1 {
		t.Fatalf("got %d runs", len(jobs))
	}
}

func TestFailedWriteIsRetriedAndAckNeverOvertakesTheResult(t *testing.T) {
	r := newRig(t, 4, fileCheck("m", "web", "continuous", 0, true))
	r.pod("web", true)
	r.w.fail = errors.New("apiserver down")
	r.e.SetLab(LabInfo{Key: lab1, UID: "uid1", Refs: []CheckRef{{ID: "m", Device: "web", ManualRun: true}}, Results: map[string]Published{}, Request: RunRequest{Token: 1}})
	r.step(time.Second)
	if len(r.w.log) != 0 {
		t.Fatalf("%v", r.w.log)
	}
	r.w.fail = nil
	r.step(500 * time.Millisecond) // inside the pause
	if len(r.w.log) != 0 {
		t.Fatalf("retried too early: %v", r.w.log)
	}
	r.step(3 * time.Second)
	if fmt.Sprint(r.w.log) != "[check m passed ack 1]" {
		t.Fatalf("after the pause: %v", r.w.log)
	}
}

func TestRestartContinuesFromTheStatus(t *testing.T) {
	r := newRig(t, 4)
	c := fileCheck("c", "web", "continuous", 2, false)
	r.e.SetLab(LabInfo{Key: lab1, UID: "uid1", Refs: []CheckRef{{ID: "c", Device: "web"}},
		Results: map[string]Published{"c": {State: laboratoryv1alpha1.CheckPassed, FirstPassedMs: 5, ChangedMs: 5}}})
	r.e.SetDefinitions(lab1, "uid1", []labchecks.Check{c})
	r.pod("web", true)
	for i := 0; i < 10; i++ {
		r.step(time.Second)
	}
	if len(r.w.log) != 0 {
		t.Fatalf("the same result as published must write nothing: %v", r.w.log)
	}
}

func TestLabRecreatedWithAnotherUIDStartsClean(t *testing.T) {
	r := newRig(t, 4, fileCheck("c", "web", "continuous", 2, false))
	r.pod("web", true)
	for i := 0; i < 8; i++ {
		r.step(time.Second)
	}
	r.e.SetLab(LabInfo{Key: lab1, UID: "uid2", Refs: []CheckRef{{ID: "c", Device: "web"}}, Results: map[string]Published{}})
	r.e.SetDefinitions(lab1, "uid1", []labchecks.Check{fileCheck("c", "web", "continuous", 2, false)}) // late document of the old lab
	if jobs := r.step(time.Second); len(jobs) != 0 {
		t.Fatalf("definitions of another lab were taken: %v", jobs)
	}
}

func TestInactiveLabRunsNothing(t *testing.T) {
	r := newRig(t, 4, fileCheck("c", "web", "continuous", 2, false))
	r.pod("web", true)
	r.e.SetLab(LabInfo{Key: lab1, UID: "uid1", Refs: []CheckRef{{ID: "c", Device: "web"}}, Results: map[string]Published{}, Inactive: true})
	for i := 0; i < 8; i++ {
		if jobs := r.step(time.Second); len(jobs) != 0 {
			t.Fatal("a retired lab was checked")
		}
	}
}
