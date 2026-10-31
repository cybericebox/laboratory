package laboratory

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/vniledger"
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

// A retired VNI whose release no node has acknowledged yet stays out of the pool after its quarantine (another lab must not get a number a
// node may still carry flows of); once the Lab no longer records the obligation (acknowledged, or the Lab gone) it comes back.
func TestVNISweeperKeepsAReleaseThatIsNotAcknowledged(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = allocationv1alpha1.AddToScheme(s)
	_ = laboratoryv1alpha1.AddToScheme(s)
	binding := laboratoryv1alpha1.OwnedVNI{PoolUID: "pool-uid", LeaseGeneration: 2, OwnerUID: "lab-uid", Kind: "Device", Namespace: "ns", Name: "d", UID: "uid-gone", VNI: 7}
	parent := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "ns", UID: "lab-uid"},
		Status: laboratoryv1alpha1.LabStatus{ScopeInventory: []laboratoryv1alpha1.OwnedRuntimeIdentity{{OwnerUID: "lab-uid", VNIBindings: []laboratoryv1alpha1.OwnedVNI{binding}}}}}
	bitmap, free := poolpkg.InitBitmap(names.VNIPoolSize, 0)
	pool := &allocationv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "vni-0", Namespace: names.SystemNamespace, UID: "pool-uid", Labels: map[string]string{poolpkg.PoolGroupLabel: names.VNIPoolPrefix}},
		Spec:       allocationv1alpha1.PoolSpec{Size: names.VNIPoolSize},
		Status: allocationv1alpha1.PoolStatus{BitMap: bitmap, Free: free, Retired: map[string]allocationv1alpha1.PoolRetired{
			"7": {OwnerUID: "uid-gone", Generation: 2, RetiredAt: metav1.NewTime(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC))}}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(parent, pool).WithStatusSubresource(pool, parent).Build()
	p := allocationv1alpha1.Pool{}
	if err := c.Get(ctx, client.ObjectKey{Name: "vni-0", Namespace: names.SystemNamespace}, &p); err != nil {
		t.Fatal(err)
	}
	p.Status.BitMap, p.Status.Free = setBits(t, p.Status.BitMap, p.Status.Free, 7)
	if err := c.Status().Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC) // two hours after the retirement: far past the quarantine
	sw := &VNISweeper{Client: c, Grace: time.Minute, Quarantine: 10 * time.Minute, RuntimeObservation: true, Now: func() time.Time { return now }}

	if _, returned, err := sw.Sweep(ctx); err != nil || returned != 0 {
		t.Fatalf("an unacknowledged release must stay: returned %d, %v", returned, err)
	}
	var kept allocationv1alpha1.Pool
	if err := c.Get(ctx, client.ObjectKey{Name: "vni-0", Namespace: names.SystemNamespace}, &kept); err != nil || len(kept.Status.Retired) != 1 {
		t.Fatalf("the retirement record must stay: %+v %v", kept.Status.Retired, err)
	}
	// the price of the hold is visible: one held slot, retired two hours ago
	if got := gaugeValue(vniHeldSlots); got != 1 {
		t.Fatalf("held slots metric = %v, want 1", got)
	}
	if got := gaugeValue(vniHeldOldest); got != (2 * time.Hour).Seconds() {
		t.Fatalf("oldest held metric = %v, want 7200", got)
	}
	// the Lab is gone: no obligation, the slot comes back
	if err := c.Delete(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if _, returned, err := sw.Sweep(ctx); err != nil || returned != 1 {
		t.Fatalf("with no obligation the slot comes back: returned %d, %v", returned, err)
	}
	if gaugeValue(vniHeldSlots) != 0 || gaugeValue(vniHeldOldest) != 0 {
		t.Fatalf("nothing is held any more: %v %v", gaugeValue(vniHeldSlots), gaugeValue(vniHeldOldest))
	}
}

func gaugeValue(g prometheus.Gauge) float64 {
	var m dto.Metric
	_ = g.Write(&m)
	return m.GetGauge().GetValue()
}

// A Lab that is gone owes nobody an acknowledgement, but a node whose ledger still has the number may carry its flows: the slot stays
// until that node takes the number out, whether or not the node still exists. A node that is not in the cluster is counted.
func TestVNISweeperKeepsASlotWhileANodeLedgerHasIt(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = allocationv1alpha1.AddToScheme(s)
	_ = laboratoryv1alpha1.AddToScheme(s)
	bitmap, free := poolpkg.InitBitmap(names.VNIPoolSize, 0)
	pool := &allocationv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "vni-0", Namespace: names.SystemNamespace, UID: "pool-uid", Labels: map[string]string{poolpkg.PoolGroupLabel: names.VNIPoolPrefix}},
		Spec:       allocationv1alpha1.PoolSpec{Size: names.VNIPoolSize},
		Status: allocationv1alpha1.PoolStatus{BitMap: bitmap, Free: free, Retired: map[string]allocationv1alpha1.PoolRetired{
			"7": {OwnerUID: "uid-gone", Generation: 2, RetiredAt: metav1.NewTime(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC))}}},
	}
	ledger := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: vniledger.ConfigMapName("gone-node"), Namespace: names.SystemNamespace,
			Labels: map[string]string{vniledger.Label: "true"}, Annotations: map[string]string{vniledger.NodeAnnotation: "gone-node"}},
		Data: map[string]string{"7": "2026-10-31T12:00:00Z"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pool, ledger).WithStatusSubresource(pool).Build()
	p := allocationv1alpha1.Pool{}
	if err := c.Get(ctx, client.ObjectKey{Name: "vni-0", Namespace: names.SystemNamespace}, &p); err != nil {
		t.Fatal(err)
	}
	p.Status.BitMap, p.Status.Free = setBits(t, p.Status.BitMap, p.Status.Free, 7)
	if err := c.Status().Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	// no Lab at all, runtime observation off: only the ledger can hold the slot
	sw := &VNISweeper{Client: c, Grace: time.Minute, Quarantine: 10 * time.Minute, Now: func() time.Time { return now }}
	if _, returned, err := sw.Sweep(ctx); err != nil || returned != 0 {
		t.Fatalf("a slot in a node ledger must stay: returned %d, %v", returned, err)
	}
	if got := gaugeValue(vniHeldSlots); got != 1 {
		t.Fatalf("held slots metric = %v, want 1", got)
	}
	if got := gaugeValue(vniLedgerMissingNodes); got != 1 {
		t.Fatalf("missing node metric = %v, want 1 (the node is not in the cluster)", got)
	}
	// a ledger with a key that is not a number cannot be understood: the sweep fails, nothing comes back
	broken := ledger.DeepCopy()
	broken.Data = map[string]string{"x": "y"}
	if err := c.Update(ctx, broken); err != nil {
		t.Fatal(err)
	}
	if _, returned, err := sw.Sweep(ctx); err == nil || returned != 0 {
		t.Fatalf("an unreadable ledger must stop the sweep: returned %d, %v", returned, err)
	}
	// the node took the number out (its flows are confirmed gone): the slot comes back
	var cur corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(ledger), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Data = nil
	if err := c.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	if _, returned, err := sw.Sweep(ctx); err != nil || returned != 1 {
		t.Fatalf("with the ledger empty the slot comes back: returned %d, %v", returned, err)
	}
}
