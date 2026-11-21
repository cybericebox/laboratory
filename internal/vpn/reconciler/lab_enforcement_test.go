//go:build linux

package reconciler

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/vpn"
)

func TestLabEnforcementConfirmsOnlyAppliedLabsWithTheSwitchOff(t *testing.T) {
	labs := map[string]vpn.LabAccessSnapshot{
		"a": {LabName: "a", Interface: "lab1"},
		"b": {LabName: "b", Interface: "lab2", LabInitiated: true},
		"c": {LabName: "c"}, // no interface yet
	}
	got := labEnforcement(true, labs, map[string]uint64{"lab1": 7, "lab2": 9})
	if !got["a"].blocked || got["a"].dropped != 7 || got["b"].blocked || got["b"].dropped != 0 || got["c"].blocked {
		t.Fatalf("%+v", got)
	}
	if got := labEnforcement(false, labs, nil); got["a"].blocked {
		t.Fatal("nothing is confirmed before the rules are in the kernel")
	}
}

type dropsApplier struct{ counts map[string]uint64 }

func (dropsApplier) ReplaceAccessRules([]vpn.AccessRule) error                  { return nil }
func (dropsApplier) AccessCounters() (map[string]vpn.TrafficCounter, error)     { return nil, nil }
func (d dropsApplier) LabDropCounts(context.Context) (map[string]uint64, error) { return d.counts, nil }

func TestPublishLabEnforcementWritesTheConfirmationAndThrottlesTheCount(t *testing.T) {
	s := schemeForTest(t)
	leg := &lab.LabVPN{ObjectMeta: metav1.ObjectMeta{Name: "labvpn-a.vpn", Namespace: "g"}, Spec: lab.LabVPNSpec{LabName: "a", Device: "vpn", NetworkIndex: 1}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(leg).WithStatusSubresource(leg).Build()
	r := &AccessReconciler{Client: c, applied: true, IPT: dropsApplier{map[string]uint64{"lab1": 5}}}
	labs := map[string]vpn.LabAccessSnapshot{leg.Name: {LabName: "a", Gateway: "vpn", Interface: "lab1"}}
	get := func() lab.LabVPNStatus {
		t.Helper()
		var got lab.LabVPN
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(leg), &got); err != nil {
			t.Fatal(err)
		}
		return got.Status
	}
	legs := func() []lab.LabVPN {
		var l lab.LabVPNList
		if err := c.List(context.Background(), &l); err != nil {
			t.Fatal(err)
		}
		return l.Items
	}
	if err := r.publishLabEnforcement(context.Background(), labs, legs()); err != nil {
		t.Fatal(err)
	}
	if st := get(); !st.LabInitiatedBlocked || st.LabInitiatedDropped != 5 {
		t.Fatalf("%+v", st)
	}
	// the count changed, but not enough time passed: only the confirmation is certain, the count waits
	r.IPT = dropsApplier{map[string]uint64{"lab1": 6}}
	if err := r.publishLabEnforcement(context.Background(), labs, legs()); err != nil {
		t.Fatal(err)
	}
	if st := get(); st.LabInitiatedDropped != 5 {
		t.Fatalf("the count was written again at once: %+v", st)
	}
	// the switch is turned on: the confirmation goes away at once
	labs[leg.Name] = vpn.LabAccessSnapshot{LabName: "a", Gateway: "vpn", Interface: "lab1", LabInitiated: true}
	if err := r.publishLabEnforcement(context.Background(), labs, legs()); err != nil {
		t.Fatal(err)
	}
	if st := get(); st.LabInitiatedBlocked || st.LabInitiatedDropped != 0 {
		t.Fatalf("%+v", st)
	}
	// rules not applied (or a program that never reaches this code): never confirmed
	r.applied = false
	labs[leg.Name] = vpn.LabAccessSnapshot{LabName: "a", Gateway: "vpn", Interface: "lab1"}
	if err := r.publishLabEnforcement(context.Background(), labs, legs()); err != nil {
		t.Fatal(err)
	}
	if st := get(); st.LabInitiatedBlocked {
		t.Fatalf("confirmed before the rules were applied: %+v", st)
	}
}

func schemeForTest(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := lab.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}
