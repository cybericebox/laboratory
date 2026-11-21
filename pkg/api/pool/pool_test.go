package pool

import (
	"context"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
)

func newAllocator(t *testing.T, rotating bool) Allocator {
	t.Helper()
	s := runtime.NewScheme()
	if err := allocationv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&allocationv1alpha1.Pool{}).Build()
	if rotating {
		return NewRotatingAllocator(c, "vni", "ns", 16)
	}
	return NewAllocator(c, "vni", "ns", 16)
}

func alloc(t *testing.T, a Allocator) uint {
	t.Helper()
	i, err := a.AllocateIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return i
}

// The plain allocator gives the lowest free index again at once; the rotating one walks the pool first.
func TestRotatingAllocatorDoesNotReuseAtOnce(t *testing.T) {
	ctx := context.Background()
	plain := newAllocator(t, false)
	first := alloc(t, plain)
	alloc(t, plain)
	if err := plain.ReleaseIndex(ctx, first); err != nil {
		t.Fatal(err)
	}
	if got := alloc(t, plain); got != first {
		t.Fatalf("the plain allocator reuses the lowest free index: %d, want %d", got, first)
	}

	rot := newAllocator(t, true)
	a, b := alloc(t, rot), alloc(t, rot)
	if b != a+1 {
		t.Fatalf("indexes go up: %d %d", a, b)
	}
	if err := rot.ReleaseIndex(ctx, a); err != nil {
		t.Fatal(err)
	}
	c := alloc(t, rot)
	if c == a || c <= b {
		t.Fatalf("the released index %d must not come back at once, got %d after %d", a, c, b)
	}
	// once the pool has been walked it wraps and finds the free ones
	seen := map[uint]bool{a: false}
	for i := 0; i < 13; i++ {
		seen[alloc(t, rot)] = true
	}
	if !seen[a] {
		t.Fatal("after a full walk the released index is handed out again")
	}
}

// Workers of one process allocate at the same time: every one gets its own index.
func TestAllocatorConcurrentWorkersGetDistinctIndexes(t *testing.T) {
	a := newAllocator(t, true)
	const workers = 12
	got := make([]uint, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			idx, err := a.AllocateIndex(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			got[i] = idx
		}()
	}
	wg.Wait()
	seen := map[uint]bool{}
	for _, idx := range got {
		if seen[idx] {
			t.Fatalf("index %d was handed out twice: %v", idx, got)
		}
		seen[idx] = true
	}
}

// A write that loses to another writer (or to a cache that has not seen our last write) is tried again, not returned.
func TestAllocatorRetriesALostWrite(t *testing.T) {
	s := runtime.NewScheme()
	if err := allocationv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	conflicts, initDone := 3, false
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&allocationv1alpha1.Pool{}).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*allocationv1alpha1.Pool); ok && initDone && conflicts > 0 {
				conflicts--
				return apierrors.NewConflict(schema.GroupResource{Resource: "pools"}, obj.GetName(), nil)
			}
			initDone = true // the first status write is the initialisation of the pool
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		}}).Build()
	a := NewRotatingAllocator(c, "vni", "ns", 16)
	if _, err := a.AllocateIndex(context.Background()); err != nil {
		t.Fatalf("an allocation that lost its write three times must go on: %v", err)
	}
	if conflicts != 0 {
		t.Fatalf("the conflicts were not hit: %d left", conflicts)
	}
}
