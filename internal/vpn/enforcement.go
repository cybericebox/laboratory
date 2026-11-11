package vpn

import "strings"

// LabInitiatedBlockEnforced says whether BOTH forwarding gates of the VPN pod (iptables and nft) block, for a lab without the switch,
// connections that a device starts towards a client. It is read off the rules the gates generate for a default pair, not declared: the
// agent reports it as FeaturesResponse.vpn.reverse_connections, so the capability cannot be advertised for a gate that does not install
// the rule (or after the rule generation is changed to something weaker).
func LabInitiatedBlockEnforced() bool {
	probe := ForwardRule{LabInterface: "lab1", ClientCIDR: "10.8.0.2/32", LabCIDR: "10.8.1.0/24"} // LabInitiated is false: the default
	_, rev := accessGateRules("GATE", "wg0", probe, "F", "R")
	iptables := strings.Contains(rev, "--ctstate ESTABLISHED,RELATED") && strings.Contains(rev, "--ctdir REPLY")
	script, err := nftPlanScript(ForwardPlan{Allows: []ForwardRule{probe}}, func(string) bool { return true }, func() (string, error) { return "", nil })
	nft := err == nil &&
		strings.Contains(nftBaseScript("wg0"), "@reply_rev ct state established,related ct direction reply") &&
		strings.Contains(script, "add element ip "+nftTable+" reply_rev {") && !strings.Contains(script, "allow_rev {")
	return iptables && nft
}
