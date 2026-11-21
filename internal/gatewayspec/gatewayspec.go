// Package gatewayspec checks what a Lab declares as gateways (spec.gateways). The agent runs it when a lab
// is created and the operator runs it again, so a Lab made by hand or by an older agent is stopped too.
package gatewayspec

import (
	"fmt"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// DefaultMax is the most gateways one lab may have when nothing else is configured (the chart's operator.maxLabGateways).
const DefaultMax = 10

// Validate says whether the gateways of the spec are acceptable: at most limit of them (VPN and Internet together; limit <= 0 means
// DefaultMax), each with a valid name that is not the name of a device and not used twice, and a known kind; reverseConnections only on
// a VPN gateway. The errors start with a code word, as the operator's other
// validation errors do.
func Validate(spec *laboratoryv1alpha1.LabSpec, limit int) error {
	if limit <= 0 {
		limit = DefaultMax
	}
	if len(spec.Gateways) > limit {
		return fmt.Errorf("TooManyGateways: the lab has %d gateways (VPN and Internet together), the limit is %d", len(spec.Gateways), limit)
	}
	deviceNames := make(map[string]bool, len(spec.Devices))
	for _, d := range spec.Devices {
		deviceNames[d.Name] = true
	}
	seen := map[string]bool{}
	for _, g := range spec.Gateways {
		if err := names.ValidateDeviceName(g.Name); err != nil {
			return fmt.Errorf("InvalidGatewayName: %w", err)
		}
		if deviceNames[g.Name] {
			return fmt.Errorf("DuplicateGatewayName: %q is the name of a device and of a gateway", g.Name)
		}
		if seen[g.Name] {
			return fmt.Errorf("DuplicateGatewayName: gateway %q is declared more than once", g.Name)
		}
		seen[g.Name] = true
		switch g.Kind {
		case laboratoryv1alpha1.GatewayKindVPN:
		case laboratoryv1alpha1.GatewayKindInternet:
			if g.ReverseConnections {
				return fmt.Errorf("InvalidGateway: gateway %q: reverseConnections belongs to a vpn gateway", g.Name)
			}
		default:
			return fmt.Errorf("InvalidGateway: gateway %q has the unknown kind %q", g.Name, g.Kind)
		}
		if g.DHCPServer != nil && g.DHCPServer.Enabled && len(g.DHCPServer.Ranges) == 0 {
			return fmt.Errorf("InvalidGateway: gateway %q: an enabled DHCP server needs at least one range", g.Name)
		}
	}
	return nil
}
