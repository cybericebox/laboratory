package vpn

import "testing"

func TestBuildAccessRulesUsesOnlyAssignedClientsAndReadyVPNLabs(t *testing.T) {
	rules := BuildAccessRules(
		[]ClientAccessSnapshot{
			{Name: "alice", AssignedIP: "10.8.0.2/32"},
			{Name: "bob", AssignedIP: "10.8.0.3/32"},
			{Name: "no-ip"},
		},
		map[string]LabAccessSnapshot{
			"web":      {VPNCIDR: "10.16.0.0/24", Ready: true},
			"forensic": {VPNCIDR: "10.17.0.0/24", Ready: true},
			"warming":  {VPNCIDR: "10.17.0.0/24", Ready: false},
		},
		[]AccessPolicyRule{
			{Action: AccessAllow, ClientNames: []string{"alice"}, LabNames: []string{"web", "missing"}},
			{Action: AccessAllow, LabNames: []string{"forensic"}},
			{Action: AccessDeny, ClientNames: []string{"alice"}, LabNames: []string{"forensic"}},
		},
	)

	if len(rules) != 2 {
		t.Fatalf("rules = %#v, want exactly two allowed client-to-lab pairs", rules)
	}
	if rules[0] != (AccessRule{SourceCIDR: "10.8.0.2/32", DestinationCIDR: "10.16.0.0/24"}) {
		t.Fatalf("first rule = %#v", rules[0])
	}
	if rules[1] != (AccessRule{SourceCIDR: "10.8.0.3/32", DestinationCIDR: "10.17.0.0/24"}) {
		t.Fatalf("second rule = %#v", rules[1])
	}
}

func TestBuildAccessRulesDefaultsToDenyForEmptyPolicy(t *testing.T) {
	rules := BuildAccessRules(
		[]ClientAccessSnapshot{{Name: "alice", AssignedIP: "10.8.0.2/32"}},
		map[string]LabAccessSnapshot{"web": {VPNCIDR: "10.16.0.0/24", Ready: true}},
		nil,
	)
	if len(rules) != 0 {
		t.Fatalf("rules = %#v, want no rules for an empty allow-list", rules)
	}
}
