package labdhcp

import (
	"context"
	"errors"
	"testing"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDHCPInputsAndOwnedPoolMapping(t *testing.T) {
	before := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g", UID: "a-uid"}}
	after := before.DeepCopy()
	after.Spec.Gateways = []lab.GatewayTemplate{{Name: "out", Kind: lab.GatewayKindInternet, DHCPServer: &lab.DHCPServer{Enabled: true, DNS: "1.1.1.1", Ranges: []lab.DHCPRange{{Start: 2, End: 4}}}}}
	if !InputsChanged(before, after) || InputsChanged(before, before.DeepCopy()) {
		t.Fatal("wrong DHCP inputs")
	}
	p := &allocation.Pool{ObjectMeta: metav1.ObjectMeta{Namespace: "g", Name: names.DHCPPoolName("a", "out"), OwnerReferences: []metav1.OwnerReference{{APIVersion: "laboratory.cybericebox.com/v1alpha1", Kind: "Lab", Name: "a", UID: "a-uid"}}}}
	if owner, gateway := PoolOwner(p); owner != "a" || gateway != "out" {
		t.Fatalf("owned pool not mapped: %q %q", owner, gateway)
	}
	p.Name = names.DHCPPoolName("b", "out")
	if owner, _ := PoolOwner(p); owner != "" {
		t.Fatal("foreign name/owner mapped")
	}
	// A lab whose name is the start of another lab's name does not own that lab's pool.
	p.Name = names.DHCPPoolName("a-b", "out")
	if owner, _ := PoolOwner(p); owner != "" {
		t.Fatal("pool of lab a-b mapped to lab a")
	}
}

type failedReader struct {
	client.Reader
	err error
}

func (r failedReader) Get(context.Context, types.NamespacedName, client.Object, ...client.GetOption) error {
	return r.err
}
func TestDHCPDesiredDistinguishesMissingFromReadFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	_ = allocation.AddToScheme(scheme)
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g"}, Spec: lab.LabSpec{Gateways: []lab.GatewayTemplate{{Name: "out", Kind: lab.GatewayKindInternet, DHCPServer: &lab.DHCPServer{Enabled: true, Ranges: []lab.DHCPRange{{Start: 2, End: 3}}}}}}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(l).Build()
	if enabled, _, _, err := Desired(context.Background(), c, "g", "a", "out"); err != nil || enabled {
		t.Fatalf("missing optional pool %v %v", enabled, err)
	}
	if _, _, _, err := Desired(context.Background(), failedReader{c, errors.New("forbidden")}, "g", "a", "out"); err == nil {
		t.Fatal("API failure masked as disabled")
	}
}

func TestStoppedIntentDisablesDHCPAndWakesTheGateways(t *testing.T) {
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g", UID: "uid"}, Spec: lab.LabSpec{Gateways: []lab.GatewayTemplate{{Name: "attack", Kind: lab.GatewayKindVPN, DHCPServer: &lab.DHCPServer{Enabled: true, Ranges: []lab.DHCPRange{{Start: 2, End: 3}}}}}}}
	old := l.DeepCopy()
	l.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Skip"}
	if !InputsChanged(old, l) {
		t.Fatal("stop did not wake network owners")
	}
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	_ = allocation.AddToScheme(scheme)
	pool := &allocation.Pool{ObjectMeta: metav1.ObjectMeta{Name: names.DHCPPoolName("a", "attack"), Namespace: "g", OwnerReferences: []metav1.OwnerReference{{APIVersion: lab.SchemeGroupVersion.String(), Kind: "Lab", Name: "a", UID: l.UID}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(l, pool).Build()
	enabled, _, _, err := Desired(context.Background(), c, "g", "a", "attack")
	if err != nil || enabled {
		t.Fatal("stopped DHCP still enabled", err)
	}
}
