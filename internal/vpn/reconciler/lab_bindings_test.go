//go:build linux

package reconciler

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
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
