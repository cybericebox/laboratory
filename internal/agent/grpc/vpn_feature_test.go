package grpc

import (
	"testing"

	"github.com/cybericebox/laboratory/internal/vpn"
)

// The capability is the answer of the code that generates the gate rules, not a constant of the agent.
func TestVPNFeatureIsDerivedFromTheEnforcingCode(t *testing.T) {
	got := (&Handler{}).vpnFeature()
	if got.GetReverseConnections() != vpn.LabInitiatedBlockEnforced() || !got.GetReverseConnections() {
		t.Fatalf("feature %v, enforcement %v", got.GetReverseConnections(), vpn.LabInitiatedBlockEnforced())
	}
}
