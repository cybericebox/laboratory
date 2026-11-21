package vpn

import (
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"sort"
)

// ClientAccessSnapshot is the observed VPN identity of one group client.
type ClientAccessSnapshot struct {
	Name       string
	AssignedIP string
}

// LabAccessSnapshot is the minimum observed state needed to safely permit a
// VPN route. A not-ready laboratory must never receive an allow rule.
type LabAccessSnapshot struct {
	// LabName and Gateway name the VPN gateway of the lab this snapshot is for: a lab has one snapshot per VPN gateway (the map of
	// snapshots is keyed by the name of the LabVPN object, which is unique per gateway).
	LabName   string
	Gateway   string
	VPNCIDR   string
	Ready     bool
	Interface string
	// LabInitiated: the lab allows its devices to START connections towards a client (reverseConnections of the VPN gateway). False, the
	// default: from the lab to a client only replies to connections the client started pass.
	LabInitiated bool
}

// AccessRule permits packets from one WireGuard client to a subnet of one VPN gateway (connector) of a lab: the gateway's /24 or, when the
// policy narrows it, a subnet inside it (/24 to /32). A client reaches a gateway through one rule per subnet.
// Response traffic is handled by the established-connection firewall rule.
type AccessRule struct {
	ClientName string
	// GatewayName is the VPN gateway of the lab the rule is for: a client reaches every VPN gateway of a lab it may reach.
	GatewayName     string
	LabName         string
	SourceCIDR      string
	DestinationCIDR string
	LabInterface    string
	Action          AccessAction
	// LabInitiated is the switch of the lab (see LabAccessSnapshot). It is not part of Identifier: switching it changes how the pair
	// is forwarded, not which pair it is, so the pair keeps its counters.
	LabInitiated bool
}

// Identifier is stable across reconciles and short enough for an iptables
// comment. It deliberately excludes network addresses, which can be reissued. It names the access of a client to a LAB: the counters and
// the traffic ledger are kept per client and lab, so the rules for the several VPN gateways of one lab share it.
func (r AccessRule) Identifier() string {
	sum := sha256.Sum256([]byte(string(r.Action) + "\x00" + r.ClientName + "\x00" + r.LabName))
	return hex.EncodeToString(sum[:8])
}

// TrafficCounter is the cumulative kernel counter attached to one firewall
// relation.
type TrafficCounter struct {
	Packets int64
	Bytes   int64
}

// AccessStatistics is safe monitoring data for one client-to-lab relation.
// A counter reset is explicit instead of becoming a negative delta later.
type AccessStatistics struct {
	ClientName   string
	LabName      string
	Action       AccessAction
	Packets      int64
	Bytes        int64
	CounterReset bool
	// GatewayNames are the connectors of the lab the client may reach (the counters are the client's in the lab, over all of them).
	GatewayNames []string
}

// ProjectAccessStatistics associates kernel counters with the configured
// relation. Current counters are cumulative; previous lets the caller surface
// an explicit reset when iptables was recreated.
func ProjectAccessStatistics(rules []AccessRule, current map[string]TrafficCounter, previous map[string]TrafficCounter) []AccessStatistics {
	stats := make([]AccessStatistics, 0, len(rules))
	seen := map[string]bool{}
	// A row lists the connectors the client may reach; a row with none lists the denied ones. A lab with an allowed and a denied connector is
	// one allow row: the counters are the client's in the lab, over all its connectors.
	allowed, denied := map[string][]string{}, map[string][]string{}
	push := func(m map[string][]string, id, g string) {
		if l := m[id]; len(l) == 0 || l[len(l)-1] != g {
			m[id] = append(l, g)
		}
	}
	for _, rule := range rules {
		if rule.Action == AccessAllow {
			push(allowed, rule.Identifier(), rule.GatewayName)
		} else {
			push(denied, rule.Identifier(), rule.GatewayName)
		}
	}
	for _, rule := range rules {
		id := rule.Identifier()
		if seen[id] {
			continue
		}
		seen[id] = true
		action, gws := AccessDeny, denied[id]
		if len(allowed[id]) > 0 {
			action, gws = AccessAllow, allowed[id]
		}
		counter := current[id]
		previousCounter := previous[id]
		stats = append(stats, AccessStatistics{
			ClientName:   rule.ClientName,
			LabName:      rule.LabName,
			Action:       action,
			Packets:      counter.Packets,
			Bytes:        counter.Bytes,
			CounterReset: counter.Packets < previousCounter.Packets || counter.Bytes < previousCounter.Bytes,
			GatewayNames: gws,
		})
	}
	return stats
}

// AccessAction determines whether a matching policy rule grants or revokes a
// route. Deny always wins when rules overlap.
type AccessAction string

const (
	AccessAllow AccessAction = "allow"
	AccessDeny  AccessAction = "deny"
)

// Include is a subnet of a gateway's /24 relative to it: <the /24>.Host/Prefix, Prefix 24..32, Host the first address of the subnet.
type Include struct{ Host, Prefix int }

