package laboratory

import (
	"net/netip"
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func staticIface(name, ip, gateway string, routes ...laboratoryv1alpha1.Route) laboratoryv1alpha1.InterfaceSpec {
	return laboratoryv1alpha1.InterfaceSpec{Name: name, Addr: &laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeStatic, IP: ip, Gateway: gateway, Routes: routes}}
}

func endpoint(device, iface string) laboratoryv1alpha1.EndpointSpec {
	return laboratoryv1alpha1.EndpointSpec{Device: device, Interface: iface}
}

func returnRouteLab() *laboratoryv1alpha1.Lab {
	lab := referenceLab()
	lab.Spec.Devices = []laboratoryv1alpha1.DeviceTemplate{
		{Name: "direct", Type: laboratoryv1alpha1.DeviceTypeContainer},
		{Name: "behind", Type: laboratoryv1alpha1.DeviceTypeContainer},
		{Name: "inner", Type: laboratoryv1alpha1.DeviceTypeContainer},
		{Name: "own", Type: laboratoryv1alpha1.DeviceTypeContainer},
		{Name: "dflt", Type: laboratoryv1alpha1.DeviceTypeContainer},
		{Name: "dhcp", Type: laboratoryv1alpha1.DeviceTypeContainer},
		{Name: "sw1", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch},
		{Name: "sw2", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch},
	}
	lab.Spec.Connections = []laboratoryv1alpha1.ConnectionTemplate{
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("direct", "eth0"), endpoint("vpn", "")}},
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("sw1", "GigabitEthernet0/1"), endpoint("vpn", "")}},
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("sw1", "GigabitEthernet0/2"), endpoint("sw2", "GigabitEthernet0/1")}},
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("behind", "eth0"), endpoint("sw2", "GigabitEthernet0/2")}},
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("own", "eth0"), endpoint("vpn", "")}},
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("dflt", "eth0"), endpoint("vpn", "")}},
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("dhcp", "eth0"), endpoint("vpn", "")}},
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("inner", "eth0"), endpoint("direct", "eth1")}},
	}
	return lab
}

func TestVPNReturnRouteIsAddedToInterfacesOnTheVPNNetwork(t *testing.T) {
	lab := returnRouteLab()
	clients := netip.MustParsePrefix("10.128.0.0/24")
	elsewhere := laboratoryv1alpha1.Route{Dst: "10.128.0.0/10", Via: "10.128.7.5"}
	resolved := map[string][]laboratoryv1alpha1.InterfaceSpec{
		"direct": {staticIface("eth0", "10.128.7.10/24", ""), staticIface("eth1", "10.50.0.1/24", "")},
		"behind": {staticIface("eth0", "10.128.7.11/24", "")},
		"inner":  {staticIface("eth0", "10.50.0.2/24", "")},
		"own":    {staticIface("eth0", "10.128.7.12/24", "", elsewhere)},
		"dflt":   {staticIface("eth0", "10.128.7.13/24", "10.128.7.1")},
		"dhcp":   {{Name: "eth0", Addr: &laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeDHCPPreset}}},
	}
	addVPNReturnRoutes(lab, resolved, clients)

	want := laboratoryv1alpha1.Route{Dst: "10.128.0.0/24", Via: "10.128.7.1"}
	for _, c := range []struct {
		device, iface string
		routes        []laboratoryv1alpha1.Route
	}{
		{"direct", "eth0", []laboratoryv1alpha1.Route{want}},
		{"direct", "eth1", nil},                              // not on the VPN network
		{"behind", "eth0", []laboratoryv1alpha1.Route{want}}, // through two switches
		{"inner", "eth0", nil},
		{"own", "eth0", []laboratoryv1alpha1.Route{elsewhere}}, // the author's route covers the clients
		{"dflt", "eth0", nil},                                  // the VPN gateway is the default gateway
	} {
		for _, iface := range resolved[c.device] {
			if iface.Name != c.iface {
				continue
			}
			got := iface.Addr.Routes
			if len(got) != len(c.routes) || (len(got) > 0 && got[0] != c.routes[0]) {
				t.Errorf("%s.%s routes = %v, want %v", c.device, c.iface, got, c.routes)
			}
		}
	}
	if len(resolved["dhcp"][0].Addr.Routes) != 0 {
		t.Error("a DHCP interface got a static route")
	}
}

func TestVPNReturnRouteNeedsVPNAndClientSubnet(t *testing.T) {
	lab := returnRouteLab()
	resolved := func() map[string][]laboratoryv1alpha1.InterfaceSpec {
		return map[string][]laboratoryv1alpha1.InterfaceSpec{"direct": {staticIface("eth0", "10.128.7.10/24", "")}}
	}
	r := resolved()
	addVPNReturnRoutes(lab, r, netip.Prefix{})
	if len(r["direct"][0].Addr.Routes) != 0 {
		t.Error("route added without a client subnet")
	}
	lab.Spec.VPN.Enabled = false
	addVPNReturnRoutes(lab, r, netip.MustParsePrefix("10.128.0.0/24"))
	if len(r["direct"][0].Addr.Routes) != 0 {
		t.Error("route added in a lab without VPN")
	}
	if got, err := vpnClientSubnet("10.128.0.0/10"); err != nil || got.String() != "10.128.0.0/24" {
		t.Errorf("client subnet = %v, %v", got, err)
	}
}
