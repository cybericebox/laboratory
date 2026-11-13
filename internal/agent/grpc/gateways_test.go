package grpc

import (
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// A lab may have at most the configured number of gateways (VPN entries and Internet exits together, spec.vpn and spec.internet
// counted); the agent refuses one with more when it is created, and a lab within the limit keeps its declared gateways.
func TestCreateLabsRefusesTooManyGateways(t *testing.T) {
	h, _ := contractHandler(t, Features{MaxLabGateways: 3})
	declare := func(names ...string) func(*laboratoryv1alpha1.LabSpec) {
		return func(s *laboratoryv1alpha1.LabSpec) {
			s.VPN.Enabled = true
			for _, n := range names {
				s.Gateways = append(s.Gateways, laboratoryv1alpha1.GatewayTemplate{Name: n, Kind: laboratoryv1alpha1.GatewayKindVPN})
			}
		}
	}
	res := createContractLab(t, h, &protobuf.LabVariant{SpecJson: contractSpecJSON(t, declare("a", "b"), "web")}, &protobuf.LabItem{})
	wantStates(t, res, nil, stCreated)

	over := contractSpecJSON(t, declare("a", "b", "c"), "web")
	res2, err := h.CreateLabs(t.Context(), &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "w", SpecJson: over}},
		Items:    []*protobuf.LabItem{{LabGroup: "team-lab", Name: "c2", VariantId: "w"}}})
	invalidArg(t, res2, err, "TooManyGateways")

	reserved := contractSpecJSON(t, declare("vpn"), "web")
	res3, err := h.CreateLabs(t.Context(), &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "x", SpecJson: reserved}},
		Items:    []*protobuf.LabItem{{LabGroup: "team-lab", Name: "c3", VariantId: "x"}}})
	invalidArg(t, res3, err, "InvalidGatewayName")
}
