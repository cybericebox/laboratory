package grpc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/clientset/client/versioned/fake"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// monRig is a Handler over fake clientsets whose monitor is driven by hand:
// the poll interval is an hour, tests call poll after every change.
type monRig struct {
	t   *testing.T
	h   *Handler
	cs  *fake.Clientset
	now time.Time
	mu  sync.Mutex
}

func newMonRig(t *testing.T, cfg MonitoringConfig) *monRig {
	t.Helper()
	cs := fake.NewSimpleClientset()
	h := NewHandler(cs, k8sfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}), nil)
	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Hour
	}
	h.SetMonitoringConfig(cfg)
	r := &monRig{t: t, h: h, cs: cs, now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	h.monitor().now = func() time.Time {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.now
	}
	return r
}

func (r *monRig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

func (r *monRig) poll() {
	r.t.Helper()
	if err := r.h.monitor().poll(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func (r *monRig) group(name, ns string, labels map[string]string) {
	r.t.Helper()
	_, err := r.cs.LaboratoryV1alpha1().LabGroups().Create(context.Background(), &laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status:     laboratoryv1alpha1.LabGroupStatus{Namespace: ns},
	}, metav1.CreateOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
}

func (r *monRig) lab(ns, name string, labels map[string]string) {
	r.t.Helper()
	_, err := r.cs.LaboratoryV1alpha1().Labs(ns).Create(context.Background(), &laboratoryv1alpha1.Lab{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
	}, metav1.CreateOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
}

func (r *monRig) deleteLab(ns, name string) {
	r.t.Helper()
	if err := r.cs.LaboratoryV1alpha1().Labs(ns).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		r.t.Fatal(err)
	}
}

// stream runs Monitoring for the request and returns what it sends and how it ended.
type monClient struct {
	updates chan *protobuf.MonitoringUpdate
	done    chan error
	cancel  context.CancelFunc
}

func (r *monRig) subscribe(req *protobuf.MonitoringRequest) *monClient {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c := &monClient{updates: make(chan *protobuf.MonitoringUpdate, 64), done: make(chan error, 1), cancel: cancel}
	s := &fakeMonStream{ctx: ctx, sent: c.updates}
	go func() { c.done <- r.h.Monitoring(req, s) }()
	r.t.Cleanup(cancel)
	return c
}

func (c *monClient) next(t *testing.T) *protobuf.MonitoringUpdate {
	t.Helper()
	select {
	case u := <-c.updates:
		return u
	case err := <-c.done:
		t.Fatalf("stream ended: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for an update")
	}
	return nil
}

func (c *monClient) none(t *testing.T) {
	t.Helper()
	select {
	case u := <-c.updates:
		t.Fatalf("unexpected update: groups=%v labs=%v deleted=%v", groupNames(u.Groups), labNames(u.Labs), u.DeletedKeys)
	case <-time.After(150 * time.Millisecond):
	}
}

func groupNames(gs []*protobuf.LabGroup) []string {
	var out []string
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out
}

func labNames(ls []*protobuf.Lab) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var (
	instA = map[string]string{"instance": "a"}
	instB = map[string]string{"instance": "b"}
)

func TestJournalIsBoundedBySizeAndAge(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{JournalSize: 3, JournalAge: time.Minute})
	r.poll() // baseline
	for _, n := range []string{"g1", "g2", "g3", "g4", "g5"} {
		r.group(n, "", nil)
		r.poll()
	}
	if got := r.h.monitor().journalLen(); got != 3 {
		t.Fatalf("size bound: journal holds %d, want 3", got)
	}
	r.advance(2 * time.Minute)
	r.group("g6", "", nil)
	r.poll()
	if got := r.h.monitor().journalLen(); got != 1 {
		t.Fatalf("age bound: only the fresh update may stay, journal holds %d", got)
	}
}

func TestSelectorFiltersSnapshotAndDeltas(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{})
	r.group("ga", "ns-a", instA)
	r.group("gb", "ns-b", instB)
	r.lab("ns-a", "la", instA)
	r.lab("ns-b", "lb", instB)

	sub := r.subscribe(&protobuf.MonitoringRequest{Selector: "instance=a", MinIntervalMs: 1})
	snap := sub.next(t)
	if !snap.Snapshot || !eq(groupNames(snap.Groups), []string{"ga"}) || !eq(labNames(snap.Labs), []string{"la"}) {
		t.Fatalf("snapshot must hold only instance=a: groups=%v labs=%v", groupNames(snap.Groups), labNames(snap.Labs))
	}
	if snap.Capacity == nil || snap.AgentEpoch == "" {
		t.Fatal("capacity goes to everyone and every update names the agent epoch")
	}
	if snap.Groups[0].Labels["instance"] != "a" {
		t.Fatal("labels are part of the monitored objects")
	}

	r.lab("ns-b", "lb2", instB)
	r.poll()
	sub.none(t) // not ours
	r.lab("ns-a", "la2", instA)
	r.poll()
	d := sub.next(t)
	if d.Snapshot || !eq(labNames(d.Labs), []string{"la2"}) {
		t.Fatalf("delta %v snapshot=%v", labNames(d.Labs), d.Snapshot)
	}

	all := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 1})
	if s := all.next(t); len(s.Groups) != 2 || len(s.Labs) != 4 {
		t.Fatalf("an empty selector means everything: groups=%d labs=%d", len(s.Groups), len(s.Labs))
	}
}

