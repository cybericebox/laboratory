//go:build linux

package reconciler

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/vpn"
)

func TestLabAccessBindingUsesAllocatedInterface(t *testing.T) {
	labs := []lab.Lab{{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Status: lab.LabStatus{Phase: lab.PhaseReady, VPN: lab.LabNetworkStatus{Ready: true, CIDR: "10.8.7.0/24"}}}}
	legs := []lab.LabVPN{{Spec: lab.LabVPNSpec{LabName: "a", NetworkIndex: 7}}}
	snapshots, err := buildLabAccessSnapshots(labs, legs)
	if err != nil || snapshots["a"].Interface != "lab7" || !snapshots["a"].Ready {
		t.Fatalf("allocated binding = %+v, %v", snapshots, err)
	}
	legs = append(legs, lab.LabVPN{Spec: lab.LabVPNSpec{LabName: "a", NetworkIndex: 8}})
	if _, err := buildLabAccessSnapshots(labs, legs); err == nil {
		t.Fatal("ambiguous physical lab binding was accepted")
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
	mk := func(name, cidr string, stopped bool) lab.Lab {
		l := lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "g"}, Status: lab.LabStatus{Phase: lab.PhaseReady, VPN: lab.LabNetworkStatus{Ready: true, CIDR: cidr}}}
		l.Spec.VPN.Enabled = true
		if stopped {
			l.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 2, SnapshotMode: "Skip"}
		}
		return l
	}
	labs := []lab.Lab{mk("stopped", "10.8.1.0/24", true), mk("running", "10.8.2.0/24", false)}
	snaps, err := buildLabAccessSnapshots(labs, nil)
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
