package vpn

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

var alice = []ClientAccessSnapshot{{Name: "alice", AssignedIP: "10.8.0.2/32"}}

// two VPN gateways (connectors) of one lab, each with its own /24
func twoConnectors() map[string]LabAccessSnapshot {
	return map[string]LabAccessSnapshot{
		"web/office": {LabName: "web", Gateway: "office", VPNCIDR: "10.8.1.0/24", Ready: true, Interface: "lab1"},
		"web/dmz":    {LabName: "web", Gateway: "dmz", VPNCIDR: "10.8.2.0/24", Ready: true, Interface: "lab2"},
	}
}

func allowed(rules []AccessRule) []string {
	var out []string
	for _, r := range rules {
		if r.Action == AccessAllow {
			out = append(out, r.GatewayName+" "+r.DestinationCIDR)
		}
	}
	return out
}

func TestClientAdmittedToOneConnectorOfTwoInOneLab(t *testing.T) {
	rules := BuildAccessRules(alice, twoConnectors(),
		[]AccessPolicyRule{{Action: AccessAllow, ClientNames: []string{"alice"}, LabNames: []string{"web"}, GatewayNames: []string{"dmz"}}})
	if got := allowed(rules); !reflect.DeepEqual(got, []string{"dmz 10.8.2.0/24"}) {
		t.Fatalf("allowed = %v, want only the dmz connector", got)
	}
	if len(rules) != 2 {
		t.Fatalf("rules = %#v, want an explicit deny for the office connector", rules)
	}
	plan, err := CompileForwardPlan(alice, twoConnectors(), []AccessPolicyRule{{Action: AccessAllow, LabNames: []string{"web"}, GatewayNames: []string{"dmz"}}})
	if err != nil || len(plan.Allows) != 1 || plan.Allows[0].LabCIDR != "10.8.2.0/24" || plan.Allows[0].LabInterface != "lab2" {
		t.Fatalf("forward plan = %+v, %v", plan, err)
	}
}

func TestAllowWithoutGatewaysGrantsNoVPNReach(t *testing.T) {
	rules := BuildAccessRules(alice, twoConnectors(), []AccessPolicyRule{{Action: AccessAllow, LabNames: []string{"web"}}})
	if got := allowed(rules); len(got) != 0 {
		t.Fatalf("allowed = %v, a rule without gateways must not open any connector", got)
	}
}

func TestIncludeNarrowsToOneDevice(t *testing.T) {
	rules := BuildAccessRules(alice, twoConnectors(), []AccessPolicyRule{{Action: AccessAllow, LabNames: []string{"web"}, GatewayNames: []string{"office"},
		Include: []Include{{Host: 10, Prefix: 32}}}})
	if got := allowed(rules); !reflect.DeepEqual(got, []string{"office 10.8.1.10/32"}) {
		t.Fatalf("allowed = %v, want only the /32 of one device", got)
	}
}

func TestSeveralIncludesAreAUnion(t *testing.T) {
	rules := BuildAccessRules(alice, twoConnectors(), []AccessPolicyRule{
		{Action: AccessAllow, LabNames: []string{"web"}, GatewayNames: []string{"office"}, Include: []Include{{Host: 10, Prefix: 32}, {Host: 64, Prefix: 26}}},
		{Action: AccessAllow, LabNames: []string{"web"}, GatewayNames: []string{"office"}, Include: []Include{{Host: 70, Prefix: 32}, {Host: 200, Prefix: 29}}},
	})
	want := []string{"office 10.8.1.10/32", "office 10.8.1.200/29", "office 10.8.1.64/26"}
	if got := allowed(rules); !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed = %v, want %v (a subnet inside another is absorbed)", got, want)
	}
}

func TestAnAllowWithoutIncludeOpensTheWholeSegment(t *testing.T) {
	rules := BuildAccessRules(alice, twoConnectors(), []AccessPolicyRule{
		{Action: AccessAllow, LabNames: []string{"web"}, GatewayNames: []string{"office"}, Include: []Include{{Host: 10, Prefix: 32}}},
		{Action: AccessAllow, LabNames: []string{"web"}, GatewayNames: []string{"office"}},
	})
	if got := allowed(rules); !reflect.DeepEqual(got, []string{"office 10.8.1.0/24"}) {
		t.Fatalf("allowed = %v", got)
	}
}

func TestDenyWinsOverIncludeOfOneConnector(t *testing.T) {
	rules := BuildAccessRules(alice, twoConnectors(), []AccessPolicyRule{
		{Action: AccessAllow, LabNames: []string{"web"}, GatewayNames: []string{"office", "dmz"}},
		{Action: AccessDeny, LabNames: []string{"web"}, GatewayNames: []string{"office"}},
	})
	if got := allowed(rules); !reflect.DeepEqual(got, []string{"dmz 10.8.2.0/24"}) {
		t.Fatalf("allowed = %v", got)
	}
}