// AccessPolicyRule is a group-scoped rule. Empty client selectors mean every client. An allow rule names its labs and gateways (connectors);
// a deny rule with no labs or no gateways means every lab or every gateway. Include narrows an allow rule to subnets of the gateway's /24.
type AccessPolicyRule struct {
	Action       AccessAction
	ClientNames  []string
	LabNames     []string
	GatewayNames []string
	Include      []Include
}

// BuildAccessRules derives a deterministic default-deny ACL containing each
// ready client-to-lab relation. Explicit DROP rules make blocked traffic
// observable; the chain's final implicit DROP still covers unknown traffic.
func BuildAccessRules(clients []ClientAccessSnapshot, labs map[string]LabAccessSnapshot, policy []AccessPolicyRule) []AccessRule {
	rulesByRelation := make(map[string]AccessRule)
	for _, client := range clients {
		if client.AssignedIP == "" {
			continue
		}
		for _, lab := range labs {
			labName := lab.LabName
			if !lab.Ready || lab.VPNCIDR == "" {
				continue
			}
			base := AccessRule{ClientName: client.Name, LabName: labName, GatewayName: lab.Gateway, SourceCIDR: client.AssignedIP, DestinationCIDR: lab.VPNCIDR, LabInterface: lab.Interface, Action: AccessDeny, LabInitiated: lab.LabInitiated}
			allowed, denied := false, false
			var includes []Include
			whole := false
			for _, policyRule := range policy {
				if !matchesSelector(client.Name, policyRule.ClientNames) || !matchesSelector(labName, policyRule.LabNames) || !matchesSelector(lab.Gateway, policyRule.GatewayNames) {
					continue
				}
				switch policyRule.Action {
				case AccessDeny:
					denied = true
				case AccessAllow:
					// An allow rule without gateways grants no VPN reach (only browser access, decided by the proxy per laboratory).
					if len(policyRule.LabNames) == 0 || len(policyRule.GatewayNames) == 0 {
						continue
					}
					allowed = true
					if len(policyRule.Include) == 0 {
						whole = true
					}
					includes = append(includes, policyRule.Include...)
				}
			}
			if !allowed || denied {
				rulesByRelation[base.Identifier()+"/"+base.GatewayName+"/"+base.DestinationCIDR] = base
				continue
			}
			destinations := []string{lab.VPNCIDR}
			if !whole {
				// Narrowed: only the listed subnets of the gateway's /24. An entry that does not lie inside it is dropped, never widened to the
				// whole /24; when none is left, nothing is allowed.
				destinations = includeSubnets(lab.VPNCIDR, includes)
				if len(destinations) == 0 {
					rulesByRelation[base.Identifier()+"/"+base.GatewayName+"/"+base.DestinationCIDR] = base
					continue
				}
			}
			for _, destination := range destinations {
				rule := base
				rule.Action, rule.DestinationCIDR = AccessAllow, destination
				rulesByRelation[rule.Identifier()+"/"+rule.GatewayName+"/"+rule.DestinationCIDR] = rule
			}
		}
	}
	rules := make([]AccessRule, 0, len(rulesByRelation))
	for _, rule := range rulesByRelation {
		rules = append(rules, rule)
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].ClientName != rules[j].ClientName {
			return rules[i].ClientName < rules[j].ClientName
		}
		if rules[i].LabName != rules[j].LabName {
			return rules[i].LabName < rules[j].LabName
		}
		if rules[i].GatewayName != rules[j].GatewayName {
			return rules[i].GatewayName < rules[j].GatewayName
		}
		return rules[i].DestinationCIDR < rules[j].DestinationCIDR
	})
	return rules
}

// includeSubnets turns the relative entries into the subnets of the gateway's /24, sorted and without a subnet that another one contains.
// An entry outside the /24, with a prefix outside 24..32 or not at the start of its subnet is dropped.
func includeSubnets(gatewayCIDR string, includes []Include) []string {
	gw, err := netip.ParsePrefix(gatewayCIDR)
	if err != nil || !gw.Addr().Is4() || gw.Bits() != 24 {
		return nil
	}
	var prefixes []netip.Prefix
	for _, in := range includes {
		if in.Prefix < 24 || in.Prefix > 32 || in.Host < 0 || in.Host > 255 || in.Host%(1<<(32-in.Prefix)) != 0 {
			continue
		}
		octets := gw.Masked().Addr().As4()
		octets[3] = byte(in.Host)
		prefixes = append(prefixes, netip.PrefixFrom(netip.AddrFrom4(octets), in.Prefix))
	}
	sort.Slice(prefixes, func(i, j int) bool {
		if prefixes[i].Bits() != prefixes[j].Bits() {
			return prefixes[i].Bits() < prefixes[j].Bits()
		}
		return prefixes[i].Addr().Less(prefixes[j].Addr())
	})
	var out []netip.Prefix
	for _, p := range prefixes {
		covered := false
		for _, kept := range out {
			if kept.Contains(p.Addr()) && kept.Bits() <= p.Bits() {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, p)
		}
	}
	result := make([]string, len(out))
	for i, p := range out {
		result[i] = p.String()
	}
	sort.Strings(result)
	return result
}

func matchesSelector(value string, selectors []string) bool {
	if len(selectors) == 0 {
		return true
	}
	for _, selector := range selectors {
		if selector == value {
			return true
		}
	}
	return false
}
