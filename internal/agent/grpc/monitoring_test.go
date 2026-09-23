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
	if first.GetCapacity() == nil {
		t.Fatal("snapshot must include cluster capacity for platform monitoring")
	}

	if _, err := h.CreateLabGroup(ctx, &protobuf.LabGroup{Name: "team-mon"}); err != nil {
		t.Fatal(err)
	}

	delta := receiveMonitoringUpdate(t, s.sent)
	if delta.GetSnapshot() {
		t.Fatalf("changed state must be sent as a delta: %+v", delta)
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
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for monitoring update")
		return nil
	}
}
