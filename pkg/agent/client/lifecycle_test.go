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

// Frozen public aliases must preserve fractional traffic envelopes and the nested
// formula layout so scoped consumers can compile without enabling a profile.
func TestSizingV2PublicAliasesRoundTrip(t *testing.T) {
	feature := &GroupPodsFeature{DefaultVpn: &PodSize{CpuMillicores: 100, MemoryBytes: 128 << 20}, SizingV2: &GroupPodsSizingV2{Profiles: []*GroupPodsSizingProfile{{Id: "point", SupportState: "TESTED_POINT", MaxInputs: &GroupSizingInputs{MaxUsers: 5, MaxActiveLabs: 13, InternetLabs: 10, AllowedRelations: 65, Envelope: &GroupTrafficEnvelope{VpnRetainedFlows: 8200, GatewayRetainedFlows: 8192, VpnNewFlowsPerSecond: 30, GatewayNewFlowsPerSecond: 40, VpnPacketsPerSecond: 250, GatewayPacketsPerSecond: 1000, VpnPayloadMbps: 0.5, GatewayPayloadMbps: 1.25}}, Vpn: &GroupPodFormula{Base: &PodSize{CpuMillicores: 50, MemoryBytes: 80 << 20}, RoundTo: &PodSize{CpuMillicores: 5, MemoryBytes: 1 << 20}}, ValidationProvenance: "owned fixture", Scope: "point"}}}}
	data, err := proto.Marshal(feature)
	if err != nil {
		t.Fatal(err)
	}
	var got protobuf.GroupPodsFeature
	if err = proto.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(feature, &got) || got.GetSizingV2().GetProfiles()[0].GetMaxInputs().GetEnvelope().GetGatewayPayloadMbps() != 1.25 {
		t.Fatal("sizing v2 alias roundtrip lost frozen dimensions")
	}
	legacy := &protobuf.GroupPodsFeature{DefaultVpn: &protobuf.PodSize{CpuMillicores: 100, MemoryBytes: 128 << 20}}
	data, err = proto.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var old GroupPodsFeature
	if err = proto.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	if old.GetSizingV2() != nil || old.GetDefaultVpn().GetCpuMillicores() != 100 {
		t.Fatal("legacy sizing acquired a new profile/default")
	}
}
