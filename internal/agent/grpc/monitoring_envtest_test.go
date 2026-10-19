package grpc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// TestMonitoringTwoBackendsOnARealAPIServer exercises the stream against a real
// API server and the real poller: two subscribers with different selectors, a
// resume after a disconnect, and a slow subscriber dropped without effect on the other.
func TestMonitoringTwoBackendsOnARealAPIServer(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetMonitoringConfig(MonitoringConfig{PollInterval: 100 * time.Millisecond, SubscriberBuffer: 3})
	ctx := context.Background()
	mk := func(name, inst string) {
		t.Helper()
		res, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: name, Labels: map[string]string{"instance": inst}}}})
		wantStates(t, res, err, stCreated)
	}
	mk("g-a1", "a")
	mk("g-b1", "b")

	start := func(selector string, resume int64, epoch string) (chan *protobuf.MonitoringUpdate, chan error, context.CancelFunc) {
		sctx, cancel := context.WithCancel(ctx)
		updates := make(chan *protobuf.MonitoringUpdate, 256)
		done := make(chan error, 1)
		go func() {
			done <- h.Monitoring(&protobuf.MonitoringRequest{Selector: selector, MinIntervalMs: 1, ResumeAfterSequence: resume, AgentEpoch: epoch},
				&fakeMonStream{ctx: sctx, sent: updates})
		}()
		return updates, done, cancel
	}
	collect := func(updates chan *protobuf.MonitoringUpdate, want int) (seen []string, last *protobuf.MonitoringUpdate) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for len(seen) < want {
			select {
			case u := <-updates:
				for _, g := range u.Groups {
					seen = append(seen, g.Name)
				}
				last = u
			case <-deadline:
				t.Fatalf("timed out; saw %v, want %d groups", seen, want)
			}
		}
		return seen, last
	}

	aUp, aDone, aCancel := start("instance=a", 0, "")
	bUp, _, bCancel := start("instance=b", 0, "")
	defer bCancel()
	if seen, _ := collect(aUp, 1); seen[0] != "g-a1" {
		t.Fatalf("subscriber a snapshot: %v", seen)
	}
	if seen, _ := collect(bUp, 1); seen[0] != "g-b1" {
		t.Fatalf("subscriber b snapshot: %v", seen)
	}

	mk("g-a2", "a")
	mk("g-b2", "b")
	if seen, _ := collect(aUp, 1); seen[0] != "g-a2" {
		t.Fatalf("subscriber a delta: %v", seen)
	}
	if seen, _ := collect(bUp, 1); seen[0] != "g-b2" {
		t.Fatalf("subscriber b delta: %v", seen)
	}
	select {
	case u := <-aUp:
		t.Fatalf("subscriber a received something of b: %+v", u.Groups)
	case <-time.After(300 * time.Millisecond):
	}

	// Labels set through UpdateLabGroups show in the stream.
	res, err := h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{
		Changes: &protobuf.LabGroupChanges{Labels: &protobuf.LabelChanges{Set: map[string]string{"tier": "gold"}}},
		Items:   []*protobuf.UpdateLabGroupItem{{Name: "g-a1"}},
	})
	wantStates(t, res, err, stUpdated)
	_, upd := collect(aUp, 1)
	if upd.Groups[0].Labels["tier"] != "gold" || upd.Groups[0].Labels["instance"] != "a" {
		t.Fatalf("merged labels: %v", upd.Groups[0].Labels)
	}
	epoch, position := upd.AgentEpoch, upd.Sequence

	// Subscriber a disconnects; two groups appear; it resumes and gets exactly those.
	aCancel()
	<-aDone
	mk("g-a3", "a")
	mk("g-a4", "a")
	mk("g-b3", "b")
	time.Sleep(500 * time.Millisecond)
	rUp, _, rCancel := start("instance=a", position, epoch)
	defer rCancel()
	seen, last := collect(rUp, 2)
	if last.Snapshot || len(seen) != 2 {
		t.Fatalf("resume must replay the missed updates, not a snapshot: snapshot=%v seen=%v", last.Snapshot, seen)
	}
	got := map[string]bool{seen[0]: true, seen[1]: true}
	if !got["g-a3"] || !got["g-a4"] {
		t.Fatalf("missed groups: %v", seen)
	}

	// A subscriber that cannot keep up is dropped; the healthy one never notices.
	slowCtx, slowCancel := context.WithCancel(ctx)
	defer slowCancel()
	slowDone := make(chan error, 1)
	go func() {
		slowDone <- h.Monitoring(&protobuf.MonitoringRequest{MinIntervalMs: 1}, &blockingStream{ctx: slowCtx})
	}()
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < 8; i++ {
		mk(fmt.Sprintf("g-flood-%d", i), "b")
		time.Sleep(150 * time.Millisecond)
	}
	select {
	case err := <-slowDone:
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("slow subscriber ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the slow subscriber was not dropped")
	}
	// b saw the b-floods (and g-b3) without gaps: it is still attached.
	count := 0
	deadline := time.After(5 * time.Second)
	for count < 9 {
		select {
		case u := <-bUp:
			count += len(u.Groups)
		case <-deadline:
			t.Fatalf("the healthy subscriber saw %d of 9 groups", count)
		}
	}
}

