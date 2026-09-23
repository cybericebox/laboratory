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

	if len(rules) != 4 {
		t.Fatalf("rules = %#v, want every ready client-to-lab pair", rules)
	}
	if rules[0] != (AccessRule{ClientName: "alice", LabName: "forensic", SourceCIDR: "10.8.0.2/32", DestinationCIDR: "10.17.0.0/24", Action: AccessDeny}) {
		t.Fatalf("first rule = %#v", rules[0])
	}
	if rules[1] != (AccessRule{ClientName: "alice", LabName: "web", SourceCIDR: "10.8.0.2/32", DestinationCIDR: "10.16.0.0/24", Action: AccessAllow}) {
		t.Fatalf("second rule = %#v", rules[1])
	}
	if rules[2] != (AccessRule{ClientName: "bob", LabName: "forensic", SourceCIDR: "10.8.0.3/32", DestinationCIDR: "10.17.0.0/24", Action: AccessAllow}) {
		t.Fatalf("third rule = %#v", rules[2])
	}
	if rules[3] != (AccessRule{ClientName: "bob", LabName: "web", SourceCIDR: "10.8.0.3/32", DestinationCIDR: "10.16.0.0/24", Action: AccessDeny}) {
		t.Fatalf("fourth rule = %#v", rules[3])
	}
}

func TestBuildAccessRulesDefaultsToDenyForEmptyPolicy(t *testing.T) {
	rules := BuildAccessRules(
		[]ClientAccessSnapshot{{Name: "alice", AssignedIP: "10.8.0.2/32"}},
		map[string]LabAccessSnapshot{"web": {VPNCIDR: "10.16.0.0/24", Ready: true}},
		nil,
	)
	if len(rules) != 1 || rules[0].Action != AccessDeny {
		t.Fatalf("rules = %#v, want one explicitly denied relation", rules)
	}
}

func TestAccessStatisticsDenyOverridesAllowAndReportsBlockedCounters(t *testing.T) {
	rules := BuildAccessRules(
		[]ClientAccessSnapshot{{Name: "alice", AssignedIP: "10.8.0.2/32"}},
		map[string]LabAccessSnapshot{"web": {VPNCIDR: "10.16.0.0/24", Ready: true}},
		[]AccessPolicyRule{
			{Action: AccessAllow},
			{Action: AccessDeny, ClientNames: []string{"alice"}, LabNames: []string{"web"}},
		},
	)
	if len(rules) != 1 || rules[0].Action != AccessDeny {
		t.Fatalf("rules = %#v, want one denied relation", rules)
	}
	stats := ProjectAccessStatistics(rules, map[string]TrafficCounter{
		rules[0].Identifier(): {Packets: 3, Bytes: 180},
	}, nil)
	if len(stats) != 1 || stats[0].Action != AccessDeny || stats[0].Bytes != 180 {
		t.Fatalf("stats = %#v, want blocked traffic counter", stats)
	}
}
