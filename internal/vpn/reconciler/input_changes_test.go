//go:build linux

package reconciler

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestClientConfigurationEventsExcludeTelemetry(t *testing.T) {
	before := &lab.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "group"},
		Spec: lab.LabGroupClientSpec{PublicKey: "key1"}, Status: lab.LabGroupClientStatus{AssignedIP: "10.8.0.2/32"}}
	cases := []struct {
		name         string
		change       func(*lab.LabGroupClient)
		peer, access bool
	}{
		{"rx bytes", func(c *lab.LabGroupClient) { c.Status.Statistics.RxBytes++ }, false, false},
		{"tx bytes", func(c *lab.LabGroupClient) { c.Status.Statistics.TxBytes++ }, false, false},
		{"handshake", func(c *lab.LabGroupClient) { c.Status.Statistics.LastHandshake = metav1.Now() }, false, false},
		{"metadata", func(c *lab.LabGroupClient) { c.ResourceVersion = "2" }, false, false},
		{"config text", func(c *lab.LabGroupClient) { c.Status.Config = "updated download" }, false, false},
		{"new address", func(c *lab.LabGroupClient) { c.Status.AssignedIP = "10.8.0.3/32" }, true, true},
		{"address removed", func(c *lab.LabGroupClient) { c.Status.AssignedIP = "" }, true, true},
		{"key rotated", func(c *lab.LabGroupClient) { c.Spec.PublicKey = "key2" }, true, false},
		{"deleting", func(c *lab.LabGroupClient) { now := metav1.Now(); c.DeletionTimestamp = &now }, true, true},
		{"identical", func(*lab.LabGroupClient) {}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			after := before.DeepCopy()
			tc.change(after)
			e := event.UpdateEvent{ObjectOld: before, ObjectNew: after}
			if got := clientPeerChanges().Update(e); got != tc.peer {
				t.Fatalf("peer update = %v, want %v", got, tc.peer)
			}
			if got := clientAccessChanges().Update(e); got != tc.access {
				t.Fatalf("access update = %v, want %v", got, tc.access)
			}
		})
	}
	for _, p := range []struct {
		name  string
		allow func() bool
	}{
		{"peer creation", func() bool { return clientPeerChanges().Create(event.CreateEvent{Object: before}) }},
		{"access creation", func() bool { return clientAccessChanges().Create(event.CreateEvent{Object: before}) }},
		{"peer deletion", func() bool { return clientPeerChanges().Delete(event.DeleteEvent{Object: before}) }},
		{"access deletion", func() bool { return clientAccessChanges().Delete(event.DeleteEvent{Object: before}) }},
	} {
		if !p.allow() {
			t.Fatal(p.name + " was ignored")
		}
	}
}

func TestLabAccessEventsRetainReadinessAndAllocationChanges(t *testing.T) {
	before := &lab.Lab{Status: lab.LabStatus{Phase: lab.PhaseReady, VPN: lab.LabNetworkStatus{Ready: true, CIDR: "10.8.1.0/24"}}}
	cases := []struct {
		name   string
		change func(*lab.Lab)
		want   bool
	}{
		{"metadata", func(l *lab.Lab) { l.ResourceVersion = "2" }, false},
		{"interface not ready", func(l *lab.Lab) { l.Status.VPN.Ready = false }, true},
		{"phase not ready", func(l *lab.Lab) { l.Status.Phase = lab.PhasePending }, true},
		{"new subnet", func(l *lab.Lab) { l.Status.VPN.CIDR = "10.8.2.0/24" }, true},
		{"deleting", func(l *lab.Lab) { now := metav1.Now(); l.DeletionTimestamp = &now }, true},
		{"identical", func(*lab.Lab) {}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			after := before.DeepCopy()
			tc.change(after)
			if got := labAccessChanges().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}); got != tc.want {
				t.Fatalf("access update = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStoppedIntentClosesAccessWithoutStatusOrPolicyChange(t *testing.T) {
	old := &lab.Lab{Status: lab.LabStatus{Phase: lab.PhaseReady, VPN: lab.LabNetworkStatus{Ready: true, CIDR: "10.8.1.0/24"}}}
	next := old.DeepCopy()
	next.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Required"}
	if labAccessReady(next) {
		t.Fatal("stopped intent remained access ready")
	}
	if !labAccessInputsChanged(old, next) {
		t.Fatal("stop did not wake physical access owner")
	}
	next.Spec.Lifecycle.Revision = 2
	prior := next.DeepCopy()
	prior.Spec.Lifecycle.Revision = 1
	if !labAccessInputsChanged(prior, next) {
		t.Fatal("new operation fence not refreshed")
	}
}
