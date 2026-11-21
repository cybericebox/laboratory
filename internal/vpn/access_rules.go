package vpn

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

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

// blockCommentPrefix marks the rule that drops what a lab that may not start connections sends towards the clients and no pair accepted.
// One rule per lab interface; its counter is the number of such packets.
const blockCommentPrefix = "cibblk:"

// accessBlockRules are the drop rules, one per lab interface of a lab without the switch, placed after the rules of the pairs in the access
// chain. They are not needed for the decision (the chain ends in DROP) but they count what was refused.
func accessBlockRules(chain, wgIface string, rules []ForwardRule) []string {
	seen := map[string]bool{}
	var ifaces []string
	for _, r := range rules {
		if !r.LabInitiated && !seen[r.LabInterface] {
			seen[r.LabInterface] = true
			ifaces = append(ifaces, r.LabInterface)
		}
	}
	sort.Strings(ifaces)
	out := make([]string, 0, len(ifaces))
	for _, iface := range ifaces {
		out = append(out, fmt.Sprintf("-A %s -i %s -o %s -m comment --comment %s%s -j DROP", chain, iface, wgIface, blockCommentPrefix, iface))
	}
	return out
}

var savedBlockLine = regexp.MustCompile(`^\[(\d+):\d+\] -A \S+ .*--comment "?` + blockCommentPrefix + `(lab\d+)"?(?:\s|$)`)

// parseBlockCounters reads the packet counts of the block rules from iptables-save -c output, by lab interface.
func parseBlockCounters(saved []byte) map[string]uint64 {
	out := map[string]uint64{}
	for _, line := range strings.Split(string(saved), "\n") {
		if m := savedBlockLine.FindStringSubmatch(line); m != nil {
			n, err := strconv.ParseUint(m[1], 10, 64)
			if err == nil {
				out[m[2]] += n
			}
		}
	}
	return out
}
