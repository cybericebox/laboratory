package grouppods

import (
	"fmt"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"math"
)

// EvaluateProfile returns eligible=false for every unsupported or unbounded
// profile, so the caller retains legacy formulas and presets. It does not select
// or resize a running group. CPU and memory each sum, floor, then round upward.
func EvaluateProfile(p *protobuf.GroupPodsSizingProfile, in *protobuf.GroupSizingInputs) (vpn, gateway *protobuf.PodSize, eligible bool, err error) {
	if p.GetSupportState() != "SUPPORTED" || in == nil || p.GetMaxInputs() == nil || !boundedInputs(in, p.GetMaxInputs()) {
		return nil, nil, false, nil
	}
	vpn, err = evaluateFormula(p.GetVpn(), in, in.GetEnvelope().GetVpnRetainedFlows())
	if err != nil {
		return nil, nil, false, err
	}
	gateway, err = evaluateFormula(p.GetGateway(), in, in.GetEnvelope().GetGatewayRetainedFlows())
	if err != nil {
		return nil, nil, false, err
	}
	return vpn, gateway, true, nil
}
func boundedInputs(in, max *protobuf.GroupSizingInputs) bool {
	for _, v := range [][2]int64{{in.GetMaxUsers(), max.GetMaxUsers()}, {in.GetMaxActiveLabs(), max.GetMaxActiveLabs()}, {in.GetInternetLabs(), max.GetInternetLabs()}, {in.GetAllowedRelations(), max.GetAllowedRelations()}} {
		if v[0] < 0 || v[1] <= 0 || v[0] > v[1] {
			return false
		}
	}
	a, b := in.GetEnvelope(), max.GetEnvelope()
	if a == nil || b == nil {
		return false
	}
	for _, v := range [][2]int64{{a.GetVpnRetainedFlows(), b.GetVpnRetainedFlows()}, {a.GetGatewayRetainedFlows(), b.GetGatewayRetainedFlows()}, {a.GetVpnNewFlowsPerSecond(), b.GetVpnNewFlowsPerSecond()}, {a.GetGatewayNewFlowsPerSecond(), b.GetGatewayNewFlowsPerSecond()}, {a.GetVpnPacketsPerSecond(), b.GetVpnPacketsPerSecond()}, {a.GetGatewayPacketsPerSecond(), b.GetGatewayPacketsPerSecond()}} {
		if v[0] < 0 || v[1] <= 0 || v[0] > v[1] {
			return false
		}
	}
	for _, v := range [][2]float64{{a.GetVpnPayloadMbps(), b.GetVpnPayloadMbps()}, {a.GetGatewayPayloadMbps(), b.GetGatewayPayloadMbps()}} {
		if math.IsNaN(v[0]) || math.IsNaN(v[1]) || math.IsInf(v[0], 0) || math.IsInf(v[1], 0) || v[0] < 0 || v[1] <= 0 || v[0] > v[1] {
			return false
		}
	}
	return in.GetInternetLabs() <= in.GetMaxActiveLabs()
}
func evaluateFormula(f *protobuf.GroupPodFormula, in *protobuf.GroupSizingInputs, flows int64) (*protobuf.PodSize, error) {
	if f == nil || f.GetBase() == nil || f.GetFloor() == nil || f.GetRoundTo() == nil {
		return nil, fmt.Errorf("profile formula requires base, floor and round_to")
	}
	result := &protobuf.PodSize{}
	for _, dimension := range []struct {
		get func(*protobuf.PodSize) int64
		dst *int64
	}{
		{func(p *protobuf.PodSize) int64 { return p.GetCpuMillicores() }, &result.CpuMillicores},
		{func(p *protobuf.PodSize) int64 { return p.GetMemoryBytes() }, &result.MemoryBytes},
	} {
		total := dimension.get(f.GetBase())
		if total < 0 {
			return nil, fmt.Errorf("negative formula base")
		}
		for _, term := range []struct {
			p *protobuf.PodSize
			n int64
		}{{f.GetPerUser(), in.GetMaxUsers()}, {f.GetPerActiveLab(), in.GetMaxActiveLabs()}, {f.GetPerInternetLab(), in.GetInternetLabs()}, {f.GetPerAllowedRelation(), in.GetAllowedRelations()}, {f.GetPerRetainedFlow(), flows}} {
			coefficient := dimension.get(term.p)
			if coefficient < 0 || term.n < 0 || coefficient > 0 && term.n > (math.MaxInt64-total)/coefficient {
				return nil, fmt.Errorf("formula coefficient negative or sum overflow")
			}
			total += coefficient * term.n
		}
		floor, round := dimension.get(f.GetFloor()), dimension.get(f.GetRoundTo())
		if floor <= 0 || round <= 0 {
			return nil, fmt.Errorf("formula floor and round_to must be positive")
		}
		total = max(total, floor)
		if remainder := total % round; remainder != 0 {
			if total > math.MaxInt64-(round-remainder) {
				return nil, fmt.Errorf("formula rounding overflow")
			}
			total += round - remainder
		}
		*dimension.dst = total
	}
	return result, nil
}

// TestedPoint is informational. Its missing complete bounded traffic envelope
// and TESTED_POINT state make it ineligible for resource selection.
func TestedPoint() *protobuf.GroupPodsSizingProfile {
	vpn := &protobuf.PodSize{CpuMillicores: 50, MemoryBytes: 80 * 1024 * 1024}
	gateway := &protobuf.PodSize{CpuMillicores: 25, MemoryBytes: 32 * 1024 * 1024}
	return &protobuf.GroupPodsSizingProfile{
		Id: "native-five-peer-2026-10-08", SupportState: "TESTED_POINT",
		MaxInputs:            &protobuf.GroupSizingInputs{MaxUsers: 5, MaxActiveLabs: 13, InternetLabs: 10, AllowedRelations: 65},
		Vpn:                  &protobuf.GroupPodFormula{Base: vpn, Floor: vpn, RoundTo: &protobuf.PodSize{CpuMillicores: 1, MemoryBytes: 1024 * 1024}},
		Gateway:              &protobuf.GroupPodFormula{Base: gateway, Floor: gateway, RoundTo: &protobuf.PodSize{CpuMillicores: 1, MemoryBytes: 1024 * 1024}},
		ValidationProvenance: "2026-10-08 local-proof: paced-ramp-hold-arithmetic.json, vpn-native-paced.jsonl, gateway-native.jsonl, traffic-report.json. VPN repeat 30689/30689; initial refresh burst lost 6061/295200 replies and CPU throttled. Gateway 180000/180000 over 180s at 1000pps. Lifetime VPN peak 52.301Mi includes both attempts; gateway peak 14.066Mi; no OOM/max events.",
		Scope:                "One isolated Kubernetes 1.37 /lab API/WG/conntrack/gateway process point, conditional on the tested traffic envelope; no operator/CNI/OVS/snapshot restore, lifecycle readiness or general capacity certification. Defaults unchanged.",
	}
}
