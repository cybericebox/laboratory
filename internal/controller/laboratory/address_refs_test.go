package laboratory

import (
	"encoding/json"
	"strings"
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func referenceLab() *laboratoryv1alpha1.Lab {
	return &laboratoryv1alpha1.Lab{
		Spec: laboratoryv1alpha1.LabSpec{
			VPN:      laboratoryv1alpha1.LabNetworkSpec{Enabled: true},
			Internet: laboratoryv1alpha1.LabNetworkSpec{Enabled: true},
		},
		Status: laboratoryv1alpha1.LabStatus{
			VPN:      laboratoryv1alpha1.LabNetworkStatus{CIDR: "10.128.7.0/24"},
			Internet: laboratoryv1alpha1.LabNetworkStatus{CIDR: "10.192.9.0/24"},
		},
	}
}

func TestResolveDeviceInterfaces(t *testing.T) {
	lab := referenceLab()
	tmpl := laboratoryv1alpha1.DeviceTemplate{Interfaces: []laboratoryv1alpha1.InterfaceSpec{{
		Name: "eth0",
		Addr: &laboratoryv1alpha1.AddrSpec{
			Type:       laboratoryv1alpha1.AddrTypeStatic,
			AddressRef: &laboratoryv1alpha1.NetworkIPRef{Network: "vpn", Host: 10},
			GatewayRef: &laboratoryv1alpha1.NetworkIPRef{Network: "vpn", Host: 1},
			Routes: []laboratoryv1alpha1.Route{{
				DstRef: &laboratoryv1alpha1.NetworkSubnetRef{Network: "internet"},
				ViaRef: &laboratoryv1alpha1.NetworkIPRef{Network: "vpn", Host: 1},
			}},
		},
	}}}

	got, err := resolveDeviceInterfaces(tmpl, lab)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 1 || got[0].Addr.IP != "10.128.7.10/24" || got[0].Addr.Gateway != "10.128.7.1" {
		t.Fatalf("unexpected resolved address and gateway: %+v", got)
	}
	if len(got[0].Addr.Routes) != 1 || got[0].Addr.Routes[0].Dst != "10.192.9.0/24" || got[0].Addr.Routes[0].Via != "10.128.7.1" {
		t.Fatalf("unexpected resolved route: %+v", got[0].Addr.Routes)
	}
	if got[0].Addr.AddressRef != nil || got[0].Addr.GatewayRef != nil || got[0].Addr.Routes[0].DstRef != nil || got[0].Addr.Routes[0].ViaRef != nil {
		t.Fatalf("materialized device still contains references: %+v", got[0].Addr)
	}
	if tmpl.Interfaces[0].Addr.AddressRef == nil || tmpl.Interfaces[0].Addr.Routes[0].DstRef == nil {
		t.Fatal("resolver mutated the lab template")
	}
	lab.Spec.VPN.DHCPServer = &laboratoryv1alpha1.DHCPServer{Enabled: true}
	if _, err := resolveDeviceInterfaces(tmpl, lab); err != nil {
		t.Fatalf("static address must remain valid with DHCP enabled: %v", err)
	}

	lab.Status.VPN.CIDR = "10.128.44.0/24"
	lab.Status.Internet.CIDR = "10.192.55.0/24"
	again, err := resolveDeviceInterfaces(tmpl, lab)
	if err != nil || again[0].Addr.IP != "10.128.44.10/24" || again[0].Addr.Routes[0].Dst != "10.192.55.0/24" {
		t.Fatalf("second lab allocation not reflected: %+v, %v", again, err)
	}
}

func TestResolveDeviceInterfacesPreservesLiteralAddresses(t *testing.T) {
	tmpl := laboratoryv1alpha1.DeviceTemplate{Interfaces: []laboratoryv1alpha1.InterfaceSpec{{
		Name: "eth0",
		Addr: &laboratoryv1alpha1.AddrSpec{
			Type: laboratoryv1alpha1.AddrTypeStatic, IP: "192.0.2.10/24", Gateway: "192.0.2.1",
			Routes: []laboratoryv1alpha1.Route{{Dst: "198.51.100.0/24", Via: "192.0.2.254"}},
		},
	}}}
	got, err := resolveDeviceInterfaces(tmpl, referenceLab())
	if err != nil || got[0].Addr.IP != "192.0.2.10/24" || got[0].Addr.Gateway != "192.0.2.1" || got[0].Addr.Routes[0].Dst != "198.51.100.0/24" || got[0].Addr.Routes[0].Via != "192.0.2.254" {
		t.Fatalf("literal address changed: %+v, %v", got, err)
	}
}

func TestResolveDeviceInterfacesRejectsInvalidReferences(t *testing.T) {
	tests := []struct {
		name string
		edit func(*laboratoryv1alpha1.Lab, *laboratoryv1alpha1.AddrSpec)
	}{
		{"gateway host as interface address", func(_ *laboratoryv1alpha1.Lab, a *laboratoryv1alpha1.AddrSpec) { a.AddressRef.Host = 1 }},
		{"broadcast host", func(_ *laboratoryv1alpha1.Lab, a *laboratoryv1alpha1.AddrSpec) { a.AddressRef.Host = 255 }},
		{"disabled source", func(l *laboratoryv1alpha1.Lab, _ *laboratoryv1alpha1.AddrSpec) { l.Spec.VPN.Enabled = false }},
		{"missing allocation", func(l *laboratoryv1alpha1.Lab, _ *laboratoryv1alpha1.AddrSpec) { l.Status.VPN.CIDR = "" }},
		{"not a /24", func(l *laboratoryv1alpha1.Lab, _ *laboratoryv1alpha1.AddrSpec) { l.Status.VPN.CIDR = "10.128.0.0/16" }},
		{"literal and reference", func(_ *laboratoryv1alpha1.Lab, a *laboratoryv1alpha1.AddrSpec) { a.IP = "10.128.7.10/24" }},
		{"bad gateway host", func(_ *laboratoryv1alpha1.Lab, a *laboratoryv1alpha1.AddrSpec) {
			a.GatewayRef = &laboratoryv1alpha1.NetworkIPRef{Network: "vpn", Host: 0}
		}},
		{"mixed route family", func(_ *laboratoryv1alpha1.Lab, a *laboratoryv1alpha1.AddrSpec) {
			a.Routes = []laboratoryv1alpha1.Route{{Dst: "2001:db8::/64", ViaRef: &laboratoryv1alpha1.NetworkIPRef{Network: "vpn", Host: 1}}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lab := referenceLab()
			addr := laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeStatic, AddressRef: &laboratoryv1alpha1.NetworkIPRef{Network: "vpn", Host: 10}}
			tt.edit(lab, &addr)
			_, err := resolveDeviceInterfaces(laboratoryv1alpha1.DeviceTemplate{Interfaces: []laboratoryv1alpha1.InterfaceSpec{{Name: "eth0", Addr: &addr}}}, lab)
			if err == nil {
				t.Fatal("invalid reference accepted")
			}
		})
	}
}

func TestResolveLabDeviceInterfacesRejectsDuplicateBeforeMaterialization(t *testing.T) {
	lab := referenceLab()
	lab.Spec.Devices = []laboratoryv1alpha1.DeviceTemplate{
		{Name: "first", Interfaces: []laboratoryv1alpha1.InterfaceSpec{{
			Name: "eth0",
			Addr: &laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeStatic,
				AddressRef: &laboratoryv1alpha1.NetworkIPRef{Network: "vpn", Host: 10}},
		}}},
		{Name: "second", Interfaces: []laboratoryv1alpha1.InterfaceSpec{{
			Name: "eth0",
			Addr: &laboratoryv1alpha1.AddrSpec{Type: laboratoryv1alpha1.AddrTypeStatic,
				AddressRef: &laboratoryv1alpha1.NetworkIPRef{Network: "vpn", Host: 10}},
		}}},
	}
	got, err := resolveLabDeviceInterfaces(lab)
	if err == nil || got != nil {
		t.Fatalf("duplicate address should prevent all Device materialization: %+v, %v", got, err)
	}
}

