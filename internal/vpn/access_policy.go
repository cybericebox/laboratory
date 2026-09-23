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
	VPNCIDR string
	Ready   bool
}

// AccessRule permits packets from one WireGuard client to one lab VPN CIDR.
// Response traffic is handled by the established-connection firewall rule.
type AccessRule struct {
	ClientName      string
	LabName         string
	SourceCIDR      string
	DestinationCIDR string
	Action          AccessAction
}

// Identifier is stable across reconciles and short enough for an iptables
// comment. It deliberately excludes network addresses, which can be reissued.
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
	for _, rule := range rules {
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
		for labName, lab := range labs {
			if !lab.Ready || lab.VPNCIDR == "" {
				continue
			}
			rule := AccessRule{ClientName: client.Name, LabName: labName, SourceCIDR: client.AssignedIP, DestinationCIDR: lab.VPNCIDR, Action: AccessDeny}
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
			rulesByRelation[rule.Identifier()] = rule
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
		return rules[i].LabName < rules[j].LabName
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
