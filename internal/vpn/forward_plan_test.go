package vpn

import (
	"fmt"
	"reflect"
	"testing"
)

func TestForwardPlanKeepsOnlyAllowedBindings(t *testing.T) {
	clients := []ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.2/32"}}
	labs := map[string]LabAccessSnapshot{
		"a": {LabName: "a", Gateway: "vpn", VPNCIDR: "10.8.1.0/24", Ready: true, Interface: "lab1"},
		"b": {LabName: "b", Gateway: "vpn", VPNCIDR: "10.8.2.0/24", Ready: true, Interface: "lab2"},
	}
	plan, err := CompileForwardPlan(clients, labs, allowLabs("a"))
	if err != nil || len(plan.Allows) != 1 || len(plan.Decisions) != 2 {
		t.Fatalf("unexpected plan: %+v, %v", plan, err)
	}
	if r := plan.Allows[0]; r.ClientName != "p1" || r.LabName != "a" || r.ClientCIDR != "10.8.0.2/32" || r.LabCIDR != "10.8.1.0/24" || r.LabInterface != "lab1" || r.BindingID == "" {
		t.Fatalf("wrong allowed binding: %+v", r)
	}
	blocked, err := CompileForwardPlan(clients, labs, append(allowLabs("a", "b"), AccessPolicyRule{Action: AccessDeny, LabNames: []string{"a"}}))
	if err != nil || len(blocked.Allows) != 1 || blocked.Allows[0].LabName != "b" {
		t.Fatalf("explicit deny did not win: %+v, %v", blocked, err)
	}
}

func TestForwardPlanTwentyByTwentyHasTwentyKernelBindings(t *testing.T) {
	var clients []ClientAccessSnapshot
	labs := map[string]LabAccessSnapshot{}
	var policy []AccessPolicyRule
	for i := 0; i < 20; i++ {
		p, l := fmt.Sprintf("p%02d", i), fmt.Sprintf("l%02d", i)
		clients = append(clients, ClientAccessSnapshot{Name: p, AssignedIP: fmt.Sprintf("10.8.0.%d/32", i+2)})
		labs[l] = LabAccessSnapshot{LabName: l, Gateway: "vpn", VPNCIDR: fmt.Sprintf("10.8.%d.0/24", i+1), Ready: true, Interface: fmt.Sprintf("lab%d", i+1)}
		policy = append(policy, AccessPolicyRule{Action: AccessAllow, ClientNames: []string{p}, LabNames: []string{l}, GatewayNames: []string{"vpn"}})
	}
	plan, err := CompileForwardPlan(clients, labs, policy)
	if err != nil || len(plan.Allows) != 20 || len(plan.Decisions) != 400 {
		t.Fatalf("20x20 plan = %d allows, %d decisions, %v", len(plan.Allows), len(plan.Decisions), err)
	}
	if plan.Allows[0].ClientName != "p00" || plan.Allows[19].ClientName != "p19" {
		t.Fatal("bindings are not deterministically ordered")
	}
}

func TestForwardPlanBindingIdentityFollowsAllocation(t *testing.T) {
	labs := map[string]LabAccessSnapshot{"a": {LabName: "a", Gateway: "vpn", VPNCIDR: "10.8.1.0/24", Ready: true, Interface: "lab1"}}
	policy := allowLabs("a", "b", "l1")
	a, err := CompileForwardPlan([]ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.2"}}, labs, policy)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CompileForwardPlan([]ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.2/32"}}, labs, policy)
	if err != nil || !reflect.DeepEqual(a.Allows, b.Allows) {
		t.Fatalf("equivalent address changed binding: %+v, %v", b, err)
	}
	c, err := CompileForwardPlan([]ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.3/32"}}, labs, policy)
	if err != nil || c.Allows[0].BindingID == a.Allows[0].BindingID {
		t.Fatal("reissued address kept old accounting identity")
	}
}

func TestForwardPlanRejectsUnsafeBindings(t *testing.T) {
	for _, tc := range []struct{ address, subnet, iface string }{
		{"10.8.0.2/24", "10.8.1.0/24", "lab1"},
		{"invalid", "10.8.1.0/24", "lab1"},
		{"10.8.0.2/32", "invalid", "lab1"},
		{"10.8.0.2/32", "10.8.1.0/24", ""},
		{"10.8.0.2/32", "10.8.1.0/24", "lab+"},
		{"10.8.0.2/32", "10.8.1.0/24", "lab1\n-j ACCEPT"},
	} {
		_, err := CompileForwardPlan([]ClientAccessSnapshot{{Name: "p1", AssignedIP: tc.address}},
			map[string]LabAccessSnapshot{"a": {LabName: "a", Gateway: "vpn", VPNCIDR: tc.subnet, Ready: true, Interface: tc.iface}}, allowLabs("a", "b", "l1"))
		if err == nil {
			t.Fatalf("unsafe binding accepted: %+v", tc)
		}
	}
	plan, err := CompileForwardPlan([]ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.2/32"}},
		map[string]LabAccessSnapshot{"a": {LabName: "a", Gateway: "vpn", VPNCIDR: "10.8.1.0/24", Ready: false}}, allowLabs("a", "b", "l1"))
	if err != nil || len(plan.Allows) != 0 {
		t.Fatalf("not-ready lab became reachable: %+v, %v", plan, err)
	}
}

func BenchmarkForwardPlan(b *testing.B) {
	for _, users := range []int{10, 20} {
		for _, labs := range []int{10, 20} {
			b.Run(fmt.Sprintf("%dU_%dL", users, labs), func(b *testing.B) {
				clients := []ClientAccessSnapshot{}
				nets := map[string]LabAccessSnapshot{}
				rules := []AccessPolicyRule{}
				for i := 0; i < users; i++ {
					name := fmt.Sprintf("p%d", i)
					clients = append(clients, ClientAccessSnapshot{Name: name, AssignedIP: fmt.Sprintf("10.8.0.%d/32", i+2)})
					rules = append(rules, AccessPolicyRule{Action: AccessAllow, ClientNames: []string{name}, LabNames: []string{fmt.Sprintf("l%d", i%labs)}, GatewayNames: []string{"vpn"}})
				}
				for i := 0; i < labs; i++ {
					nets[fmt.Sprintf("l%d", i)] = LabAccessSnapshot{LabName: fmt.Sprintf("l%d", i), Gateway: "vpn", Ready: true, VPNCIDR: fmt.Sprintf("10.8.%d.0/24", i+1), Interface: fmt.Sprintf("lab%d", i+1)}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := CompileForwardPlan(clients, nets, rules); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
