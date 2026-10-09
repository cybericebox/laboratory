package pool

import (
	"context"
	"testing"

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
