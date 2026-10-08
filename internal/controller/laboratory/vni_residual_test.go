package laboratory

import (
	"context"
	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strconv"
	"testing"
)

func TestResidualDefaultOffPruneAndDeleteRespectOwnedVNILease(t *testing.T) {
	for _, action := range []string{"prune-device", "prune-connection", "delete-lab"} {
		for _, replaced := range []bool{false, true} {
			t.Run(action+map[bool]string{false: "/current", true: "/stale-lease"}[replaced], func(t *testing.T) {
				ctx := context.Background()
				s := runtime.NewScheme()
				_ = lab.AddToScheme(s)
				_ = allocation.AddToScheme(s)
				parent := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab"}}
				labels := map[string]string{names.LabelLab: "l"}
				d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "switch", Namespace: "ns", UID: "switch", Labels: labels}, Spec: lab.DeviceSpec{Name: "switch", LabRef: "l", Type: lab.DeviceTypeUnmanagedSwitch}}
				conn := &lab.Connection{ObjectMeta: metav1.ObjectMeta{Name: "conn", Namespace: "ns", UID: "conn", Labels: labels}, Spec: lab.ConnectionSpec{LabRef: "l"}}
				bitmap, free := poolpkg.InitBitmap(names.VNIPoolSize, 0)
				p := &allocation.Pool{ObjectMeta: metav1.ObjectMeta{Name: "vni-0", Namespace: names.SystemNamespace, UID: "pool", Labels: map[string]string{poolpkg.PoolGroupLabel: "vni", poolpkg.PoolStateLabel: poolpkg.PoolStateEmpty, poolpkg.LatestPoolLabel: "true"}}, Spec: allocation.PoolSpec{Size: names.VNIPoolSize}, Status: allocation.PoolStatus{BitMap: bitmap, Free: free}}
				c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent, d, conn, p).WithObjects(parent, d, conn, p).Build()
				leases := []*poolpkg.Lease{}
				for _, object := range []client.Object{d, conn} {
					lease, err := poolpkg.AllocateOwnedIndex(ctx, c, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, string(object.GetUID()))
					if err != nil {
						t.Fatal(err)
					}
					leases = append(leases, lease)
					switch v := object.(type) {
					case *lab.Device:
						v.Status.VNI = &lease.Index
						v.Status.VNILease = &lab.VNILease{PoolUID: lease.PoolUID, Generation: lease.Generation}
					case *lab.Connection:
						v.Status.VNI = &lease.Index
						v.Status.VNILease = &lab.VNILease{PoolUID: lease.PoolUID, Generation: lease.Generation}
					}
					if err := c.Status().Update(ctx, object); err != nil {
						t.Fatal(err)
					}
				}
				original := leases[0]
				if action == "prune-connection" {
					original = leases[1]
				}
				var newer *poolpkg.Lease
				if replaced {
					if err := poolpkg.ReleaseOwnedIndex(ctx, c, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, *original); err != nil {
						t.Fatal(err)
					}
					var current allocation.Pool
					if err := c.Get(ctx, client.ObjectKeyFromObject(p), &current); err != nil {
						t.Fatal(err)
					}
					current.Annotations = map[string]string{poolpkg.CursorAnnotation: strconv.FormatUint(uint64(original.Index), 10)}
					if err := c.Update(ctx, &current); err != nil {
						t.Fatal(err)
					}
					var err error
					newer, err = poolpkg.AllocateOwnedIndex(ctx, c, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, "replacement")
					if err != nil || newer.Index != original.Index {
						t.Fatalf("fixture did not reuse original slot: %+v %v", newer, err)
					}
				}
				r := &LabReconciler{Client: c, Reader: c}
				var err error
				switch action {
				case "prune-device":
					err = r.pruneDevices(ctx, parent)
				case "prune-connection":
					err = r.pruneConnections(ctx, parent)
				case "delete-lab":
					_, err = r.reconcileDelete(ctx, parent)
				}
				if err != nil {
					t.Fatalf("default-off owned lease blocked physical delete: %v", err)
				}
				if replaced {
					if err := poolpkg.ValidateLease(ctx, c, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, *newer); err != nil {
						t.Fatalf("old physical delete freed replacement lease: %v", err)
					}
				} else if err := poolpkg.ValidateLease(ctx, c, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, *original); err == nil {
					t.Fatal("current owned reservation leaked")
				}
				if parent.Status.Lifecycle != nil || parent.Status.Resources != nil || len(parent.Status.ScopeReports) != 0 {
					t.Fatal("default-off delete manufactured lifecycle release credit")
				}
			})
		}
	}
}
func TestResidualDefaultOffOwnedReleaseKeepsUnresolvedNativeDebt(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	_ = lab.AddToScheme(s)
	_ = allocation.AddToScheme(s)
	bitmap, free := poolpkg.InitBitmap(names.VNIPoolSize, 0)
	p := &allocation.Pool{ObjectMeta: metav1.ObjectMeta{Name: "vni-0", Namespace: names.SystemNamespace, UID: "pool", Labels: map[string]string{poolpkg.PoolGroupLabel: "vni", poolpkg.PoolStateLabel: poolpkg.PoolStateEmpty, poolpkg.LatestPoolLabel: "true"}}, Spec: allocation.PoolSpec{Size: names.VNIPoolSize}, Status: allocation.PoolStatus{BitMap: bitmap, Free: free}}
	parent := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab"}}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(p, parent).WithObjects(p, parent).Build()
	lease, err := poolpkg.AllocateOwnedIndex(ctx, c, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, "original-owner")
	if err != nil {
		t.Fatal(err)
	}
	parent.Status.ScopeInventory = []lab.OwnedRuntimeIdentity{{ScopeKind: "LabFabric", OwnerUID: "lab", ScopeUID: "lab", VNIBindings: []lab.OwnedVNI{{UID: "original-owner", VNI: lease.Index, PoolUID: lease.PoolUID, LeaseGeneration: lease.Generation}}}}
	if err := c.Status().Update(ctx, parent); err != nil {
		t.Fatal(err)
	}
	r := &LabReconciler{Client: c, Reader: c}
	if err := r.releaseDefaultOffVNI(ctx, lease.Index, &lab.VNILease{PoolUID: lease.PoolUID, Generation: lease.Generation}, lease.OwnerUID); err == nil {
		t.Fatal("disabled observation bypassed retained native debt")
	}
	if err := poolpkg.ValidateLease(ctx, c, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, *lease); err != nil {
		t.Fatal("unresolved native debt freed pool reservation", err)
	}
}
