package grpc

import (
	"context"
	"testing"

	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// An agent that runs with the lifecycle capabilities off answers every lifecycle call UNSUPPORTED (not retryable, nothing
// changed); one that advertises a capability accepts the call.
func TestLifecycleCallsWithoutCapabilityAreUnsupported(t *testing.T) {
	h, _ := newTestHandler(t)
	ctx := context.Background()
	h.SetFeatures(Features{Lifecycle: &protobuf.LifecycleFeature{}})

	check := func(name string, res *protobuf.BatchResult, err error) {
		t.Helper()
		if err != nil || len(res.GetResults()) != 1 {
			t.Fatalf("%s: %v %v", name, res, err)
		}
		if r := res.Results[0]; r.State != protobuf.ItemState_ITEM_STATE_FAILED || r.Reason != client.ReasonUnsupported || r.Retryable {
			t.Fatalf("%s: want UNSUPPORTED, got %+v", name, r)
		}
	}
	gt := &protobuf.GroupTarget{Group: "g", OperationId: "op", Revision: 1}
	res, err := h.StopLabGroups(ctx, &protobuf.StopLabGroupsRequest{Items: []*protobuf.StopLabGroupItem{{Target: gt, RequireAllLabsStopped: true}}})
	check("StopLabGroups", res, err)
	res, err = h.StartLabGroups(ctx, &protobuf.StartLabGroupsRequest{Items: []*protobuf.GroupTarget{gt}})
	check("StartLabGroups", res, err)
	lt := &protobuf.LabLifecycleTarget{Ref: &protobuf.ItemRef{LabGroup: "g", Name: "a"}, OperationId: "op", LifecycleRevision: 2}
	res, err = h.StopLabs(ctx, &protobuf.StopLabsRequest{Items: []*protobuf.StopLabItem{{Target: lt, SnapshotMode: protobuf.StopSnapshotMode_STOP_SNAPSHOT_MODE_SKIP}}})
	check("StopLabs", res, err)
	res, err = h.StartLabs(ctx, &protobuf.StartLabsRequest{Items: []*protobuf.LabLifecycleTarget{lt}})
	check("StartLabs", res, err)

	// Advertised: the call goes on (the group does not exist, which is a different answer).
	h.SetFeatures(Features{Lifecycle: &protobuf.LifecycleFeature{FullGroupStop: true}})
	res, err = h.StopLabGroups(ctx, &protobuf.StopLabGroupsRequest{Items: []*protobuf.StopLabGroupItem{{Target: gt, RequireAllLabsStopped: true}}})
	if err != nil || res.Results[0].Reason == client.ReasonUnsupported {
		t.Fatalf("advertised capability must not be refused: %+v %v", res, err)
	}
}