func TestIncludeOutsideTheSegmentNeverWidens(t *testing.T) {
	for _, in := range []Include{{Host: 256, Prefix: 32}, {Host: -1, Prefix: 32}, {Host: 10, Prefix: 16}, {Host: 10, Prefix: 33}, {Host: 1, Prefix: 25}, {Host: 130, Prefix: 25 - 1}} {
		rules := BuildAccessRules(alice, twoConnectors(), []AccessPolicyRule{{Action: AccessAllow, LabNames: []string{"web"}, GatewayNames: []string{"office"}, Include: []Include{in}}})
		if got := allowed(rules); len(got) != 0 {
			t.Fatalf("include %+v opened %v, an invalid entry must open nothing", in, got)
		}
	}
	// A valid entry beside an invalid one keeps only the valid one.
	rules := BuildAccessRules(alice, twoConnectors(), []AccessPolicyRule{{Action: AccessAllow, LabNames: []string{"web"}, GatewayNames: []string{"office"}, Include: []Include{{Host: 5, Prefix: 32}, {Host: 10, Prefix: 16}}}})
	if got := allowed(rules); !reflect.DeepEqual(got, []string{"office 10.8.1.5/32"}) {
		t.Fatalf("allowed = %v", got)
	}
}

// One VPN gateway behind a switch: the VPN pod only knows the /24 of the gateway, the devices behind the switch are on the same segment.
func TestSingleGatewayIsOneFlatSegment(t *testing.T) {
	labs := map[string]LabAccessSnapshot{"web/vpn": {LabName: "web", Gateway: "vpn", VPNCIDR: "10.8.1.0/24", Ready: true, Interface: "lab1"}}
	plan, err := CompileForwardPlan(alice, labs, allowLabs("web"))
	if err != nil || len(plan.Allows) != 1 || plan.Allows[0].LabCIDR != "10.8.1.0/24" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
}

func TestStatisticsRowPerClientAndLabListsReachableConnectors(t *testing.T) {
	rules := BuildAccessRules(alice, twoConnectors(),
		[]AccessPolicyRule{{Action: AccessAllow, LabNames: []string{"web"}, GatewayNames: []string{"dmz"}, Include: []Include{{Host: 1, Prefix: 32}, {Host: 2, Prefix: 32}}}})
	stats := ProjectAccessStatistics(rules, map[string]TrafficCounter{rules[0].Identifier(): {Packets: 3, Bytes: 90}}, nil)
	// One row per client, lab and action: the allowed connector and the denied one are two rows, each with its own connectors.
	if len(stats) != 2 || stats[0].Action != AccessAllow || stats[0].Bytes != 90 || !reflect.DeepEqual(stats[0].GatewayNames, []string{"dmz"}) ||
		stats[1].Action != AccessDeny || !reflect.DeepEqual(stats[1].GatewayNames, []string{"office"}) {
		t.Fatalf("stats = %#v", stats)
	}
	denied := BuildAccessRules(alice, twoConnectors(), nil)
	stats = ProjectAccessStatistics(denied, nil, nil)
	if len(stats) != 1 || stats[0].Action != AccessDeny || !reflect.DeepEqual(stats[0].GatewayNames, []string{"dmz", "office"}) {
		t.Fatalf("denied stats = %#v", stats)
	}
}

func labWithGateways(name string, cidrs map[string]string) laboratoryv1alpha1.Lab {
	l := laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for g, cidr := range cidrs {
		l.Spec.Gateways = append(l.Spec.Gateways, laboratoryv1alpha1.GatewayTemplate{Name: g, Kind: laboratoryv1alpha1.GatewayKindVPN})
		l.Status.Gateways = append(l.Status.Gateways, laboratoryv1alpha1.GatewayStatus{Name: g, CIDR: cidr})
	}
	return l
}

func TestReachableDestinationsIsTheExactAllowedSet(t *testing.T) {
	labs := []laboratoryv1alpha1.Lab{
		labWithGateways("web", map[string]string{"office": "10.8.1.0/24", "dmz": "10.8.2.0/24"}),
		labWithGateways("ctf", map[string]string{"vpn": "10.8.3.0/24"}),
	}
	policy := &laboratoryv1alpha1.LabGroupAccessPolicy{Spec: laboratoryv1alpha1.LabGroupAccessPolicySpec{Rules: []laboratoryv1alpha1.LabGroupAccessRule{
		{Action: laboratoryv1alpha1.LabGroupAccessAllow, ClientNames: []string{"alice"}, LabNames: []string{"web"}, GatewayNames: []string{"office"}, Include: []laboratoryv1alpha1.AccessInclude{{Host: 10, Prefix: 32}}},
		{Action: laboratoryv1alpha1.LabGroupAccessAllow, ClientNames: []string{"alice"}, LabNames: []string{"ctf"}, GatewayNames: []string{"vpn"}},
	}}}
	got := ReachableDestinations(alice[0], labs, policy)
	if want := []string{"10.8.1.10/32", "10.8.3.0/24"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AllowedIPs = %v, want %v", got, want)
	}
	if got := ReachableDestinations(ClientAccessSnapshot{Name: "bob", AssignedIP: "10.8.0.3/32"}, labs, policy); len(got) != 0 {
		t.Fatalf("bob = %v, a client without a rule reaches nothing", got)
	}
	if got := ReachableDestinations(alice[0], labs, nil); got != nil {
		t.Fatalf("no policy = %v", got)
	}
}
