package vpn

import (
	"fmt"
	"strings"
)

// The scripts of the nftables forwarding of the VPN pod (nft_forward_linux.go), as plain functions of their inputs so that they can be
// tested on any system.

const nftTable = "cicevpn"

// nftAcceptMark is what the nft gate puts on a packet it lets through. The iptables FORWARD chain keeps its policy DROP and accepts ONLY a
// packet that carries this mark: whatever happens to the nft table (not yet installed, deleted by a cleanup or a crash, emptied) nothing is
// marked and nothing is forwarded. The mark exists only inside the pod's kernel; no packet arrives with it.
const nftAcceptMark = "0x00ce0000"

var accountingSets = []struct{ name, direction, kind string }{
	{"acct_fwd", "F", "T"}, {"acct_rev", "R", "T"}, {"new_fwd", "F", "N"}, {"new_rev", "R", "N"},
}

// nftBaseScript installs the table closed: the permission sets are emptied (the accounting sets survive). Permission to forward from a
// lab to a client is in one of two sets, by the switch of the lab:
//
//	allow_rev   the lab may start connections towards the client: forwarded as it is, untracked (the lab's traffic is in the notrack sets)
//	reply_rev   the lab may not: forwarded only when conntrack says it is an established or related packet in the REPLY direction, that
//	            is a reply to a connection the client started. The traffic of such a lab is therefore tracked, in both directions.
func nftBaseScript(wgIface string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table ip %s\n", nftTable)
	for _, set := range []string{"allow_fwd", "allow_rev", "reply_rev"} {
		fmt.Fprintf(&b, "add set ip %s %s { type ifname . ipv4_addr . ipv4_addr; flags interval; }\nflush set ip %s %s\n", nftTable, set, nftTable, set)
	}
	for _, s := range accountingSets {
		fmt.Fprintf(&b, "add set ip %s %s { type ifname . ipv4_addr . ipv4_addr; flags interval; counter; }\n", nftTable, s.name)
	}
	// notrack: the lab prefixes and the (lab interface, client) pairs of the labs that may start connections. Every other pair is tracked.
	fmt.Fprintf(&b, "add set ip %s labnets { type ipv4_addr; flags interval; }\nflush set ip %s labnets\n", nftTable, nftTable)
	fmt.Fprintf(&b, "add set ip %s open_clients { type ifname . ipv4_addr; }\nflush set ip %s open_clients\n", nftTable, nftTable)
	// the lab interfaces of the labs that may not start connections: what they send towards the clients and nothing accepted is dropped, and counted
	fmt.Fprintf(&b, "add set ip %s blocked_labs { type ifname; counter; }\nflush set ip %s blocked_labs\n", nftTable, nftTable)
	for _, chain := range []string{"count_fwd", "count_rev"} {
		fmt.Fprintf(&b, "add chain ip %s %s\nflush chain ip %s %s\n", nftTable, chain, nftTable, chain)
	}
	fmt.Fprintf(&b, "add rule ip %s count_fwd tcp flags & (syn | ack) == syn oifname . ip saddr . ip daddr @new_fwd\n", nftTable)
	fmt.Fprintf(&b, "add rule ip %s count_fwd oifname . ip saddr . ip daddr @acct_fwd meta mark set %s accept\n", nftTable, nftAcceptMark)
	fmt.Fprintf(&b, "add rule ip %s count_rev tcp flags & (syn | ack) == syn iifname . ip saddr . ip daddr @new_rev\n", nftTable)
	fmt.Fprintf(&b, "add rule ip %s count_rev iifname . ip saddr . ip daddr @acct_rev meta mark set %s accept\n", nftTable, nftAcceptMark)
	// raw hook: the traffic between the participants and the labs that may start connections is not tracked; the traffic of the other labs
	// is, because their replies are told from new connections by conntrack. What is addressed to the pod itself (its status page, its
	// ping) is tracked as well.
	fmt.Fprintf(&b, "add chain ip %s prerouting { type filter hook prerouting priority -300; policy accept; }\nflush chain ip %s prerouting\n", nftTable, nftTable)
	fmt.Fprintf(&b, "add rule ip %s prerouting iifname %q ip daddr @labnets notrack\n", nftTable, wgIface)
	fmt.Fprintf(&b, "add rule ip %s prerouting iifname . ip daddr @open_clients notrack\n", nftTable)
	// the gate: closed by policy; participants never reach each other through the pod
	fmt.Fprintf(&b, "add chain ip %s forward { type filter hook forward priority -1; policy drop; }\nflush chain ip %s forward\n", nftTable, nftTable)
	fmt.Fprintf(&b, "add rule ip %s forward ct state invalid drop\n", nftTable)
	fmt.Fprintf(&b, "add rule ip %s forward iifname %q oifname %q drop\n", nftTable, wgIface, wgIface)
	fmt.Fprintf(&b, "add rule ip %s forward iifname %q oifname . ip saddr . ip daddr @allow_fwd jump count_fwd\n", nftTable, wgIface)
	fmt.Fprintf(&b, "add rule ip %s forward oifname %q iifname . ip saddr . ip daddr @allow_rev jump count_rev\n", nftTable, wgIface)
	fmt.Fprintf(&b, "add rule ip %s forward oifname %q iifname . ip saddr . ip daddr @reply_rev ct state established,related ct direction reply jump count_rev\n", nftTable, wgIface)
	fmt.Fprintf(&b, "add rule ip %s forward oifname %q iifname @blocked_labs drop\n", nftTable, wgIface)
	return b.String()
}

