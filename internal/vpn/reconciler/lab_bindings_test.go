//go:build linux

package reconciler

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/vpn"
)

func TestLabAccessBindingUsesAllocatedInterface(t *testing.T) {
	l, leg := vpnLab("g", "a", "10.8.7.0/24", 7)
	snapshots, err := buildLabAccessSnapshots([]lab.Lab{*l}, []lab.LabVPN{*leg})
	snap, ok := snapshots[leg.Name]
	if err != nil || !ok || snap.Interface != "lab7" || !snap.Ready || snap.LabName != "a" || snap.Gateway != "vpn" || snap.VPNCIDR != "10.8.7.0/24" {
		t.Fatalf("allocated binding = %+v, %v", snapshots, err)
	}
	// Two legs of one interface number are ambiguous.
	other := leg.DeepCopy()
	other.Name = names.LabVPNObjectName("a", "second")
	other.Spec.Device = "second"
	l.Spec.Gateways = append(l.Spec.Gateways, lab.GatewayTemplate{Name: "second", Kind: lab.GatewayKindVPN})
	if _, err := buildLabAccessSnapshots([]lab.Lab{*l}, []lab.LabVPN{*leg, *other}); err == nil {
		t.Fatal("ambiguous physical lab binding was accepted")
	}
}

// A lab with several VPN gateways has a snapshot for each, with its own network, interface and switch; a gateway whose leg is not there yet,
// an Internet gateway and a leg of a gateway that is gone have none.
func TestEveryVPNGatewayOfALabHasASnapshot(t *testing.T) {
	l := lab.Lab{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec: lab.LabSpec{Gateways: []lab.GatewayTemplate{
			{Name: "attack", Kind: lab.GatewayKindVPN, ReverseConnections: true},
			{Name: "user", Kind: lab.GatewayKindVPN},
			{Name: "waiting", Kind: lab.GatewayKindVPN},
			{Name: "out", Kind: lab.GatewayKindInternet},
		}},
		Status: lab.LabStatus{Phase: lab.PhaseReady, Gateways: []lab.GatewayStatus{
			{Name: "attack", CIDR: "10.8.3.0/24", Ready: true}, {Name: "user", CIDR: "10.8.4.0/24"}, {Name: "out", CIDR: "10.8.5.0/24", Ready: true},
		}},
	}
	leg := func(device string, n uint) lab.LabVPN {
		return lab.LabVPN{ObjectMeta: metav1.ObjectMeta{Name: names.LabVPNObjectName("a", device)}, Spec: lab.LabVPNSpec{LabName: "a", Device: device, NetworkIndex: n}}
	}
	snapshots, err := buildLabAccessSnapshots([]lab.Lab{l}, []lab.LabVPN{leg("attack", 3), leg("user", 4), leg("out", 5), leg("gone", 6)})
	if err != nil || len(snapshots) != 2 {
		t.Fatalf("%+v %v", snapshots, err)
	}
	attack, user := snapshots[names.LabVPNObjectName("a", "attack")], snapshots[names.LabVPNObjectName("a", "user")]
	if attack.Interface != "lab3" || !attack.Ready || !attack.LabInitiated || attack.VPNCIDR != "10.8.3.0/24" {
		t.Fatalf("attack: %+v", attack)
	}
	if user.Interface != "lab4" || user.Ready || user.LabInitiated {
		t.Fatalf("user: %+v", user)
	}
	rules := vpn.BuildAccessRules([]vpn.ClientAccessSnapshot{{Name: "c", AssignedIP: "10.8.0.2/32"}}, snapshots, []vpn.AccessPolicyRule{{Action: vpn.AccessAllow}})
	if len(rules) != 1 || rules[0].GatewayName != "attack" || rules[0].LabName != "a" || rules[0].LabInterface != "lab3" {
		t.Fatalf("only the ready gateway is opened: %+v", rules)
	}
	user.Ready = true
	snapshots[names.LabVPNObjectName("a", "user")] = user
	rules = vpn.BuildAccessRules([]vpn.ClientAccessSnapshot{{Name: "c", AssignedIP: "10.8.0.2/32"}}, snapshots, []vpn.AccessPolicyRule{{Action: vpn.AccessAllow}})
	if len(rules) != 2 || rules[0].GatewayName != "attack" || rules[1].GatewayName != "user" || rules[0].Identifier() != rules[1].Identifier() {
		t.Fatalf("a client reaches every gateway of the lab, under one lab identifier: %+v", rules)
	}
}

