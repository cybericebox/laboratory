package reconciler

import "github.com/cybericebox/laboratory/internal/vpn"

func allowLabs(labs ...string) []vpn.AccessPolicyRule {
	return []vpn.AccessPolicyRule{{Action: vpn.AccessAllow, LabNames: labs, GatewayNames: []string{"vpn"}}}
}
