//go:build linux

package nodeagent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
)

type goneOwnerFixture struct {
	o        *NativeRuntimeObserver
	c        client.Client
	flows    *FlowManager
	binding  lab.OwnedVNI
	barriers *int
}

// A Connection (a direct link, a switch) deleted before its Lab leaves its VNI lease in the pool and its flows on the node. Only the pool
// can still say whose the number is; the scope of the Lab must be able to release it.
func goneOwnerTest(t *testing.T, edit func(*allocation.Pool, *lab.Lab, *lab.OwnedVNI, *[]client.Object)) *goneOwnerFixture {
	t.Helper()
	s := runtime.NewScheme()
	_ = lab.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = allocation.AddToScheme(s)
	bitmap, free := poolpkg.InitBitmap(names.VNIPoolSize, 0)
	bitmap, free, err := poolpkg.SetBitsForTest(bitmap, free, 13)
	if err != nil {
		t.Fatal(err)
	}
	pool := &allocation.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "vni-0", Namespace: names.SystemNamespace, UID: "pool-uid", Labels: map[string]string{poolpkg.PoolGroupLabel: names.VNIPoolPrefix}},
		Spec:       allocation.PoolSpec{Size: names.VNIPoolSize},
		Status:     allocation.PoolStatus{BitMap: bitmap, Free: free, Leases: map[string]allocation.PoolLease{"13": {OwnerUID: "conn-uid", Generation: 13}}},
	}
	parent := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab", Generation: 3}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 2}}}
	binding := lab.OwnedVNI{Kind: "Connection", Namespace: "ns", Name: "l--vpn-eth0--web-eth0", UID: "conn-uid", VNI: 13, PoolUID: "pool-uid", LeaseGeneration: 13, OwnerUID: "lab", OperationID: "stop", Revision: 2, Generation: 3}
	extra := []client.Object{}
	if edit != nil {
		edit(pool, parent, &binding, &extra)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append([]client.Object{pool, parent}, extra...)...).Build()
	barriers := new(int)
	flows := &FlowManager{client: scopeFlowAdapterWith(t, func() map[string]uint32 { return nil }, func() { *barriers++ }), floods: map[uint]t6Flood{}}
	o := &NativeRuntimeObserver{Reader: c, NodeName: "node", BootID: "boot", JournalDir: t.TempDir(), Network: &NetworkAttachReconciler{OVS: &OVSManager{}, Flows: flows}}
	return &goneOwnerFixture{o: o, c: c, flows: flows, binding: binding, barriers: barriers}
}

func TestVNIOfAConnectionDeletedBeforeItsLabIsReleasedThroughThePool(t *testing.T) {
	f := goneOwnerTest(t, nil)
	if err := f.o.retireVNI(context.Background(), f.binding, f.flows); err != nil {
		t.Fatalf("a lease whose owner is gone could not be released: %v", err)
	}
	if *f.barriers != 1 || !f.o.vniReleased(f.binding) {
		t.Fatalf("barriers=%d released=%v", *f.barriers, f.o.vniReleased(f.binding))
	}
}

func TestVNIOfAGoneOwnerIsNeverReleasedOnAnyDoubt(t *testing.T) {
	vni := uint(13)
	for name, edit := range map[string]func(*allocation.Pool, *lab.Lab, *lab.OwnedVNI, *[]client.Object){
		"the pool gave the number to another owner": func(p *allocation.Pool, _ *lab.Lab, _ *lab.OwnedVNI, _ *[]client.Object) {
			p.Status.Leases["13"] = allocation.PoolLease{OwnerUID: "someone-else", Generation: 14}
		},
		"another generation of the lease": func(_ *allocation.Pool, _ *lab.Lab, b *lab.OwnedVNI, _ *[]client.Object) { b.LeaseGeneration = 12 },
		"the slot is back in the pool": func(p *allocation.Pool, _ *lab.Lab, _ *lab.OwnedVNI, _ *[]client.Object) {
			delete(p.Status.Leases, "13")
			p.Status.BitMap, p.Status.Free, _ = poolpkg.SetBitsForTest(p.Status.BitMap, p.Status.Free)
		},
		"a replacement Connection holds the number": func(_ *allocation.Pool, _ *lab.Lab, _ *lab.OwnedVNI, x *[]client.Object) {
			*x = append(*x, &lab.Connection{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns", UID: "replacement"}, Status: lab.ConnectionStatus{VNI: &vni}})
		},
		"the owner still exists under its UID": func(_ *allocation.Pool, _ *lab.Lab, _ *lab.OwnedVNI, x *[]client.Object) {
			*x = append(*x, &lab.Connection{ObjectMeta: metav1.ObjectMeta{Name: "l--vpn-eth0--web-eth0", Namespace: "ns", UID: "conn-uid"}})
		},
		"the Lab of the lease is not there": func(_ *allocation.Pool, l *lab.Lab, _ *lab.OwnedVNI, _ *[]client.Object) { l.UID = "another-lab" },
	} {
		t.Run(name, func(t *testing.T) {
			f := goneOwnerTest(t, edit)
			if err := f.o.retireVNI(context.Background(), f.binding, f.flows); err == nil || *f.barriers != 0 || f.o.vniReleased(f.binding) {
				t.Fatalf("released on a doubt: err=%v barriers=%d released=%v", err, *f.barriers, f.o.vniReleased(f.binding))
			}
		})
	}
}

// The scope of the Lab (LabFabric) records the binding of a gone owner instead of failing on it, as long as the pool still holds the lease.
func TestLabFabricScopeAcceptsAGoneOwnersLeaseOnlyWhileThePoolHoldsIt(t *testing.T) {
	for _, held := range []bool{true, false} {
		f := goneOwnerTest(t, func(p *allocation.Pool, _ *lab.Lab, _ *lab.OwnedVNI, _ *[]client.Object) {
			if !held {
				p.Status.Leases["13"] = allocation.PoolLease{OwnerUID: "someone-else", Generation: 14}
			}
		})
		id := lab.OwnedRuntimeIdentity{ScopeKind: "LabFabric", ScopeUID: "lab", OwnerUID: "lab", Namespace: "ns", LabName: "l", NodeName: "node", NodeBootID: "boot", VNIBindings: []lab.OwnedVNI{f.binding}}
		err := f.o.captureScopeFabric(context.Background(), &id)
		if held && err != nil {
			t.Fatalf("a held lease of a gone owner must not fail the scope: %v", err)
		}
		if !held && err == nil {
			t.Fatal("a lease the pool gave to someone else was accepted")
		}
	}
}
