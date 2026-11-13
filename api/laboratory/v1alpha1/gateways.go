package v1alpha1

// AllGateways is every gateway of the lab: the one spec.vpn stands for, the one spec.internet stands for (each only when enabled),
// then the declared ones in spec order. It is the one list the operator works on.
func (s *LabSpec) AllGateways() []GatewayTemplate {
	out := make([]GatewayTemplate, 0, 2+len(s.Gateways))
	if s.VPN.Enabled {
		out = append(out, GatewayTemplate{Name: GatewayNameVPN, Kind: GatewayKindVPN, DHCPServer: s.VPN.DHCPServer, ReverseConnections: s.VPN.ReverseConnections})
	}
	if s.Internet.Enabled {
		out = append(out, GatewayTemplate{Name: GatewayNameInternet, Kind: GatewayKindInternet, DHCPServer: s.Internet.DHCPServer})
	}
	return append(out, s.Gateways...)
}

// Gateway finds a gateway of the lab by name.
func (s *LabSpec) Gateway(name string) (GatewayTemplate, bool) {
	for _, g := range s.AllGateways() {
		if g.Name == name {
			return g, true
		}
	}
	return GatewayTemplate{}, false
}

// HasGateway says whether the lab has at least one gateway of the kind.
func (s *LabSpec) HasGateway(kind GatewayKind) bool {
	for _, g := range s.AllGateways() {
		if g.Kind == kind {
			return true
		}
	}
	return false
}

// HasVPN says whether the lab has at least one VPN gateway.
func (s *LabSpec) HasVPN() bool { return s.HasGateway(GatewayKindVPN) }

// HasInternet says whether the lab has at least one Internet gateway.
func (s *LabSpec) HasInternet() bool { return s.HasGateway(GatewayKindInternet) }

// IsShorthand is true for the two gateways spec.vpn and spec.internet stand for. Their objects, statuses and names are the ones
// labs have always had.
func (g GatewayTemplate) IsShorthand() bool {
	return g.Name == GatewayNameVPN || g.Name == GatewayNameInternet
}

// GatewayStatus returns the status entry of the gateway, or nil when none was written yet.
func (s *LabStatus) GatewayStatus(name string) *GatewayStatus {
	for i := range s.Gateways {
		if s.Gateways[i].Name == name {
			return &s.Gateways[i]
		}
	}
	return nil
}

// GatewayCIDR is the /24 of the gateway named name (empty = the shorthand gateway of the kind), or "". A lab written before
// status.gateways still has its two CIDRs in status.vpn and status.internet; they answer for the shorthand gateways.
func (s *LabStatus) GatewayCIDR(kind GatewayKind, name string) string {
	if name == "" {
		name = string(kind)
	}
	if g := s.GatewayStatus(name); g != nil && g.CIDR != "" {
		return g.CIDR
	}
	switch name {
	case GatewayNameVPN:
		return s.VPN.CIDR
	case GatewayNameInternet:
		return s.Internet.CIDR
	}
	return ""
}
