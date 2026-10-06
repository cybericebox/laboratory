package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"google.golang.org/grpc"
)

// fakeMonStream captures server-pushed monitoring updates.
type fakeMonStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent chan *protobuf.MonitoringUpdate
}

func (f *fakeMonStream) Context() context.Context { return f.ctx }

func (f *fakeMonStream) Send(u *protobuf.MonitoringUpdate) error {
	select {
	case f.sent <- u:
		return nil
	case <-f.ctx.Done():
		return f.ctx.Err()
	}
}

func TestMonitoringSendsSnapshotThenOnlyChangedRecords(t *testing.T) {
	h, _ := newTestHandler(t)
	// The shared poller observes the platform once per interval; a short one keeps the test fast and not racing its timeout.
	h.SetMonitoringConfig(MonitoringConfig{PollInterval: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &fakeMonStream{ctx: ctx, sent: make(chan *protobuf.MonitoringUpdate, 4)}
	done := make(chan error, 1)
	go func() {
		done <- h.Monitoring(&protobuf.MonitoringRequest{MinIntervalMs: 1}, s)
	}()

	first := receiveMonitoringUpdate(t, s.sent)
	if !first.GetSnapshot() {
		t.Fatalf("first update must be a snapshot: %+v", first)
	}
	if first.GetCapacity().GetTenant() != "default" {
		t.Fatalf("the snapshot carries the subscriber's tenant capacity: %+v", first.GetCapacity())
	}

	if first.GetFeatures().GetTenant() != "default" || first.GetFeatures().GetScheduler() == nil {
		t.Fatalf("the snapshot carries the subscriber's features: %+v", first.GetFeatures())
	}

	res, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: groupItems("team-mon")})
	wantStates(t, res, err, stCreated)

	delta := receiveMonitoringUpdate(t, s.sent)
	if delta.GetSnapshot() {
		t.Fatalf("changed state must be sent as a delta: %+v", delta)
	}
	if delta.GetFeatures() != nil {
		t.Fatalf("unchanged features are not repeated: %+v", delta.GetFeatures())
	}
	if len(delta.GetGroups()) != 1 || delta.GetGroups()[0].GetName() != "team-mon" {
		t.Fatalf("expected only changed group, got %+v", delta.GetGroups())
	}
	cancel()
	<-done
}

func receiveMonitoringUpdate(t *testing.T, updates <-chan *protobuf.MonitoringUpdate) *protobuf.MonitoringUpdate {
	t.Helper()
	select {
	case update := <-updates:
		return update
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for monitoring update")
		return nil
	}
}

// L-3: a tenant holds a bounded number of Monitoring streams.
func TestMonitoringStreamsArePerTenantCapped(t *testing.T) {
	h := &Handler{}
	h.monMaxStreams = 2
	if !h.openStream("a") || !h.openStream("a") || h.openStream("a") {
		t.Fatal("two streams of a tenant, not three")
	}
	if !h.openStream("b") {
		t.Fatal("another tenant is not held back")
	}
	h.closeStream("a")
	if !h.openStream("a") {
		t.Fatal("a place is free after one closes")
	}
	h.monMaxStreams = 0
	for i := 0; i < 50; i++ {
		if !h.openStream("c") {
			t.Fatal("0 means unlimited")
		}
	}
}
