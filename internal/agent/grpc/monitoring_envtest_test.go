package grpc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
