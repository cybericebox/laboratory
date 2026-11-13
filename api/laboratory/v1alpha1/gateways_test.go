package v1alpha1

import "testing"

func TestAllGatewaysListsTheShorthandOnesFirst(t *testing.T) {
	var spec LabSpec
	if got := spec.AllGateways(); len(got) != 0 || spec.HasVPN() || spec.HasInternet() {
		t.Fatalf("a lab without gateways has none: %v", got)
	}
	spec.Internet = LabNetworkSpec{Enabled: true, DHCPServer: &DHCPServer{Enabled: true, DNS: "1.1.1.1"}}
	spec.VPN = LabNetworkSpec{Enabled: true, ReverseConnections: true}
	spec.Gateways = []GatewayTemplate{{Name: "attack", Kind: GatewayKindVPN}, {Name: "out-east", Kind: GatewayKindInternet}}
	all := spec.AllGateways()
	if len(all) != 4 || all[0].Name != "vpn" || all[1].Name != "internet" || all[2].Name != "attack" || all[3].Name != "out-east" {
		t.Fatalf("order: %v", all)
	}
	if !all[0].ReverseConnections || all[1].DHCPServer == nil || all[1].Kind != GatewayKindInternet {
		t.Fatalf("the shorthand settings are carried over: %+v %+v", all[0], all[1])
	}
	if !all[0].IsShorthand() || !all[1].IsShorthand() || all[2].IsShorthand() {
		t.Fatal("only vpn and internet are the shorthand gateways")
	}
	if g, ok := spec.Gateway("attack"); !ok || g.Kind != GatewayKindVPN {
		t.Fatalf("lookup by name: %v %v", g, ok)
	}
	if _, ok := spec.Gateway("nope"); ok {
		t.Fatal("unknown name")
	}
}

func TestAShorthandGatewayThatIsOffIsNotListed(t *testing.T) {
	spec := LabSpec{Gateways: []GatewayTemplate{{Name: "attack", Kind: GatewayKindVPN}}}
	if got := spec.AllGateways(); len(got) != 1 || got[0].Name != "attack" {
		t.Fatalf("%v", got)
	}
	if !spec.HasVPN() || spec.HasInternet() {
		t.Fatal("kinds")
	}
	if _, ok := spec.Gateway("vpn"); ok {
		t.Fatal("spec.vpn is off: there is no gateway named vpn")
	}
}

func TestGatewayCIDRFallsBackToTheOldStatusFields(t *testing.T) {
	st := LabStatus{VPN: LabNetworkStatus{CIDR: "10.128.5.0/24"}, Internet: LabNetworkStatus{CIDR: "10.192.5.0/24"}}
	if got := st.GatewayCIDR(GatewayKindVPN, ""); got != "10.128.5.0/24" {
		t.Fatalf("vpn: %q", got)
	}
	if got := st.GatewayCIDR(GatewayKindInternet, "internet"); got != "10.192.5.0/24" {
		t.Fatalf("internet: %q", got)
	}
	st.Gateways = []GatewayStatus{{Name: "vpn", Kind: GatewayKindVPN, CIDR: "10.128.9.0/24"}, {Name: "attack", Kind: GatewayKindVPN, CIDR: "10.128.10.0/24"}}
	if got := st.GatewayCIDR(GatewayKindVPN, ""); got != "10.128.9.0/24" {
		t.Fatalf("the entry in status.gateways wins over the old field: %q", got)
	}
	if got := st.GatewayCIDR(GatewayKindVPN, "attack"); got != "10.128.10.0/24" {
		t.Fatalf("declared gateway: %q", got)
	}
	if got := st.GatewayCIDR(GatewayKindVPN, "missing"); got != "" {
		t.Fatalf("unknown gateway: %q", got)
	}
}
