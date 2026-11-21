package labdhcp

import (
	"fmt"
	"net"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/dhcp"
)

// Settings extracts the ranges and the optional DNS of the DHCP server of one gateway of the lab.
// VPN DHCP deliberately does not advertise DNS.
func Settings(lab *laboratoryv1alpha1.Lab, gateway string) ([]dhcp.Range, string, error) {
	g, ok := lab.Spec.Gateway(gateway)
	if !ok {
		return nil, "", fmt.Errorf("unknown gateway %q", gateway)
	}
	server := g.DHCPServer
	if server == nil || !server.Enabled {
		return nil, "", fmt.Errorf("DHCP of gateway %q is disabled", gateway)
	}
	if g.Kind == laboratoryv1alpha1.GatewayKindVPN && server.DNS != "" {
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
