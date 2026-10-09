package pool

import (
	"context"
	"testing"

	"github.com/bits-and-blooms/bitset"
	api "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAllocatorUsesDeclaredExtentAcrossCompactBitmapReload(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Pool{}).Build()
	ctx := context.Background()
	for want := uint(1); want < 128; want++ {
		a := NewRotatingAllocator(c, "extent", "owned", 128)
		got, err := a.AllocateIndex(ctx)
		if err != nil {
			t.Fatalf("allocation%d failed early: %v", want, err)
		}
		if got != want {
			t.Fatalf("got%d want%d", got, want)
		}
	}
	a := NewRotatingAllocator(c, "extent", "owned", 128)
	if err := a.ReleaseIndex(ctx, 70); err != nil {
		t.Fatal(err)
	}
	if got, err := a.AllocateIndex(ctx); err != nil || got != 70 {
		t.Fatalf("release/wrap: got%d err%v", got, err)
	}
	if got, err := a.AllocateIndex(ctx); err != nil || got != 128 {
		t.Fatalf("full pool rotation: got%d err%v", got, err)
	}
}

type stalePoolList struct {
	client.Client
	pool *api.Pool
}

func (c *stalePoolList) List(ctx context.Context, out client.ObjectList, opts ...client.ListOption) error {
	if list, ok := out.(*api.PoolList); ok {
		list.Items = []api.Pool{*c.pool.DeepCopy()}
		return nil
	}
	return c.Client.List(ctx, out, opts...)
}
func TestAllocatorConflictDoesNotHandOutDuplicateThenRetries(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	bitmap, free := InitBitmap(128, 0)
	pool := &api.Pool{ObjectMeta: metav1.ObjectMeta{Name: "race-0", Namespace: "owned", Labels: map[string]string{PoolStateLabel: PoolStateEmpty, LatestPoolLabel: "true", PoolGroupLabel: "race"}}, Spec: api.PoolSpec{Size: 128}, Status: api.PoolStatus{Free: free, BitMap: bitmap}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Pool{}).WithObjects(pool).Build()
	ctx := context.Background()
	var original api.Pool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &original); err != nil {
		t.Fatal(err)
	}
	if got, err := NewRotatingAllocator(c, "race", "owned", 128).AllocateIndex(ctx); err != nil || got != 1 {
		t.Fatalf("first got%d err%v", got, err)
	}
	stale := NewRotatingAllocator(&stalePoolList{Client: c, pool: &original}, "race", "owned", 128)
	if got, err := stale.AllocateIndex(ctx); !apierrors.IsConflict(err) {
		t.Fatalf("stale allocator must conflict rather than return%d: %v", got, err)
	}
	if got, err := NewRotatingAllocator(c, "race", "owned", 128).AllocateIndex(ctx); err != nil || got != 2 {
		t.Fatalf("fresh retry got%d err%v", got, err)
	}
}

func TestAllocatorKeepsLargeUnusedTailCompact(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Pool{}).Build()
	ctx := context.Background()
	a := NewRotatingAllocator(c, "compact", "owned", 65000)
	for i := 0; i < 65; i++ {
		if _, err := a.AllocateIndex(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var pool api.Pool
	if err := c.Get(ctx, client.ObjectKey{Namespace: "owned", Name: "compact-0"}, &pool); err != nil {
		t.Fatal(err)
	}
	if len(pool.Status.BitMap) > 128 {
		t.Fatalf("unused tail expanded API payload to%d bytes", len(pool.Status.BitMap))
	}
}

func TestAllocatorNeverLeavesDeclaredBounds(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	bitmap := bitset.New(256)
	for i := uint(0); i < 128; i++ {
		bitmap.Set(i)
	}
	pool := &api.Pool{ObjectMeta: metav1.ObjectMeta{Name: "bounded-0", Namespace: "owned", Labels: map[string]string{PoolStateLabel: PoolStatePartial, LatestPoolLabel: "true", PoolGroupLabel: "bounded"}}, Spec: api.PoolSpec{Size: 128}, Status: api.PoolStatus{Free: 1, BitMap: encodeBitmap(bitmap)}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Pool{}).WithObjects(pool).Build()
	a := NewAllocator(c, "bounded", "owned", 128)
	if got, err := a.AllocateIndex(context.Background()); err == nil {
		t.Fatalf("allocated out-of-range%d from oversized legacy bitmap", got)
	}
}
