package vpn

import "sort"

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
	SourceCIDR      string
	DestinationCIDR string
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

// BuildAccessRules derives a deterministic, de-duplicated default-deny ACL.
// It deliberately ignores unknown labs, clients without an allocated address,
// and labs that are not ready for VPN traffic.
func BuildAccessRules(clients []ClientAccessSnapshot, labs map[string]LabAccessSnapshot, policy []AccessPolicyRule) []AccessRule {
	allowed := make(map[AccessRule]struct{})
	denied := make(map[AccessRule]struct{})
	for _, client := range clients {
		if client.AssignedIP == "" {
			continue
		}
		for labName, lab := range labs {
			if !lab.Ready || lab.VPNCIDR == "" {
				continue
			}
			rule := AccessRule{SourceCIDR: client.AssignedIP, DestinationCIDR: lab.VPNCIDR}
			for _, policyRule := range policy {
				if !matchesSelector(client.Name, policyRule.ClientNames) || !matchesSelector(labName, policyRule.LabNames) {
					continue
				}
				switch policyRule.Action {
				case AccessDeny:
					denied[rule] = struct{}{}
				case AccessAllow:
					allowed[rule] = struct{}{}
				}
			}
		}
	}
	rules := make([]AccessRule, 0, len(allowed))
	for rule := range allowed {
		if _, blocked := denied[rule]; blocked {
			continue
		}
		rules = append(rules, rule)
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].SourceCIDR != rules[j].SourceCIDR {
			return rules[i].SourceCIDR < rules[j].SourceCIDR
		}
		return rules[i].DestinationCIDR < rules[j].DestinationCIDR
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