func TestLabVPNAccessEventsIgnoreHealthButKeepPhysicalChanges(t *testing.T) {
	before := &lab.LabVPN{Spec: lab.LabVPNSpec{LabName: "a", NetworkIndex: 7}}
	after := before.DeepCopy()
	after.Status.DHCPReady = true
	if labVPNAccessChanges().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("DHCP status changed the access binding")
	}
	after.Spec.NetworkIndex = 8
	if !labVPNAccessChanges().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("new physical interface was ignored")
	}
}

// A lab stopped while its group had no VPN pod (no fence was taken): when the group is resumed, the new VPN pod builds the access from the
// state of the day. The stopped lab is not ready, so no rule allows any client to it, whatever the group's policy says; a lab that was
// running stays reachable. This is what makes it safe to stop a lab in a suspended group without a fence.
func TestResumedVPNPodDoesNotOpenAStoppedLab(t *testing.T) {
	var labs []lab.Lab
	var legs []lab.LabVPN
	for i, c := range []struct {
		name, cidr string
		stopped    bool
	}{{"stopped", "10.8.1.0/24", true}, {"running", "10.8.2.0/24", false}} {
		l, leg := vpnLab("g", c.name, c.cidr, uint(i+1))
		if c.stopped {
			l.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 2, SnapshotMode: "Skip"}
		}
		labs, legs = append(labs, *l), append(legs, *leg)
	}
	snaps, err := buildLabAccessSnapshots(labs, legs)
	if err != nil {
		t.Fatal(err)
	}
	rules := vpn.BuildAccessRules([]vpn.ClientAccessSnapshot{{Name: "c", AssignedIP: "10.8.0.2/32"}}, snaps, []vpn.AccessPolicyRule{{Action: vpn.AccessAllow}})
	allowed := map[string]bool{}
	for _, r := range rules {
		if r.Action == vpn.AccessAllow {
			allowed[r.LabName] = true
		}
	}
	if allowed["stopped"] {
		t.Fatal("a stopped lab was opened to a client")
	}
	if !allowed["running"] {
		t.Fatal("a running lab must stay reachable")
	}
}

// The switch of the lab reaches the access snapshot, and a change of it is an access input (the rules are rebuilt).
func TestLabSwitchReachesTheSnapshotAndTriggersARebuild(t *testing.T) {
	mk := func(on bool) (*lab.Lab, lab.LabVPN) {
		l, leg := vpnLab("g", "a", "10.8.7.0/24", 7)
		l.Spec.Gateways[0].ReverseConnections = on
		return l, *leg
	}
	off, offLeg := mk(false)
	on, onLeg := mk(true)
	snapshots, err := buildLabAccessSnapshots([]lab.Lab{*off}, []lab.LabVPN{offLeg})
	if err != nil || snapshots[offLeg.Name].LabInitiated {
		t.Fatalf("the default must block: %+v %v", snapshots, err)
	}
	snapshots, err = buildLabAccessSnapshots([]lab.Lab{*on}, []lab.LabVPN{onLeg})
	if err != nil || !snapshots[onLeg.Name].LabInitiated {
		t.Fatalf("the switch was lost: %+v %v", snapshots, err)
	}
	if !labAccessInputsChanged(off, on) || !labAccessInputsChanged(on, off) {
		t.Fatal("a change of the switch did not trigger a rebuild")
	}
	if labAccessInputsChanged(on, on.DeepCopy()) {
		t.Fatal("an unchanged lab triggered a rebuild")
	}
}
