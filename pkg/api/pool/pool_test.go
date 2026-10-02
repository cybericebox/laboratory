package pool

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
