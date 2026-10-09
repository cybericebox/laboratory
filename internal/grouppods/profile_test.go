package grouppods

import (
	"math"
	"testing"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func profileFixture() *protobuf.GroupPodsSizingProfile {
	f := &protobuf.GroupPodFormula{Base: &protobuf.PodSize{CpuMillicores: 3, MemoryBytes: 4}, PerUser: &protobuf.PodSize{CpuMillicores: 2, MemoryBytes: 3}, PerActiveLab: &protobuf.PodSize{CpuMillicores: 4, MemoryBytes: 5}, PerInternetLab: &protobuf.PodSize{CpuMillicores: 6, MemoryBytes: 7}, PerAllowedRelation: &protobuf.PodSize{CpuMillicores: 8, MemoryBytes: 9}, PerRetainedFlow: &protobuf.PodSize{CpuMillicores: 1, MemoryBytes: 2}, Floor: &protobuf.PodSize{CpuMillicores: 1, MemoryBytes: 1}, RoundTo: &protobuf.PodSize{CpuMillicores: 10, MemoryBytes: 16}}
	return &protobuf.GroupPodsSizingProfile{SupportState: "SUPPORTED", Vpn: f, Gateway: f, MaxInputs: &protobuf.GroupSizingInputs{MaxUsers: 5, MaxActiveLabs: 13, InternetLabs: 10, AllowedRelations: 65, Envelope: &protobuf.GroupTrafficEnvelope{VpnRetainedFlows: 100, GatewayRetainedFlows: 100, VpnNewFlowsPerSecond: 100, GatewayNewFlowsPerSecond: 100, VpnPacketsPerSecond: 100, GatewayPacketsPerSecond: 100, VpnPayloadMbps: 100, GatewayPayloadMbps: 100}}}
}
func TestSizingV2DimensionsAndEligibility(t *testing.T) {
	p := profileFixture()
	in := &protobuf.GroupSizingInputs{MaxUsers: 1, MaxActiveLabs: 1, InternetLabs: 1, AllowedRelations: 1, Envelope: &protobuf.GroupTrafficEnvelope{VpnRetainedFlows: 3, GatewayRetainedFlows: 5}}
	v, g, ok, err := EvaluateProfile(p, in)
	if err != nil || !ok || v.GetCpuMillicores() != 30 || v.GetMemoryBytes() != 48 || g.GetCpuMillicores() != 30 || g.GetMemoryBytes() != 48 {
		t.Fatalf("formula result %v %v %v %v", v, g, ok, err)
	}
	p.SupportState = "TESTED_POINT"
	if _, _, ok, _ := EvaluateProfile(p, in); ok {
		t.Fatal("tested point selected reduced limits")
	}
	p.SupportState = "SUPPORTED"
	in.Envelope = nil
	if _, _, ok, _ := EvaluateProfile(p, in); ok {
		t.Fatal("missing envelope selected")
	}
	in.Envelope = &protobuf.GroupTrafficEnvelope{VpnPacketsPerSecond: 101}
	if _, _, ok, _ := EvaluateProfile(p, in); ok {
		t.Fatal("unbounded rate selected")
	}
	in.Envelope = &protobuf.GroupTrafficEnvelope{VpnPayloadMbps: math.Inf(1)}
	if _, _, ok, _ := EvaluateProfile(p, in); ok {
		t.Fatal("infinite payload selected")
	}
}
func TestSizingV2OverflowCannotProduceTinyAllocation(t *testing.T) {
	p := profileFixture()
	p.Vpn.Base.CpuMillicores = math.MaxInt64
	in := &protobuf.GroupSizingInputs{MaxUsers: 1, Envelope: &protobuf.GroupTrafficEnvelope{}}
	if _, _, ok, err := EvaluateProfile(p, in); ok || err == nil {
		t.Fatalf("overflow accepted %v %v", ok, err)
	}
}
