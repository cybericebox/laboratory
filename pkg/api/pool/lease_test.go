package pool

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	api "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func leaseClient(t *testing.T) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	_ = api.AddToScheme(s)
	bitmap, free := InitBitmap(2, 0)
	p := &api.Pool{ObjectMeta: metav1.ObjectMeta{Name: "lease-0", Namespace: "owned", UID: "actual-pool", Labels: map[string]string{PoolGroupLabel: "lease", PoolStateLabel: PoolStateEmpty, LatestPoolLabel: "true"}}, Spec: api.PoolSpec{Size: 2}, Status: api.PoolStatus{BitMap: bitmap, Free: free}}
	return fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(p).WithObjects(p).Build()
}

type oldLeaseReader struct {
	client.Client
	saved *api.Pool
}

func (c oldLeaseReader) Get(ctx context.Context, key client.ObjectKey, out client.Object, opts ...client.GetOption) error {
	if p, ok := out.(*api.Pool); ok && key.Name == c.saved.Name {
		c.saved.DeepCopyInto(p)
		return nil
	}
	return c.Client.Get(ctx, key, out, opts...)
}
func TestOwnedPoolOldACKCannotClearReallocatedBitBeforeNewObjectStatus(t *testing.T) {
	for _, newUID := range []string{"replacement-uid", "original-uid"} {
		t.Run(newUID, func(t *testing.T) {
			c := leaseClient(t)
			ctx := context.Background()
			old, err := AllocateOwnedIndex(ctx, c, "lease", "owned", 2, "original-uid")
			if err != nil {
				t.Fatal(err)
			}
			var stale api.Pool
			if err := c.Get(ctx, client.ObjectKey{Namespace: "owned", Name: "lease-0"}, &stale); err != nil {
				t.Fatal(err)
			}
			if err := ReleaseOwnedIndex(ctx, c, "lease", "owned", 2, *old); err != nil {
				t.Fatal(err)
			}
			// The new actual owner has reserved the bit in the Pool CAS; its VNI status
			// write is deliberately absent. A scan of child statuses cannot protect it.
			newer, err := AllocateOwnedIndex(ctx, c, "lease", "owned", 2, newUID)
			if err != nil {
				t.Fatal(err)
			}
			if newer.Index != old.Index || newer.Generation <= old.Generation {
				t.Fatalf("fixture did not recycle exact bit/epoch: old%+v new%+v", old, newer)
			}
			if err := ReleaseOwnedIndex(ctx, oldLeaseReader{Client: c, saved: &stale}, "lease", "owned", 2, *old); !apierrors.IsConflict(err) {
				t.Fatalf("stale owner CAS did not conflict: %v", err)
			}
			if err := ReleaseOwnedIndex(ctx, c, "lease", "owned", 2, *old); err != nil {
				t.Fatal(err)
			}
			if err := ValidateLease(ctx, c, "lease", "owned", 2, *newer); err != nil {
				t.Fatalf("old ACK freed new reserved bit: %v", err)
			}
			replay, err := AllocateOwnedIndex(ctx, c, "lease", "owned", 2, newUID)
			if err != nil || *replay != *newer {
				t.Fatalf("crash before VNI status caused duplicate allocation: %+v %v", replay, err)
			}
			if err := NewRotatingAllocator(c, "lease", "owned", 2).ReleaseIndex(ctx, newer.Index); err == nil {
				t.Fatal("legacy names/index-only release bypassed owned epoch")
			}
		})
	}
}
func TestOwnedPoolIdentityAndMissingLegacyReservationStayHeld(t *testing.T) {
	ctx := context.Background()
	c := leaseClient(t)
	if _, err := PinExistingIndex(ctx, c, "lease", "owned", 2, 1, "owner"); err == nil {
		t.Fatal("clear bit rebound from API status")
	}
	index, err := NewRotatingAllocator(c, "lease", "owned", 2).AllocateIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := PinExistingIndex(ctx, c, "lease", "owned", 2, index, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PinExistingIndex(ctx, c, "lease", "owned", 2, index, "replacement"); err == nil {
		t.Fatal("current lease rebound to replacement UID")
	}
	wrong := *lease
	wrong.PoolUID = "replacement-pool"
	if err := ReleaseOwnedIndex(ctx, c, "lease", "owned", 2, wrong); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLease(ctx, c, "lease", "owned", 2, *lease); err != nil {
		t.Fatal("different Pool UID released actual lease", err)
	}
}

// PinExistingIndex tells a slot that is no longer held apart from other failures, with errors a caller can test.
func TestPinExistingIndexNamesAReleasedSlot(t *testing.T) {
	ctx := context.Background()
	c := leaseClient(t)
	// bit 1 is clear: the slot was returned to the pool
	if _, err := PinExistingIndex(ctx, c, "lease", "owned", 2, 1, "owner"); !stderrors.Is(err, ErrLeaseAbsent) {
		t.Fatalf("a clear bit: %v", err)
	}
	// set the bit and retire it: waiting out its quarantine
	var p api.Pool
	if err := c.Get(ctx, client.ObjectKey{Name: "lease-0", Namespace: "owned"}, &p); err != nil {
		t.Fatal(err)
	}
	bm, err := decodeBitmap(p.Status.BitMap, p.Spec.Size)
	if err != nil {
		t.Fatal(err)
	}
	bm.Set(1)
	p.Status.BitMap = encodeBitmap(bm)
	p.Status.Retired = map[string]api.PoolRetired{"1": {}}
	if err := c.Status().Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if _, err := PinExistingIndex(ctx, c, "lease", "owned", 2, 1, "owner"); !stderrors.Is(err, ErrLeaseRetired) {
		t.Fatalf("a retired slot: %v", err)
	}
}

// A lease is released only by its own retirement record: the same owner and generation, no lease record. A slot back in the pool, a
// legacy hold, a lease record, a retirement of another owner or generation are not.
func TestLeaseReleasedNeedsItsOwnRetirementRecord(t *testing.T) {
	ctx := context.Background()
	c := leaseClient(t)
	lease := Lease{Index: 1, PoolUID: "actual-pool", OwnerUID: "owner", Generation: 3}
	update := func(f func(p *api.Pool, set func(bool))) {
		var p api.Pool
		if err := c.Get(ctx, client.ObjectKey{Name: "lease-0", Namespace: "owned"}, &p); err != nil {
			t.Fatal(err)
		}
		bm, err := decodeBitmap(p.Status.BitMap, p.Spec.Size)
		if err != nil {
			t.Fatal(err)
		}
		f(&p, func(on bool) {
			if on {
				bm.Set(1)
			} else {
				bm.Clear(1)
			}
			p.Status.BitMap = encodeBitmap(bm)
		})
		if err := c.Status().Update(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	want := func(name string, released bool) {
		t.Helper()
		got, err := LeaseReleased(ctx, c, "lease", "owned", 2, lease)
		if err != nil || got != released {
			t.Fatalf("%s: released %v err %v, want %v", name, got, err, released)
		}
	}
	want("bit clear, back in the pool: the proof of whose lease it was is gone", false)
	update(func(p *api.Pool, set func(bool)) { set(true) })
	want("a legacy hold: bit set, no lease record, not retired", false)
	update(func(p *api.Pool, set func(bool)) {
		p.Status.Retired = map[string]api.PoolRetired{"1": {OwnerUID: "owner", Generation: 3}}
	})
	want("retired by this very lease", true)
	update(func(p *api.Pool, set func(bool)) {
		p.Status.Retired = map[string]api.PoolRetired{"1": {OwnerUID: "owner", Generation: 4}}
	})
	want("retired by a later generation of the same owner", false)
	update(func(p *api.Pool, set func(bool)) {
		p.Status.Retired = map[string]api.PoolRetired{"1": {OwnerUID: "someone-else", Generation: 3}}
	})
	want("retired by another owner", false)
	update(func(p *api.Pool, set func(bool)) {
		p.Status.Retired = map[string]api.PoolRetired{"1": {OwnerUID: "owner", Generation: 3}}
		p.Status.Leases = map[string]api.PoolLease{"1": {OwnerUID: "someone-else", Generation: 4}}
	})
	want("a lease record of another owner", false)
	update(func(p *api.Pool, set func(bool)) {
		p.Status.Retired = nil
		p.Status.Leases = map[string]api.PoolLease{"1": {OwnerUID: "owner", Generation: 3}}
	})
	want("still held by this lease", false)
	other := lease
	other.PoolUID = "another-pool"
	if got, _ := LeaseReleased(ctx, c, "lease", "owned", 2, other); got {
		t.Fatal("a lease of another pool is never released here")
	}
}

// What a node may do for a lease: drop the flows while it is held or retired by itself; never after the slot has gone to another owner,
// also when that happens between the check before the flows go and the check after (the race the second check is for).
func TestValidateOrReleasedAndTheReissueRace(t *testing.T) {
	ctx := context.Background()
	c := leaseClient(t)
	lease := Lease{Index: 1, PoolUID: "actual-pool", OwnerUID: "owner", Generation: 3}
	set := func(f func(p *api.Pool)) {
		var p api.Pool
		if err := c.Get(ctx, client.ObjectKey{Name: "lease-0", Namespace: "owned"}, &p); err != nil {
			t.Fatal(err)
		}
		bm, _ := decodeBitmap(p.Status.BitMap, p.Spec.Size)
		bm.Set(1)
		p.Status.BitMap = encodeBitmap(bm)
		f(&p)
		if err := c.Status().Update(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	// the controller has retired the lease at the stop: it is still this owner's until the node has acknowledged
	set(func(p *api.Pool) {
		p.Status.Retired = map[string]api.PoolRetired{"1": {OwnerUID: "owner", Generation: 3}}
	})
	if err := ValidateOrReleased(ctx, c, "lease", "owned", 2, lease); err != nil {
		t.Fatalf("retired by itself: %v", err)
	}
	// the slot is given back and issued to another lab: neither the check before nor the one after may pass
	set(func(p *api.Pool) {
		p.Status.Retired = nil
		p.Status.Leases = map[string]api.PoolLease{"1": {OwnerUID: "new-owner", Generation: 4}}
	})
	if err := ValidateOrReleased(ctx, c, "lease", "owned", 2, lease); err == nil {
		t.Fatal("a slot issued to another owner must never be dropped for the old lease")
	}
	// back in the pool without a record: not provable either
	set(func(p *api.Pool) { p.Status.Leases = nil })
	var q api.Pool
	if err := c.Get(ctx, client.ObjectKey{Name: "lease-0", Namespace: "owned"}, &q); err != nil {
		t.Fatal(err)
	}
	bm, _ := decodeBitmap(q.Status.BitMap, q.Spec.Size)
	bm.Clear(1)
	q.Status.BitMap = encodeBitmap(bm)
	if err := c.Status().Update(ctx, &q); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOrReleased(ctx, c, "lease", "owned", 2, lease); err == nil {
		t.Fatal("a slot back in the pool is not proof")
	}

	// the race: the check before the flows passes (retired by itself), the slot is reissued while the flows are dropped, the check after fails
	set(func(p *api.Pool) {
		p.Status.Retired = map[string]api.PoolRetired{"1": {OwnerUID: "owner", Generation: 3}}
	})
	if err := ValidateOrReleased(ctx, c, "lease", "owned", 2, lease); err != nil { // before
		t.Fatalf("before: %v", err)
	}
	set(func(p *api.Pool) { // reissued during the barrier
		p.Status.Retired = nil
		p.Status.Leases = map[string]api.PoolLease{"1": {OwnerUID: "new-owner", Generation: 5}}
	})
	if err := ValidateOrReleased(ctx, c, "lease", "owned", 2, lease); err == nil { // after: no acknowledgement
		t.Fatal("the release of a reissued slot must not be acknowledged")
	}
}

// A retired slot whose obligation is still open (a node has not yet acknowledged that it dropped the flows) is not returned after its
// quarantine, and is returned once the obligation is gone.
func TestReturnMaturedUnlessHoldsTheOpenObligations(t *testing.T) {
	ctx := context.Background()
	c := leaseClient(t)
	var p api.Pool
	if err := c.Get(ctx, client.ObjectKey{Name: "lease-0", Namespace: "owned"}, &p); err != nil {
		t.Fatal(err)
	}
	bm, _ := decodeBitmap(p.Status.BitMap, p.Spec.Size)
	bm.Set(1)
	p.Status.BitMap = encodeBitmap(bm)
	p.Status.Free--
	retiredAt := metav1.NewTime(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC))
	p.Status.Retired = map[string]api.PoolRetired{"1": {OwnerUID: "owner", Generation: 3, RetiredAt: retiredAt}}
	if err := c.Status().Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	later := retiredAt.Add(time.Hour)
	open := func(index uint, owner string, generation int64) bool {
		return index == 1 && owner == "owner" && generation == 3
	}
	if n, err := ReturnMaturedUnless(ctx, c, "lease", "owned", 2, 10*time.Minute, later, open); err != nil || n != 0 {
		t.Fatalf("an open obligation keeps the slot: %d %v", n, err)
	}
	if released, _ := LeaseReleased(ctx, c, "lease", "owned", 2, Lease{Index: 1, PoolUID: "actual-pool", OwnerUID: "owner", Generation: 3}); !released {
		t.Fatal("while held back, the retirement record still proves the lease")
	}
	if n, err := ReturnMaturedUnless(ctx, c, "lease", "owned", 2, 10*time.Minute, later, func(uint, string, int64) bool { return false }); err != nil || n != 1 {
		t.Fatalf("no obligation: the slot comes back: %d %v", n, err)
	}
}