func TestInvalidSelectorFailsTheCall(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{})
	sub := r.subscribe(&protobuf.MonitoringRequest{Selector: "instance in (a"})
	select {
	case err := <-sub.done:
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an invalid selector must end the call")
	}
}

func TestDeletionsFollowTheLabelsOfTheDeletedObject(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{})
	r.group("ga", "ns-a", instA)
	r.lab("ns-a", "la", instA)
	r.lab("ns-a", "lb-in-a-ns", instB)
	a := r.subscribe(&protobuf.MonitoringRequest{Selector: "instance=a", MinIntervalMs: 1})
	b := r.subscribe(&protobuf.MonitoringRequest{Selector: "instance=b", MinIntervalMs: 1})
	a.next(t)
	b.next(t)

	r.deleteLab("ns-a", "la")
	r.poll()
	d := a.next(t)
	if len(d.DeletedKeys) != 1 || d.DeletedKeys[0].Name != "la" || d.DeletedKeys[0].Kind != "lab" {
		t.Fatalf("the deletion of an instance=a lab must reach subscriber a: %v", d.DeletedKeys)
	}
	b.none(t)

	r.deleteLab("ns-a", "lb-in-a-ns")
	r.poll()
	if d := b.next(t); len(d.DeletedKeys) != 1 || d.DeletedKeys[0].Name != "lb-in-a-ns" {
		t.Fatalf("subscriber b: %v", d.DeletedKeys)
	}
	a.none(t)
}

func TestTrafficFollowsItsLabsLabels(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{})
	r.group("ga", "ns", nil)
	r.lab("ns", "la", instA)
	r.lab("ns", "lb", instB)
	_, err := r.cs.LaboratoryV1alpha1().LabTrafficReports("ns").Create(context.Background(), &laboratoryv1alpha1.LabTrafficReport{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: "ns"},
		Spec:       laboratoryv1alpha1.LabTrafficReportSpec{Kind: laboratoryv1alpha1.LabTrafficSurfaceVPN, Instance: "i"},
		Status: laboratoryv1alpha1.LabTrafficReportStatus{BootID: "b", Ledger: []laboratoryv1alpha1.LabTrafficTouch{
			{Subject: "p1", LabName: "la", DstIP: "10.0.0.1", Attempts: 1},
			{Subject: "p2", LabName: "lb", DstIP: "10.0.0.2", Attempts: 2},
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sub := r.subscribe(&protobuf.MonitoringRequest{Selector: "instance=a", MinIntervalMs: 1})
	snap := sub.next(t)
	if len(snap.Traffic) != 1 || len(snap.Traffic[0].Ledger) != 1 || snap.Traffic[0].Ledger[0].LabName != "la" {
		t.Fatalf("the traffic report must be cut to the touches of instance=a labs: %+v", snap.Traffic)
	}
	only := r.subscribe(&protobuf.MonitoringRequest{Selector: "instance=zzz", MinIntervalMs: 1})
	if s := only.next(t); len(s.Traffic) != 0 {
		t.Fatalf("a report with no visible touch is not sent: %+v", s.Traffic)
	}
}

func TestResumeReplaysOnlyWhatWasMissed(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{})
	r.group("g0", "ns", nil)
	sub := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 1})
	snap := sub.next(t)
	epoch := snap.AgentEpoch

	r.lab("ns", "one", nil)
	r.poll()
	first := sub.next(t)
	sub.cancel()

	// Two changes happen while the subscriber is away.
	r.lab("ns", "two", nil)
	r.poll()
	r.lab("ns", "three", nil)
	r.poll()

	back := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 1, ResumeAfterSequence: first.Sequence, AgentEpoch: epoch})
	d := back.next(t)
	if d.Snapshot {
		t.Fatal("a resume inside the journal window must not send a snapshot")
	}
	if !eq(labNames(d.Labs), []string{"three", "two"}) && !eq(labNames(d.Labs), []string{"two", "three"}) {
		t.Fatalf("only the missed labs expected, got %v", labNames(d.Labs))
	}
	if d.Sequence <= first.Sequence || d.AgentEpoch != epoch {
		t.Fatalf("replay position %d epoch %q", d.Sequence, d.AgentEpoch)
	}
	// ... and then it goes live.
	r.lab("ns", "four", nil)
	r.poll()
	if l := back.next(t); !eq(labNames(l.Labs), []string{"four"}) || l.Sequence <= d.Sequence {
		t.Fatalf("live update %v seq %d", labNames(l.Labs), l.Sequence)
	}

	// Resuming at the very latest sequence replays nothing.
	latest := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 1, ResumeAfterSequence: r.h.monitor().seq, AgentEpoch: epoch})
	latest.none(t)
}

