package v1alpha1

import "testing"

func TestGatewayLookups(t *testing.T) {
	var spec LabSpec
	if spec.HasVPN() || spec.HasInternet() || len(spec.GatewaysOfKind(GatewayKindVPN)) != 0 {
		t.Fatal("a lab without gateways has none")
	}
	spec.Gateways = []GatewayTemplate{
		{Name: "attack", Kind: GatewayKindVPN}, {Name: "out", Kind: GatewayKindInternet, DHCPServer: &DHCPServer{Enabled: true}}, {Name: "user", Kind: GatewayKindVPN},
	}
	if !spec.HasVPN() || !spec.HasInternet() {
		t.Fatal("kinds")
	}
	if got := spec.GatewaysOfKind(GatewayKindVPN); len(got) != 2 || got[0].Name != "attack" || got[1].Name != "user" {
		t.Fatalf("spec order: %v", got)
	}
	if g, ok := spec.Gateway("out"); !ok || g.Kind != GatewayKindInternet || g.DHCPServer == nil {
		t.Fatalf("lookup: %v %v", g, ok)
	}
	if _, ok := spec.Gateway("vpn"); ok {
		t.Fatal("there is no implicit gateway")
	}
}

func TestGatewayCIDR(t *testing.T) {
	st := LabStatus{Gateways: []GatewayStatus{{Name: "attack", Kind: GatewayKindVPN, CIDR: "10.128.10.0/24"}, {Name: "late", Kind: GatewayKindVPN}}}
	if got := st.GatewayCIDR("attack"); got != "10.128.10.0/24" {
		t.Fatalf("%q", got)
	}
	if st.GatewayCIDR("late") != "" || st.GatewayCIDR("missing") != "" {
		t.Fatal("no CIDR yet")
	}
}
