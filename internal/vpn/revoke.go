package vpn

import "net/netip"

// ConnFlow is the original direction of one tracked connection: who opened it and to whom.
type ConnFlow struct {
	// ID is the kernel's identifier of the entry; Key is the caller's own handle
	// (an index into the dump the flow came from).
	ID       uint32
	Mark     uint32
	Key      int
	Src, Dst netip.Addr
}

// RevokedFlows picks the tracked connections the access rules no longer allow: the ones a
// client opened to an address in a lab network (labCIDRs) whose (client, lab) relation has no
// allow rule now. FORWARD accepts established connections before the access chain is looked
// at, so changing the chain stops new connections only; an open SSH session or download
// lives on until its entry is removed from the conntrack table, after which its next packet
// is a new connection that the chain refuses.
//
// Connections that do not start in a client's address and go to a lab (the pod's own
// traffic, connections between lab networks, anything the rules never covered) are left
// alone: they were never governed by a rule.
func RevokedFlows(flows []ConnFlow, labCIDRs []string, rules []AccessRule, clientCIDRs ...[]string) []ConnFlow {
	labs := prefixes(labCIDRs)
	if len(labs) == 0 {
		return nil
	}
	var clients []netip.Prefix
	strictClients := len(clientCIDRs) > 0
	if strictClients {
		clients = prefixes(clientCIDRs[0])
	}
	type pair struct {
		src, dst netip.Prefix
		iface    string
	}
	var allowed []pair
	for _, r := range rules {
		if r.Action != AccessAllow {
			continue
		}
		s, sok := parsePrefix(r.SourceCIDR)
		d, dok := parsePrefix(r.DestinationCIDR)
		if sok && dok {
			allowed = append(allowed, pair{s, d, r.LabInterface})
		}
	}
	var out []ConnFlow
	for _, f := range flows {
		forward := inAny(labs, f.Dst) && !inAny(labs, f.Src) && (!strictClients || inAny(clients, f.Src))
		marked := f.Mark&FlowCountedMark != 0
		reverse := strictClients && (inAny(labs, f.Src) || marked) && inAny(clients, f.Dst)
		if !forward && !reverse {
			continue
		}
		src, dst := f.Src, f.Dst
		if reverse {
			src, dst = dst, src
		}
		ok := false
		for _, a := range allowed {
			labMatches := a.dst.Contains(dst)
			if reverse && marked {
				index, err := LabInterfaceIndex(a.iface)
				labMatches = err == nil && uint32(index) == (f.Mark&LabIdentityMask)>>8
			}
			if a.src.Contains(src) && labMatches {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, f)
		}
	}
	return out
}

// parsePrefix reads "10.8.0.2/32" or a bare address (a host).
func parsePrefix(s string) (netip.Prefix, bool) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p, true
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return netip.PrefixFrom(a, a.BitLen()), true
	}
	return netip.Prefix{}, false
}

func prefixes(list []string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range list {
		if p, ok := parsePrefix(s); ok {
			out = append(out, p)
		}
	}
	return out
}

func inAny(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
