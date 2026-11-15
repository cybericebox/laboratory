package vpn

import (
	"sort"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// PolicyRules turns the spec of a LabGroupAccessPolicy into the rules BuildAccessRules works with. A rule with an unknown action is left out.
func PolicyRules(policy *laboratoryv1alpha1.LabGroupAccessPolicy) []AccessPolicyRule {
	rules := make([]AccessPolicyRule, 0, len(policy.Spec.Rules))
	for _, rule := range policy.Spec.Rules {
		var action AccessAction
		switch rule.Action {
		case laboratoryv1alpha1.LabGroupAccessAllow:
			action = AccessAllow
		case laboratoryv1alpha1.LabGroupAccessDeny:
			action = AccessDeny
		default:
			continue
		}
		var include []Include
		for _, in := range rule.Include {
			include = append(include, Include{Host: int(in.Host), Prefix: int(in.Prefix)})
		}
		rules = append(rules, AccessPolicyRule{Action: action, ClientNames: rule.ClientNames, LabNames: rule.LabNames, GatewayNames: rule.GatewayNames, Include: include})
	}
	return rules
}

// ReachableDestinations is the set of subnets a client may reach through the VPN: the destinations of every allow rule BuildAccessRules makes
// for it, over all the VPN gateways of all labs, sorted and without duplicates. It is what the WireGuard config of the client routes into the
// tunnel (AllowedIPs). A gateway counts once it has its /24, ready or not, so the config does not change when a lab only becomes ready.
func ReachableDestinations(client ClientAccessSnapshot, labs []laboratoryv1alpha1.Lab, policy *laboratoryv1alpha1.LabGroupAccessPolicy) []string {
	if policy == nil || client.AssignedIP == "" {
		return nil
	}
	snapshots := map[string]LabAccessSnapshot{}
	for i := range labs {
		l := &labs[i]
		if !l.DeletionTimestamp.IsZero() {
			continue
		}
		for _, g := range l.Spec.GatewaysOfKind(laboratoryv1alpha1.GatewayKindVPN) {
			if cidr := l.Status.GatewayCIDR(g.Name); cidr != "" {
				snapshots[l.Name+"/"+g.Name] = LabAccessSnapshot{LabName: l.Name, Gateway: g.Name, VPNCIDR: cidr, Ready: true}
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, rule := range BuildAccessRules([]ClientAccessSnapshot{client}, snapshots, PolicyRules(policy)) {
		if rule.Action == AccessAllow && !seen[rule.DestinationCIDR] {
			seen[rule.DestinationCIDR] = true
			out = append(out, rule.DestinationCIDR)
		}
	}
	sortPrefixes(out)
	return out
}

func sortPrefixes(p []string) { sort.Strings(p) }
