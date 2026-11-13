package vpn

import (
	"crypto/sha256"
	"encoding/hex"
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

// AccessRule permits packets from one WireGuard client to one lab VPN CIDR.
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
}

// ProjectAccessStatistics associates kernel counters with the configured
// relation. Current counters are cumulative; previous lets the caller surface
// an explicit reset when iptables was recreated.
func ProjectAccessStatistics(rules []AccessRule, current map[string]TrafficCounter, previous map[string]TrafficCounter) []AccessStatistics {
	stats := make([]AccessStatistics, 0, len(rules))
	seen := map[string]bool{}
	for _, rule := range rules {
		// The rows are per client and lab: the rules for the several VPN gateways of a lab are one row.
		if seen[rule.Identifier()] {
			continue
		}
		seen[rule.Identifier()] = true
		counter := current[rule.Identifier()]
		previousCounter := previous[rule.Identifier()]
		stats = append(stats, AccessStatistics{
			ClientName:   rule.ClientName,
			LabName:      rule.LabName,
			Action:       rule.Action,
			Packets:      counter.Packets,
			Bytes:        counter.Bytes,
			CounterReset: counter.Packets < previousCounter.Packets || counter.Bytes < previousCounter.Bytes,
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

// AccessPolicyRule is a group-scoped rule. Empty client or lab selectors mean
// every client or every lab respectively.
type AccessPolicyRule struct {
	Action      AccessAction
	ClientNames []string
	LabNames    []string
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
			rule := AccessRule{ClientName: client.Name, LabName: labName, GatewayName: lab.Gateway, SourceCIDR: client.AssignedIP, DestinationCIDR: lab.VPNCIDR, LabInterface: lab.Interface, Action: AccessDeny, LabInitiated: lab.LabInitiated}
			allowed := false
			denied := false
			for _, policyRule := range policy {
				if !matchesSelector(client.Name, policyRule.ClientNames) || !matchesSelector(labName, policyRule.LabNames) {
					continue
				}
				switch policyRule.Action {
				case AccessDeny:
					denied = true
				case AccessAllow:
					allowed = true
				}
			}
			if allowed && !denied {
				rule.Action = AccessAllow
			}
			rulesByRelation[rule.Identifier()+"/"+rule.GatewayName] = rule
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
		return rules[i].GatewayName < rules[j].GatewayName
	})
	return rules
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
