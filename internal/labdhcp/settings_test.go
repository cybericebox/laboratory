package labdhcp

import (
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestSettingsArePerGateway(t *testing.T) {
	lab := &laboratoryv1alpha1.Lab{Spec: laboratoryv1alpha1.LabSpec{Gateways: []laboratoryv1alpha1.GatewayTemplate{
		{Name: "attack", Kind: laboratoryv1alpha1.GatewayKindVPN, DHCPServer: &laboratoryv1alpha1.DHCPServer{
			Enabled: true, Ranges: []laboratoryv1alpha1.DHCPRange{{Start: 2, End: 10}},
		}},
		{Name: "out", Kind: laboratoryv1alpha1.GatewayKindInternet, DHCPServer: &laboratoryv1alpha1.DHCPServer{
			Enabled: true, Ranges: []laboratoryv1alpha1.DHCPRange{{Start: 20, End: 50}, {Start: 100, End: 150}}, DNS: "1.1.1.1",
		}},
		{Name: "plain", Kind: laboratoryv1alpha1.GatewayKindVPN},
	}}}
	vpnRanges, vpnDNS, err := Settings(lab, "attack")
	if err != nil || len(vpnRanges) != 1 || vpnDNS != "" {
		t.Fatalf("VPN DHCP settings = %+v, %q, %v", vpnRanges, vpnDNS, err)
	}
	internetRanges, internetDNS, err := Settings(lab, "out")
	if err != nil || len(internetRanges) != 2 || internetRanges[1].Start != 100 || internetDNS != "1.1.1.1" {
		t.Fatalf("internet DHCP settings = %+v, %q, %v", internetRanges, internetDNS, err)
	}
	if _, _, err := Settings(lab, "plain"); err == nil {
		t.Fatal("a gateway without DHCP has no settings")
	}
	if _, _, err := Settings(lab, "nope"); err == nil {
		t.Fatal("unknown gateway")
	}
}

func TestSettingsRejectInvalidRangesAndVPNDNS(t *testing.T) {
	lab := &laboratoryv1alpha1.Lab{Spec: laboratoryv1alpha1.LabSpec{Gateways: []laboratoryv1alpha1.GatewayTemplate{
		{Name: "attack", Kind: laboratoryv1alpha1.GatewayKindVPN, DHCPServer: &laboratoryv1alpha1.DHCPServer{Enabled: true, DNS: "1.1.1.1"}},
		{Name: "out", Kind: laboratoryv1alpha1.GatewayKindInternet, DHCPServer: &laboratoryv1alpha1.DHCPServer{Enabled: true, Ranges: []laboratoryv1alpha1.DHCPRange{{Start: 1, End: 254}}}},
	}}}
	if _, _, err := Settings(lab, "attack"); err == nil {
		t.Fatal("VPN DNS accepted")
	}
	if _, _, err := Settings(lab, "out"); err == nil {
		t.Fatal("invalid range accepted")
	}
}
