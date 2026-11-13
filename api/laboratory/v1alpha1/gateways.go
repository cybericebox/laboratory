package v1alpha1

// Gateway finds a gateway of the lab by name.
func (s *LabSpec) Gateway(name string) (GatewayTemplate, bool) {
	for _, g := range s.Gateways {
		if g.Name == name {
			return g, true
		}
	}
	return GatewayTemplate{}, false
}

// GatewaysOfKind lists the gateways of the lab of one kind, in spec order.
func (s *LabSpec) GatewaysOfKind(kind GatewayKind) []GatewayTemplate {
	var out []GatewayTemplate
	for _, g := range s.Gateways {
		if g.Kind == kind {
			out = append(out, g)
		}
	}
	return out
}

// HasVPN says whether the lab has at least one VPN gateway.
func (s *LabSpec) HasVPN() bool { return len(s.GatewaysOfKind(GatewayKindVPN)) > 0 }

// HasInternet says whether the lab has at least one Internet gateway.
func (s *LabSpec) HasInternet() bool { return len(s.GatewaysOfKind(GatewayKindInternet)) > 0 }

// GatewayStatus returns the status entry of the gateway, or nil when none was written yet.
func (s *LabStatus) GatewayStatus(name string) *GatewayStatus {
	for i := range s.Gateways {
		if s.Gateways[i].Name == name {
			return &s.Gateways[i]
		}
	}
	return nil
}

// GatewayCIDR is the /24 of the gateway named name, or "" while it has none.
func (s *LabStatus) GatewayCIDR(name string) string {
	if g := s.GatewayStatus(name); g != nil {
		return g.CIDR
	}
	return ""
}
