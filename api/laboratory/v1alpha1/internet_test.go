package v1alpha1

import "testing"

func TestInternetConflictsAreSharedBroadcastDomains(t *testing.T) {
	ep := func(d, i string) EndpointSpec { return EndpointSpec{Device: d, Interface: i} }
	spec := func(conns ...ConnectionTemplate) *LabSpec {
		return &LabSpec{
			Gateways: []GatewayTemplate{{Name: "out", Kind: GatewayKindInternet}, {Name: "vpn", Kind: GatewayKindVPN}},
			Devices: []DeviceTemplate{
				{Name: "bot", Type: DeviceTypeContainer, Internet: InternetDeny},
				{Name: "pc", Type: DeviceTypeContainer},
				{Name: "sw", Type: DeviceTypeUnmanagedSwitch},
				{Name: "sw2", Type: DeviceTypeHub},
			},
			Connections: conns,
		}
	}
	c := func(a, b EndpointSpec) ConnectionTemplate { return ConnectionTemplate{Endpoints: []EndpointSpec{a, b}} }
	for name, tc := range map[string]struct {
		spec *LabSpec
		want int
	}{
		"no connections":   {spec(), 0},
		"directly":         {spec(c(ep("out", "eth0"), ep("bot", "eth0"))), 1},
		"through a switch": {spec(c(ep("out", "eth0"), ep("sw", "GigabitEthernet0/1")), c(ep("bot", "eth0"), ep("sw", "GigabitEthernet0/2"))), 1},
		"through two switches": {spec(
			c(ep("out", "eth0"), ep("sw", "GigabitEthernet0/1")), c(ep("sw", "GigabitEthernet0/2"), ep("sw2", "GigabitEthernet0/1")),
			c(ep("bot", "eth0"), ep("sw2", "GigabitEthernet0/2"))), 1},
		"another network": {spec(c(ep("out", "eth0"), ep("pc", "eth0")), c(ep("bot", "eth0"), ep("sw", "GigabitEthernet0/1"))), 0},
		"a VPN network":   {spec(c(ep("vpn", "eth0"), ep("bot", "eth0"))), 0},
	} {
		if got := tc.spec.InternetConflicts(); len(got) != tc.want || (tc.want == 1 && (got[0].Device != "bot" || got[0].Gateway != "out")) {
			t.Errorf("%s: %+v, want %d", name, got, tc.want)
		}
	}
	s := spec()
	s.Devices[0].Internet = ""
	s.Connections = []ConnectionTemplate{c(ep("out", "eth0"), ep("bot", "eth0"))}
	if got := s.InternetConflicts(); len(got) != 0 {
		t.Errorf("a device without internet: deny is never in conflict: %+v", got)
	}
}
