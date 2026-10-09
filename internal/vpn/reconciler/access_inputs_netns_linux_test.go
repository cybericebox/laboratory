//go:build linux

package reconciler

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nstest"
	"github.com/cybericebox/laboratory/internal/vpn"
)

// A second reconcile with unchanged permissions must not flush real counters.
// This catches the existing unconditional firewall replacement without mocking it.
func TestNetnsAccessReconcilePreservesCountersForUnchangedPermissions(t *testing.T) {
	nstest.Require(t)
	scheme := runtime.NewScheme()
	if err := lab.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const ns = "group"
	peer := &lab.LabGroupClient{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: ns},
		Status:     lab.LabGroupClientStatus{AssignedIP: "10.8.0.2/32"},
	}
	deviceLab := &lab.Lab{
		ObjectMeta: metav1.ObjectMeta{Name: "l1", Namespace: ns},
		Status: lab.LabStatus{Phase: lab.PhaseReady,
			VPN: lab.LabNetworkStatus{Ready: true, CIDR: "10.8.1.0/24"}},
	}
	policy := &lab.LabGroupAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: ns, Generation: 1},
		Spec: lab.LabGroupAccessPolicySpec{Rules: []lab.LabGroupAccessRule{{
			Action: lab.LabGroupAccessAllow, ClientNames: []string{"p1"}, LabNames: []string{"l1"},
		}}},
	}
	leg := &lab.LabVPN{ObjectMeta: metav1.ObjectMeta{Name: "labvpn-l1", Namespace: ns}, Spec: lab.LabVPNSpec{LabName: "l1", NetworkIndex: 1}}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(peer, deviceLab, policy).
		WithObjects(peer, deviceLab, policy, leg).Build()
	ipt, err := vpn.NewIPTablesManager("wg0")
	if err != nil {
		t.Fatal(err)
	}
	if err := ipt.SetupForwardPolicy(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ipt.Cleanup)
	r := &AccessReconciler{Client: c, IPT: ipt}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: names.LabGroupAccessPolicyName}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rule := vpn.AccessRule{ClientName: "p1", LabName: "l1", SourceCIDR: "10.8.0.2/32", DestinationCIDR: "10.8.1.0/24", Action: vpn.AccessAllow, LabInterface: "lab1"}
	saved := nstest.Run(t, "", "iptables-save", "-c", "-t", "filter")
	var meterRule string
	for _, line := range strings.Split(saved, "\n") {
		if strings.Contains(line, "cibacct:") && strings.Contains(line, ":F:T") {
			meterRule = line
			break
		}
	}
	if meterRule == "" {
		t.Fatal("missing real forward meter")
	}
	fields := strings.Fields(meterRule)
	chain := fields[2]
	comment := strings.Trim(fields[len(fields)-3], "\"")
	nstest.Run(t, "", "iptables", "-t", "filter", "-R", chain, "2", "-m", "comment", "--comment", comment, "-j", "ACCEPT", "-c", "7", "700")
	before, err := ipt.AccessCounters()
	if err != nil || before[rule.Identifier()].Packets != 7 {
		t.Fatalf("control: real counter was not seeded: %+v, %v", before, err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	after, err := ipt.AccessCounters()
	if err != nil {
		t.Fatal(err)
	}
	if got := after[rule.Identifier()]; got.Packets != 7 || got.Bytes != 700 {
		t.Fatalf("unchanged permissions reset the real counter: %+v, want 7 packets/700 bytes", got)
	}
}
