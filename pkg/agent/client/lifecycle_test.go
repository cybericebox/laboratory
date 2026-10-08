package client

import (
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestLifecyclePublicAliasesRoundTrip(t *testing.T) {
	request := &StopLabsRequest{Items: []*StopLabItem{{Target: &LabLifecycleTarget{OperationId: "op", LifecycleRevision: 1, ExpectedLabUid: "uid"}, SnapshotMode: StopSnapshotMode_STOP_SNAPSHOT_MODE_REQUIRED, Terminal: true}}}
	data, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var got protobuf.StopLabsRequest
	if err := proto.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(request, &got) {
		t.Fatal("public stop alias changed wire contract")
	}
	var _ *StartLabsRequest = &protobuf.StartLabsRequest{}
	var _ *LabLifecycleStatus = &protobuf.LabLifecycleStatus{}
	var _ *ResourceAllocation = &protobuf.ResourceAllocation{}
	var _ *ResourceAmounts = &protobuf.ResourceAmounts{}
	var _ *LifecycleFeature = &protobuf.LifecycleFeature{}
}