func TestResumeFallsBackToASnapshot(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{JournalSize: 2})
	r.group("g0", "ns", nil)
	sub := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 1})
	epoch := sub.next(t).AgentEpoch
	sub.cancel()

	for _, n := range []string{"a", "b", "c", "d"} {
		r.lab("ns", n, nil)
		r.poll()
	}
	// Wrong epoch: another agent process.
	other := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 1, ResumeAfterSequence: 1, AgentEpoch: "another-process"})
	if s := other.next(t); !s.Snapshot {
		t.Fatal("an epoch mismatch needs a snapshot")
	}
	// Right epoch, but the journal (2 updates) no longer reaches back to sequence 1.
	old := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 1, ResumeAfterSequence: 1, AgentEpoch: epoch})
	if s := old.next(t); !s.Snapshot || len(s.Labs) != 4 {
		t.Fatalf("a position outside the journal needs a snapshot: snapshot=%v labs=%d", s.Snapshot, len(s.Labs))
	}
	// A sequence from the future is no position either.
	future := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 1, ResumeAfterSequence: 9999, AgentEpoch: epoch})
	if s := future.next(t); !s.Snapshot {
		t.Fatal("a sequence beyond the agent's needs a snapshot")
	}
	// No epoch named: cannot trust the sequence.
	noEpoch := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 1, ResumeAfterSequence: r.h.monitor().seq})
	if s := noEpoch.next(t); !s.Snapshot {
		t.Fatal("a resume without an epoch needs a snapshot")
	}
}

func TestResumeWithASelectorReplaysOnlyMatchingUpdates(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{})
	r.group("g", "ns", nil)
	sub := r.subscribe(&protobuf.MonitoringRequest{Selector: "instance=a", MinIntervalMs: 1})
	epoch := sub.next(t).AgentEpoch
	sub.cancel()
	base := r.h.monitor().seq

	r.lab("ns", "x-a", instA)
	r.poll()
	r.lab("ns", "x-b", instB)
	r.poll()
	r.deleteLab("ns", "x-b")
	r.poll()

	back := r.subscribe(&protobuf.MonitoringRequest{Selector: "instance=a", MinIntervalMs: 1, ResumeAfterSequence: base, AgentEpoch: epoch})
	d := back.next(t)
	if d.Snapshot || !eq(labNames(d.Labs), []string{"x-a"}) || len(d.DeletedKeys) != 0 {
		t.Fatalf("replay for instance=a: labs=%v deleted=%v", labNames(d.Labs), d.DeletedKeys)
	}
}

func TestMinIntervalMergesChangesAndSubscribersAreIndependent(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{})
	r.group("g", "ns", nil)
	slow := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 400})
	fast := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 1})
	slow.next(t)
	fast.next(t)

	for _, n := range []string{"m1", "m2", "m3"} {
		r.lab("ns", n, nil)
		r.poll()
	}
	got := map[string]bool{}
	for len(got) < 3 {
		for _, n := range labNames(fast.next(t).Labs) {
			got[n] = true
		}
	}
	if len(got) != 3 {
		t.Fatalf("the fast subscriber sees every change, got %v", got)
	}
	time.Sleep(450 * time.Millisecond)
	d := slow.next(t)
	if len(d.Labs) != 3 {
		t.Fatalf("the slow subscriber gets the three changes merged in one update, got %v", labNames(d.Labs))
	}
	slow.none(t)
}