// nftKeys are the element keys of a binding in the permission and accounting sets.
func nftKeys(r ForwardRule) (fwd, rev string) {
	return fmt.Sprintf("%q . %s . %s", r.LabInterface, strings.TrimSuffix(r.ClientCIDR, "/32"), r.LabCIDR),
		fmt.Sprintf("%q . %s . %s", r.LabInterface, r.LabCIDR, strings.TrimSuffix(r.ClientCIDR, "/32"))
}

// revSet is the permission set of the lab-to-client direction of a binding, by the switch of its lab.
func revSet(r ForwardRule) string {
	if r.LabInitiated {
		return "allow_rev"
	}
	return "reply_rev"
}

// nftPlanScript replaces the permission and notrack sets in one transaction. newEpoch gives the comment identity of the accounting
// elements of a binding that has none yet (haveEpoch says which have), so a restart keeps the counters of the pairs that stay.
func nftPlanScript(plan ForwardPlan, haveEpoch func(bindingID string) bool, newEpoch func() (string, error)) (string, error) {
	var b strings.Builder
	for _, set := range []string{"allow_fwd", "allow_rev", "reply_rev", "labnets", "open_clients", "blocked_labs"} {
		fmt.Fprintf(&b, "flush set ip %s %s\n", nftTable, set)
	}
	labnets, open, blocked := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range plan.Allows {
		fwd, rev := nftKeys(r)
		fmt.Fprintf(&b, "add element ip %s allow_fwd { %s }\nadd element ip %s %s { %s }\n", nftTable, fwd, nftTable, revSet(r), rev)
		if !r.LabInitiated && !blocked[r.LabInterface] {
			blocked[r.LabInterface] = true
			fmt.Fprintf(&b, "add element ip %s blocked_labs { %q counter }\n", nftTable, r.LabInterface)
		}
		if r.LabInitiated {
			if !labnets[r.LabCIDR] {
				labnets[r.LabCIDR] = true
				fmt.Fprintf(&b, "add element ip %s labnets { %s }\n", nftTable, r.LabCIDR)
			}
			client := strings.TrimSuffix(r.ClientCIDR, "/32")
			if key := fmt.Sprintf("%q . %s", r.LabInterface, client); !open[key] {
				open[key] = true
				fmt.Fprintf(&b, "add element ip %s open_clients { %s }\n", nftTable, key)
			}
		}
		if !haveEpoch(r.BindingID) {
			epoch, err := newEpoch()
			if err != nil {
				return "", err
			}
			comment := "cibacct:" + r.BindingID + ":" + epoch
			for _, s := range accountingSets {
				key := fwd
				if s.direction == "R" {
					key = rev
				}
				fmt.Fprintf(&b, "add element ip %s %s { %s counter comment %q }\n", nftTable, s.name, key, comment)
			}
		}
	}
	return b.String(), nil
}
