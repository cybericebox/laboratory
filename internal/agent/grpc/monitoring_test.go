package grpc

import (
	"context"
	"testing"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"google.golang.org/grpc"
)

// fakeMonStream captures Sends and feeds one Recv then EOF.
type fakeMonStream struct {
	grpc.ServerStream
	ctx   context.Context
	recvN int
	sent  []*protobuf.MonitoringUpdate
}

func (f *fakeMonStream) Context() context.Context { return f.ctx }
func (f *fakeMonStream) Send(u *protobuf.MonitoringUpdate) error {
	f.sent = append(f.sent, u)
	return nil
}
func (f *fakeMonStream) Recv() (*protobuf.Empty, error) {
	if f.recvN == 0 {
		f.recvN++
		return &protobuf.Empty{}, nil
	}
	return nil, context.Canceled
}

func TestMonitoringEmitsSnapshot(t *testing.T) {
	h, _ := newTestHandler(t)
	ctx := context.Background()
	if _, err := h.CreateLabGroup(ctx, &protobuf.LabGroup{Name: "team-mon"}); err != nil {
		t.Fatal(err)
	}
	s := &fakeMonStream{ctx: ctx}
	_ = h.Monitoring(s)
	if len(s.sent) == 0 || len(s.sent[0].Groups) == 0 {
		t.Errorf("expected at least one group in snapshot, got %+v", s.sent)
	}
}