// blockingStream never accepts an update: a subscriber that cannot keep up.
type blockingStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (b *blockingStream) Context() context.Context { return b.ctx }
func (b *blockingStream) Send(*protobuf.MonitoringUpdate) error {
	<-b.ctx.Done()
	return b.ctx.Err()
}

func TestSlowSubscriberIsDroppedWithoutHoldingBackOthers(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{SubscriberBuffer: 2})
	r.group("g", "ns", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slowDone := make(chan error, 1)
	go func() {
		slowDone <- r.h.Monitoring(&protobuf.MonitoringRequest{MinIntervalMs: 1}, &blockingStream{ctx: ctx})
	}()
	good := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 1})
	good.next(t)

	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		name := string(rune('a' + i))
		r.lab("ns", name, nil)
		r.poll()
		time.Sleep(20 * time.Millisecond) // lets the streams run, as a real poll interval would
		for len(good.updates) > 0 {
			for _, n := range labNames((<-good.updates).Labs) {
				seen[n] = true
			}
		}
	}
	select {
	case err := <-slowDone:
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("the slow stream must end with ResourceExhausted, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the slow subscriber was not dropped")
	}
	for len(good.updates) > 0 {
		for _, n := range labNames((<-good.updates).Labs) {
			seen[n] = true
		}
	}
	if len(seen) != 8 {
		t.Fatalf("the healthy subscriber must see all 8 labs, saw %v", seen)
	}
	select {
	case err := <-good.done:
		t.Fatalf("the healthy stream ended: %v", err)
	default:
	}
}

func TestAccumulatorMergesUpsertsAndDeletes(t *testing.T) {
	a := newAccumulator()
	lab := func(n, spec string) *protobuf.MonitoringUpdate {
		return &protobuf.MonitoringUpdate{Labs: []*protobuf.Lab{{Namespace: "ns", Name: n, LabGroupName: "g", SpecJson: []byte(spec)}}}
	}
	key := func(n string) *protobuf.MonitoringDeletedKey {
		return &protobuf.MonitoringDeletedKey{Kind: "lab", LabGroupName: "g", Namespace: "ns", Name: n}
	}
	a.add(lab("x", "1"))
	a.add(lab("x", "2")) // newest wins
	a.add(lab("y", "1"))
	a.add(&protobuf.MonitoringUpdate{DeletedKeys: []*protobuf.MonitoringDeletedKey{key("y")}}) // upsert then delete
	a.add(&protobuf.MonitoringUpdate{DeletedKeys: []*protobuf.MonitoringDeletedKey{key("z")}})
	a.add(lab("z", "3")) // delete then recreate
	out := a.take()
	if !eq(labNames(out.Labs), []string{"x", "z"}) || string(out.Labs[0].SpecJson) != "2" {
		t.Fatalf("labs %v", labNames(out.Labs))
	}
	if len(out.DeletedKeys) != 1 || out.DeletedKeys[0].Name != "y" {
		t.Fatalf("deleted %v", out.DeletedKeys)
	}
	if !a.empty() {
		t.Fatal("take empties the accumulator")
	}
}

func TestSubscribingStartsThePollerOnlyWhileSomebodyListens(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{PollInterval: 20 * time.Millisecond})
	r.group("g", "ns", nil)
	sub := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 1})
	sub.next(t)
	// No manual poll: the shared poller picks the change up by itself.
	r.lab("ns", "auto", nil)
	if d := sub.next(t); !eq(labNames(d.Labs), []string{"auto"}) {
		t.Fatalf("got %v", labNames(d.Labs))
	}
	sub.cancel()
	<-sub.done
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.h.monitor().mu.Lock()
		running := r.h.monitor().running
		r.h.monitor().mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the poller must stop when the last subscriber leaves")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Changes made meanwhile are not lost: the next poll journals them.
	r.lab("ns", "while-away", nil)
	again := r.subscribe(&protobuf.MonitoringRequest{MinIntervalMs: 1})
	if s := again.next(t); len(s.Labs) != 2 {
		t.Fatalf("a new subscriber sees the changes made while nobody listened: %v", labNames(s.Labs))
	}
}

var _ = errors.New
