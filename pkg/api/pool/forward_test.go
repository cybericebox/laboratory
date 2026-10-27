package pool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

func forwardFixture(t *testing.T, w *writeCounter) *ForwardAllocator {
	t.Helper()
	s := runtime.NewScheme()
	if err := allocationv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&allocationv1alpha1.Pool{}).WithInterceptorFuncs(w.funcs()).Build()
	return &ForwardAllocator{Client: c, Prefix: "lab-subnets", Namespace: "g", Size: 254}
}

// An allocation is one write of the pool (the first one also creates it), and the indexes only go forward; the insurance does not run
// before the pool has come to its end.
func TestForwardAllocatorWritesOnceAndGoesForward(t *testing.T) {
	ctx := context.Background()
	var w writeCounter
	a := forwardFixture(t, &w)
	var inUseCalls atomic.Int32
	a.InUse = func(context.Context) (map[uint]bool, error) { inUseCalls.Add(1); return nil, nil }
	if _, err := a.AllocateIndex(ctx); err != nil { // creates the pool: the create is not counted, the init and the allocation are
		t.Fatal(err)
	}
	before := w.writes.Load()
	last := uint(0)
	for i := 0; i < 100; i++ {
		n, err := a.AllocateIndex(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n <= last {
			t.Fatalf("indexes must only go forward: %d after %d", n, last)
		}
		last = n
	}
	if got := w.writes.Load() - before; got != 100 {
		t.Fatalf("one write per allocation, got %d for 100", got)
	}
	if inUseCalls.Load() != 0 {
		t.Fatal("the insurance must not run before the pool has come to its end")
	}
}

// A group whose labs come and go outlives its 253 indexes: the pool is taken round once the end is near, never hands out an index
// something still refers to, and never one given out a moment ago.
func TestForwardAllocatorNeverRunsOutAndNeverRepeatsALiveIndex(t *testing.T) {
	ctx := context.Background()
	var w writeCounter
	a := forwardFixture(t, &w)
	clock := time.Now().Add(time.Hour)
	a.Now = func() time.Time { return clock }
	live := map[uint]bool{}
	var order []uint
	a.InUse = func(context.Context) (map[uint]bool, error) {
		out := map[uint]bool{}
		for k := range live {
			out[k] = true
		}
		return out, nil
	}
	for i := 0; i < 1500; i++ {
		n, err := a.AllocateIndex(ctx)
		if err != nil {
			t.Fatalf("allocation %d: %v", i, err)
		}
		if live[n] {
			t.Fatalf("allocation %d handed out %d, which something still refers to", i, n)
		}
		live[n] = true
		order = append(order, n)
		if len(order) > 60 { // the oldest lab is deleted; nothing is returned to the pool
			delete(live, order[0])
			order = order[1:]
		}
		if i%7 == 0 {
			clock = clock.Add(3 * time.Minute) // time passes: nothing is recent for long
		}
	}
}

// With every index in use and recent, the pool is full: an error, no repeated index.
func TestForwardAllocatorFullPoolRefuses(t *testing.T) {
	ctx := context.Background()
	var w writeCounter
	a := forwardFixture(t, &w)
	a.Size = 6
	clock := time.Now().Add(time.Hour)
	a.Now = func() time.Time { return clock }
	live := map[uint]bool{}
	a.InUse = func(context.Context) (map[uint]bool, error) { return live, nil }
	for i := 0; i < 5; i++ { // bit 0 is reserved: 5 usable
		n, err := a.AllocateIndex(ctx)
		if err != nil {
			t.Fatal(err)
		}
		live[n] = true
	}
	if _, err := a.AllocateIndex(ctx); err == nil {
		t.Fatal("a full pool of live indexes must refuse")
	}
}
