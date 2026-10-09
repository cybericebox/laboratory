package labdhcp

import (
	"fmt"
	"net"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/dhcp"
)

// Settings extracts one lab-facing DHCP interface's ranges and optional DNS.
// VPN DHCP deliberately does not advertise DNS.
func Settings(lab *laboratoryv1alpha1.Lab, network string) ([]dhcp.Range, string, error) {
	var server *laboratoryv1alpha1.DHCPServer
	switch network {
	case networkVPN:
		server = lab.Spec.VPN.DHCPServer
	case "internet":
		server = lab.Spec.Internet.DHCPServer
	default:
		return nil, "", fmt.Errorf("unknown DHCP network %q", network)
	}
	if server == nil || !server.Enabled {
		return nil, "", fmt.Errorf("%s DHCP is disabled", network)
	}
	if network == networkVPN && server.DNS != "" {
		return nil, "", fmt.Errorf("VPN DHCP cannot advertise DNS")
	}
	ranges := make([]dhcp.Range, 0, len(server.Ranges))
	for _, r := range server.Ranges {
		ranges = append(ranges, dhcp.Range{Start: r.Start, End: r.End})
	}
	if err := dhcp.ValidateRanges(ranges); err != nil {
		return nil, "", err
	}
	if server.DNS != "" {
		if ip := net.ParseIP(server.DNS); ip == nil || ip.To4() == nil {
			return nil, "", fmt.Errorf("invalid internet DHCP DNS address %q", server.DNS)
		}
	}
	return ranges, server.DNS, nil
}
