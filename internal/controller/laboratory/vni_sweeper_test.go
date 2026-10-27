package laboratory

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
)

// The one way back: leases of gone owners and held slots nobody references are retired after two sweeps a grace apart, and
// every retired VNI is returned after the quarantine in one update; a live owner's lease and a referenced slot stay.
func TestVNISweeperRetiresOrphansAndReturnsAfterTheQuarantine(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = allocationv1alpha1.AddToScheme(s)
	_ = laboratoryv1alpha1.AddToScheme(s)
	vni := uint(5)
	live := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns", UID: types.UID("uid-live")}}
	referenced := &laboratoryv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: "uid-conn"}, Status: laboratoryv1alpha1.ConnectionStatus{VNI: &vni}}
	bitmap, free := poolpkg.InitBitmap(names.VNIPoolSize, 0)
	pool := &allocationv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "vni-0", Namespace: names.SystemNamespace, UID: "pool-uid", Labels: map[string]string{poolpkg.PoolGroupLabel: names.VNIPoolPrefix}},
		Spec:       allocationv1alpha1.PoolSpec{Size: names.VNIPoolSize},
		Status: allocationv1alpha1.PoolStatus{BitMap: bitmap, Free: free, NextLeaseGeneration: 3, Leases: map[string]allocationv1alpha1.PoolLease{
			"1": {OwnerUID: "uid-live", Generation: 1}, "2": {OwnerUID: "uid-gone1", Generation: 2}, "3": {OwnerUID: "uid-gone2", Generation: 3},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(live, referenced, pool).WithStatusSubresource(pool).Build()
	get := func() allocationv1alpha1.Pool {
		var p allocationv1alpha1.Pool
		if err := c.Get(ctx, client.ObjectKey{Name: "vni-0", Namespace: names.SystemNamespace}, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// bits 1, 2, 3 (leases) and 4, 5 (held without a lease; 5 is referenced by a Connection) are set
	p := get()
	p.Status.BitMap, p.Status.Free = setBits(t, p.Status.BitMap, p.Status.Free, 1, 2, 3, 4, 5)
	if err := c.Status().Update(ctx, &p); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	sw := &VNISweeper{Client: c, Grace: 5 * time.Minute, Quarantine: 10 * time.Minute, Now: func() time.Time { return now }}
	step := func(d time.Duration) (int, int) {
		now = now.Add(d)
		r, ret, err := sw.Sweep(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return r, ret
	}
	if r, ret := step(0); r != 0 || ret != 0 {
		t.Fatalf("first sight: %d %d", r, ret)
	}
	if r, ret := step(6 * time.Minute); r != 3 || ret != 0 { // 2, 3 and the held 4
		t.Fatalf("orphans retired after the grace: %d %d", r, ret)
	}
	if p := get(); len(p.Status.Retired) != 3 || len(p.Status.Leases) != 1 {
		t.Fatalf("status: retired=%d leases=%d", len(p.Status.Retired), len(p.Status.Leases))
	}
	if r, ret := step(5 * time.Minute); r != 0 || ret != 0 {
		t.Fatalf("inside the quarantine nothing comes back: %d %d", r, ret)
	}
	if r, ret := step(6 * time.Minute); r != 0 || ret != 3 {
		t.Fatalf("after the quarantine all retired come back together: %d %d", r, ret)
	}
	p = get()
	if len(p.Status.Retired) != 0 || len(p.Status.Leases) != 1 || p.Status.Leases["1"].OwnerUID != "uid-live" {
		t.Fatalf("the live lease stays: %+v", p.Status)
	}
	held, _ := poolpkg.HeldBits(ctx, c, names.VNIPoolPrefix, names.SystemNamespace)
	if len(held) != 1 || held[0] != 5 {
		t.Fatalf("the referenced slot stays held: %v", held)
	}
}

func setBits(t *testing.T, encoded string, free uint, bits ...uint) (string, uint) {
	t.Helper()
	out, f, err := poolpkg.SetBitsForTest(encoded, free, bits...)
	if err != nil {
		t.Fatal(err)
	}
	return out, f
}