// The ending notice of a reservation reaches its tenant's stream: the snapshot carries the reservation, the notice comes as a
// delta with the phase, the time and the reason.
func TestMonitoringStreamsTheEndingNotice(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetMonitoringConfig(MonitoringConfig{PollInterval: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	to := time.Now().Add(4 * time.Minute).Truncate(time.Second)
	res, err := h.CreateReservations(ctx, &protobuf.CreateReservationsRequest{Items: []*protobuf.ReservationItem{{
		Id: "r-end", FromUnixMs: time.Now().Add(-time.Hour).UnixMilli(), ToUnixMs: to.UnixMilli(), CpuMillicores: 1000, MemoryBytes: 1 << 30}}})
	wantStates(t, res, err, stCreated)
	s := &fakeMonStream{ctx: ctx, sent: make(chan *protobuf.MonitoringUpdate, 16)}
	done := make(chan error, 1)
	go func() { done <- h.Monitoring(&protobuf.MonitoringRequest{MinIntervalMs: 1}, s) }()
	first := receiveMonitoringUpdate(t, s.sent)
	if !first.GetSnapshot() || len(first.GetReservations()) != 1 || first.GetReservations()[0].GetId() != "r-end" {
		t.Fatalf("the snapshot carries the reservation: %v", first.GetReservations())
	}

	// play the operator: the notice is given
	r, err := h.cs.LaboratoryV1alpha1().Reservations().Get(ctx, "r-end", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	r.Status.Phase, r.Status.NoticeAt = laboratoryv1alpha1.ReservationPhaseEnding, &now
	r.Status.Conditions = []metav1.Condition{{Type: laboratoryv1alpha1.ReservationConditionEnding, Status: metav1.ConditionTrue,
		Reason: laboratoryv1alpha1.ReservationReasonWindowClosing, Message: "the reservation ends", LastTransitionTime: now}}
	if _, err := h.cs.LaboratoryV1alpha1().Reservations().UpdateStatus(ctx, r, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case u := <-s.sent:
			for _, got := range u.GetReservations() {
				st := got.GetStatus()
				if st.GetPhase() != protobuf.ReservationPhase_RESERVATION_PHASE_ENDING {
					continue
				}
				if u.GetSnapshot() || st.GetEndsAtUnixMs() != to.UnixMilli() || st.GetEndingReason() != "WindowClosing" || st.GetNoticeAtUnixMs() != now.UnixMilli() {
					t.Fatalf("the notice: %v (snapshot %t)", st, u.GetSnapshot())
				}
				cancel()
				<-done
				return
			}
		case <-deadline:
			t.Fatal("the ending notice did not come")
		}
	}
}
