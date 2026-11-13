package vpn

import (
	"strings"
	"testing"
)

func pairFor(t *testing.T, labInitiated bool) ForwardRule {
	t.Helper()
	plan, err := CompileForwardPlan(
		[]ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.2/32"}},
		map[string]LabAccessSnapshot{"a": {LabName: "a", VPNCIDR: "10.8.1.0/24", Ready: true, Interface: "lab1", LabInitiated: labInitiated}},
		[]AccessPolicyRule{{Action: AccessAllow}})
	if err != nil || len(plan.Allows) != 1 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	return plan.Allows[0]
}

// By default the lab-to-client rule forwards only replies of connections the client started.
func TestLabToClientRuleForwardsOnlyRepliesByDefault(t *testing.T) {
	r := pairFor(t, false)
	if r.LabInitiated {
		t.Fatal("the default must be blocked")
	}
	fwd, rev := accessGateRules("GATE", "wg0", r, "F", "R")
	if want := "-A GATE -i wg0 -o lab1 -s 10.8.0.2/32 -d 10.8.1.0/24 -j F"; fwd != want {
		t.Fatalf("client to lab changed:\n%s\nwant\n%s", fwd, want)
	}
	if want := "-A GATE -i lab1 -o wg0 -d 10.8.0.2/32 -m conntrack --ctstate ESTABLISHED,RELATED --ctdir REPLY -j R"; rev != want {
		t.Fatalf("lab to client:\n%s\nwant\n%s", rev, want)
	}
}

// With the switch on, nothing is added: the rule is the one of before this feature.
func TestLabToClientRuleOpenWhenTheLabMayStartConnections(t *testing.T) {
	_, rev := accessGateRules("GATE", "wg0", pairFor(t, true), "F", "R")
	if want := "-A GATE -i lab1 -o wg0 -d 10.8.0.2/32 -j R"; rev != want {
		t.Fatalf("lab to client:\n%s\nwant\n%s", rev, want)
	}
}

// The direction of the packet matters as much as the state: an established connection the lab started goes the original way.
func TestLabToClientRuleRequiresTheReplyDirection(t *testing.T) {
	_, rev := accessGateRules("GATE", "wg0", pairFor(t, false), "F", "R")
	if !strings.Contains(rev, "--ctdir REPLY") || !strings.Contains(rev, "ESTABLISHED,RELATED") || strings.Contains(rev, "NEW") {
		t.Fatalf("%s", rev)
	}
}

// Switching the option changes how a pair is forwarded, not which pair it is: the counters survive.
func TestSwitchDoesNotChangeThePairIdentity(t *testing.T) {
	off, on := pairFor(t, false), pairFor(t, true)
	if off.BindingID != on.BindingID {
		t.Fatalf("binding id changed: %s %s", off.BindingID, on.BindingID)
	}
	rules := BuildAccessRules([]ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.2/32"}},
		map[string]LabAccessSnapshot{"a": {LabName: "a", VPNCIDR: "10.8.1.0/24", Ready: true, Interface: "lab1"}}, []AccessPolicyRule{{Action: AccessAllow}})
	flipped := BuildAccessRules([]ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.2/32"}},
		map[string]LabAccessSnapshot{"a": {LabName: "a", VPNCIDR: "10.8.1.0/24", Ready: true, Interface: "lab1", LabInitiated: true}}, []AccessPolicyRule{{Action: AccessAllow}})
	if rules[0].Identifier() != flipped[0].Identifier() || rules[0] == flipped[0] {
		t.Fatal("the flip must change the rule (so it is applied) but not its identifier")
	}
}

// Two labs of one client are decided separately.
func TestTheSwitchIsPerLab(t *testing.T) {
	plan, err := CompileForwardPlan(
		[]ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.2/32"}, {Name: "p2", AssignedIP: "10.8.0.3/32"}},
		map[string]LabAccessSnapshot{
			"a": {LabName: "a", VPNCIDR: "10.8.1.0/24", Ready: true, Interface: "lab1", LabInitiated: true},
			"b": {LabName: "b", VPNCIDR: "10.8.2.0/24", Ready: true, Interface: "lab2"},
		}, []AccessPolicyRule{{Action: AccessAllow}})
	if err != nil || len(plan.Allows) != 4 {
		t.Fatalf("%+v %v", plan, err)
	}
	for _, r := range plan.Allows {
		if r.LabInitiated != (r.LabName == "a") {
			t.Fatalf("%s/%s: %v", r.ClientName, r.LabName, r.LabInitiated)
		}
	}
}

// The capability the agent advertises is read off the generated rules of both gates.
func TestLabInitiatedBlockEnforcedFollowsTheRules(t *testing.T) {
	if !LabInitiatedBlockEnforced() {
		t.Fatal("both gates restrict a default lab to replies: the capability must be true")
	}
	// The same probes by hand, so the function cannot be a constant that passes whatever the rules say.
	_, open := accessGateRules("G", "wg0", ForwardRule{LabInterface: "lab1", ClientCIDR: "10.8.0.2/32", LabCIDR: "10.8.1.0/24", LabInitiated: true}, "F", "R")
	if strings.Contains(open, "--ctdir") {
		t.Fatal("an open lab has no restriction, so the probe must use the default pair")
	}
}

func TestBlockRulesCountWhatTheDefaultLabsSendButOpenLabsAreNotCounted(t *testing.T) {
	rules := []ForwardRule{
		{LabInterface: "lab2"}, {LabInterface: "lab1"}, {LabInterface: "lab1"}, {LabInterface: "lab3", LabInitiated: true},
	}
	got := accessBlockRules("GATE", "wg0", rules)
	want := []string{
		"-A GATE -i lab1 -o wg0 -m comment --comment cibblk:lab1 -j DROP",
		"-A GATE -i lab2 -o wg0 -m comment --comment cibblk:lab2 -j DROP",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%q", got)
	}
}

func TestParseBlockCountersReadsIptablesSave(t *testing.T) {
	saved := "*filter\n:GATE - [0:0]\n[12:900] -A GATE -i lab1 -o wg0 -m comment --comment cibblk:lab1 -j DROP\n" +
		"[3:180] -A GATE -i lab2 -o wg0 -m comment --comment \"cibblk:lab2\" -j DROP\n[5:5] -A GATE -j DROP\nCOMMIT\n"
	got := parseBlockCounters([]byte(saved))
	if len(got) != 2 || got["lab1"] != 12 || got["lab2"] != 3 {
		t.Fatalf("%v", got)
	}
}
