package vpn

// allowLabs admits every client to the VPN gateway "vpn" of the named labs (names that do not exist are ignored).
func allowLabs(labs ...string) []AccessPolicyRule {
	return []AccessPolicyRule{{Action: AccessAllow, LabNames: labs, GatewayNames: []string{"vpn"}}}
}
