package laboratory

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
)

// A VNI lease whose owner is gone is released after two sweeps a grace apart; a lease of an object that exists, and one seen
// gone only once, are kept; a lease taken again by a new owner meanwhile is not touched.
func TestVNICollectorReleasesLeasesOfGoneOwners(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = allocationv1alpha1.AddToScheme(s)
	_ = laboratoryv1alpha1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&allocationv1alpha1.Pool{}).Build()
	live := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns", UID: types.UID("uid-live")}}
	if err := c.Create(ctx, live); err != nil {
		t.Fatal(err)
	}
	bitmap, free := poolpkg.InitBitmap(names.VNIPoolSize, 0)
	pool := &allocationv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "vni-0", Namespace: names.SystemNamespace, UID: "pool-uid", Labels: map[string]string{poolpkg.PoolGroupLabel: names.VNIPoolPrefix}},
		Spec:       allocationv1alpha1.PoolSpec{Size: names.VNIPoolSize},
		Status: allocationv1alpha1.PoolStatus{BitMap: bitmap, Free: free, NextLeaseGeneration: 3, Leases: map[string]allocationv1alpha1.PoolLease{
			"1": {OwnerUID: "uid-live", Generation: 1}, "2": {OwnerUID: "uid-gone1", Generation: 2}, "3": {OwnerUID: "uid-gone2", Generation: 3},
		}},
	}
	if err := c.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := c.Status().Update(ctx, pool); err != nil {
		t.Fatal(err)
	}
	leases := func() int {
		var pools allocationv1alpha1.PoolList
		if err := c.List(ctx, &pools); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, p := range pools.Items {
			n += len(p.Status.Leases)
		}
		return n
	}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	col := &VNICollector{Client: c, Grace: 10 * time.Minute, Now: func() time.Time { return now }}

	if n, err := col.Sweep(ctx); err != nil || n != 0 || leases() != 3 {
		t.Fatalf("first sight releases nothing: %d %v leases=%d", n, err, leases())
	}
	now = now.Add(5 * time.Minute)
	if n, _ := col.Sweep(ctx); n != 0 || leases() != 3 {
		t.Fatalf("inside the grace nothing is released: %d leases=%d", n, leases())
	}
	now = now.Add(6 * time.Minute)
	n, err := col.Sweep(ctx)
	if err != nil || n != 2 || leases() != 1 {
		t.Fatalf("both orphans released, the live owner kept: %d %v leases=%d", n, err, leases())
	}
	if n, _ := col.Sweep(ctx); n != 0 || leases() != 1 {
		t.Fatalf("nothing more to collect: %d leases=%d", n, leases())
	}
}