// An interface without an address (platform IP type "none") has a nil Addr: it resolves untouched,
// serializes without an "addr" key and gets no netconfig entry or DHCP capabilities.
func TestInterfaceWithoutAddress(t *testing.T) {
	iface := laboratoryv1alpha1.InterfaceSpec{Name: "eth0"}
	raw, err := json.Marshal(iface)
	if err != nil || strings.Contains(string(raw), "addr") {
		t.Fatalf("marshal = %s, %v; want no addr key", raw, err)
	}
	var back laboratoryv1alpha1.InterfaceSpec
	if err := json.Unmarshal(raw, &back); err != nil || back.Addr != nil || back.Name != "eth0" {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	tmpl := laboratoryv1alpha1.DeviceTemplate{Name: "web", Interfaces: []laboratoryv1alpha1.InterfaceSpec{iface}}
	lab := referenceLab()
	lab.Spec.Devices = []laboratoryv1alpha1.DeviceTemplate{tmpl}
	got, err := resolveLabDeviceInterfaces(lab)
	if err != nil || len(got["web"]) != 1 || got["web"][0].Addr != nil {
		t.Fatalf("resolve = %+v, %v", got, err)
	}
	dev := &laboratoryv1alpha1.Device{Spec: laboratoryv1alpha1.DeviceSpec{Interfaces: got["web"]}}
	if deviceHasInImageDHCP(dev) {
		t.Fatal("no address must not imply DHCP")
	}
	if c := (&DeviceReconciler{}).netConfigInitContainer(dev); c != nil {
		t.Fatalf("no address must not need a netconfig container, got %+v", c)
	}
}
