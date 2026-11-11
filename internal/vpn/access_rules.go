package vpn

import "fmt"

// accessGateRules are the two rules of a permitted pair in the access chain (iptables-restore syntax, without the trailing newline):
// from the client to the lab, and from the lab to the client. Each jumps to the relation chain of the pair, which counts and accepts.
func accessGateRules(chain, wgIface string, r ForwardRule, fwdChain, revChain string) (fwd, rev string) {
	fwd = fmt.Sprintf("-A %s -i %s -o %s -s %s -d %s -j %s", chain, wgIface, r.LabInterface, r.ClientCIDR, r.LabCIDR, fwdChain)
	rev = fmt.Sprintf("-A %s -i %s -o %s -d %s%s -j %s", chain, r.LabInterface, wgIface, r.ClientCIDR, labDirectionMatch(r), revChain)
	return fwd, rev
}

// labDirectionMatch is what a packet from the lab to a client must be, besides going to that client. A lab that may start
// connections towards the client (reverseConnections) forwards everything, as always. Any other lab forwards only the replies of
// connections the client started: the conntrack entry must be established or related (the ICMP errors of such a flow) AND the packet
// must travel in the entry's reply direction. The direction matters: a connection that a device started before the switch was turned
// off (or before this rule existed) is established too, but its packets go in the original direction, and so are dropped. A packet
// that matches nothing falls to the final DROP of the access chain, and a dropped first packet leaves no conntrack entry behind.
func labDirectionMatch(r ForwardRule) string {
	if r.LabInitiated {
		return ""
	}
	return " -m conntrack --ctstate ESTABLISHED,RELATED --ctdir REPLY"
}
