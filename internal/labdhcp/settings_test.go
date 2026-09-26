package labdhcp

import (
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestSettingsArePerLabNetwork(t *testing.T) {
	lab := &laboratoryv1alpha1.Lab{Spec: laboratoryv1alpha1.LabSpec{
		VPN: laboratoryv1alpha1.LabNetworkSpec{DHCPServer: &laboratoryv1alpha1.DHCPServer{
			Enabled: true, Ranges: []laboratoryv1alpha1.DHCPRange{{Start: 2, End: 10}},
		}},
		Internet: laboratoryv1alpha1.LabNetworkSpec{DHCPServer: &laboratoryv1alpha1.DHCPServer{
			Enabled: true, Ranges: []laboratoryv1alpha1.DHCPRange{{Start: 20, End: 50}, {Start: 100, End: 150}}, DNS: "1.1.1.1",
		}},
	}}
	vpnRanges, vpnDNS, err := Settings(lab, "vpn")
	if err != nil || len(vpnRanges) != 1 || vpnDNS != "" {
		t.Fatalf("VPN DHCP settings = %+v, %q, %v", vpnRanges, vpnDNS, err)
	}
	internetRanges, internetDNS, err := Settings(lab, "internet")
	if err != nil || len(internetRanges) != 2 || internetRanges[1].Start != 100 || internetDNS != "1.1.1.1" {
		t.Fatalf("internet DHCP settings = %+v, %q, %v", internetRanges, internetDNS, err)
	}
}

func TestSettingsRejectInvalidRangesAndVPNDNS(t *testing.T) {
	lab := &laboratoryv1alpha1.Lab{Spec: laboratoryv1alpha1.LabSpec{
		VPN:      laboratoryv1alpha1.LabNetworkSpec{DHCPServer: &laboratoryv1alpha1.DHCPServer{Enabled: true, DNS: "1.1.1.1"}},
		Internet: laboratoryv1alpha1.LabNetworkSpec{DHCPServer: &laboratoryv1alpha1.DHCPServer{Enabled: true, Ranges: []laboratoryv1alpha1.DHCPRange{{Start: 1, End: 254}}}},
	}}
	if _, _, err := Settings(lab, "vpn"); err == nil {
		t.Fatal("VPN DNS accepted")
	}
	if _, _, err := Settings(lab, "internet"); err == nil {
		t.Fatal("invalid range accepted")
	}
}
