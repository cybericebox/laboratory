package pool

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bits-and-blooms/bitset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
)

type writeCounter struct{ writes, conflicts atomic.Int64 }

func (w *writeCounter) funcs() interceptor.Funcs {
	count := func(err error) error {
		w.writes.Add(1)
		if apierrors.IsConflict(err) {
			w.conflicts.Add(1)
		}
		return err
	}
	return interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			return count(c.SubResource(sub).Update(ctx, obj, opts...))
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			return count(c.Update(ctx, obj, opts...))
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return count(c.Patch(ctx, obj, patch, opts...))
		},
	}
}

// seededPool is a pool of `size` slots whose slots 1..n are leased to owner-<bit>.
func seededClient(t *testing.T, size uint, n int, w *writeCounter) (client.Client, []Lease) {
	t.Helper()
	s := runtime.NewScheme()
	if err := allocationv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	bm := bitset.New(size).Compact()
	bm.Set(0)
	leases := map[string]allocationv1alpha1.PoolLease{}
	var out []Lease
	for b := 1; b <= n; b++ {
		bm.Set(uint(b))
		leases[strconv.Itoa(b)] = allocationv1alpha1.PoolLease{OwnerUID: "owner-" + strconv.Itoa(b), Generation: int64(b)}
		out = append(out, Lease{Index: uint(b), PoolUID: "pool-uid", OwnerUID: "owner-" + strconv.Itoa(b), Generation: int64(b)})
	}
	p := &allocationv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: "vni-0", Namespace: "ns", UID: "pool-uid", Labels: map[string]string{PoolGroupLabel: "vni", PoolStateLabel: PoolStatePartial, LatestPoolLabel: "true"}},
		Spec:       allocationv1alpha1.PoolSpec{Size: size},
		Status:     allocationv1alpha1.PoolStatus{BitMap: encodeBitmap(bm), Free: size - 1 - uint(n), Leases: leases, NextLeaseGeneration: int64(n)},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(p).WithStatusSubresource(p).WithInterceptorFuncs(w.funcs()).Build()
	return c, out
}

func runConcurrently(workers int, items []Lease, fn func(Lease)) {
	var wg sync.WaitGroup
	ch := make(chan Lease)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for l := range ch {
				fn(l)
			}
		}()
	}
	for _, l := range items {
		ch <- l
	}
	close(ch)
	wg.Wait()
}

// Churn: 240 leases of one pool are given back by 8 workers. One by one (ReleaseOwnedIndex) every release writes the pool (status,
// label, ...) and the workers conflict; retired in batches and returned by the sweep it is a handful of writes. The numbers are
// logged (go test -v).
func TestRetireBatchesTheWritesOfAChurn(t *testing.T) {
	ctx := context.Background()
	const size, n = 1000, 240

	var before writeCounter
	c, leases := seededClient(t, size, n, &before)
	runConcurrently(8, leases, func(l Lease) {
		for try := 0; try < 50; try++ {
			if err := ReleaseOwnedIndex(ctx, c, "vni", "ns", size, l); err == nil || !apierrors.IsConflict(err) {
				return
			}
		}
	})
	t.Logf("one by one: %d pool writes, %d conflicts for %d releases", before.writes.Load(), before.conflicts.Load(), n)

	var after writeCounter
	c, leases = seededClient(t, size, n, &after)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	// the controller queues; a flush every few releases writes the batch
	var mu sync.Mutex
	var queue []Retirement
	flush := func() {
		mu.Lock()
		batch := queue
		queue = nil
		mu.Unlock()
		if len(batch) > 0 {
			if failed, err := Retire(ctx, c, "vni", "ns", size, batch, now); err != nil || len(failed) > 0 {
				t.Errorf("retire: %v %v", failed, err)
			}
		}
	}
	count := 0
	runConcurrently(8, leases, func(l Lease) {
		mu.Lock()
		queue = append(queue, Retirement{Index: l.Index, Lease: &l})
		count++
		due := count%60 == 0
		mu.Unlock()
		if due {
			flush()
		}
	})
	flush()
	if _, err := ReturnMatured(ctx, c, "vni", "ns", size, 10*time.Minute, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var p allocationv1alpha1.Pool
	if err := c.Get(ctx, client.ObjectKey{Name: "vni-0", Namespace: "ns"}, &p); err != nil || len(p.Status.Retired) != n || p.Status.Free != size-1-n+0 {
		t.Fatalf("inside the quarantine nothing is returned: retired=%d free=%d err=%v", len(p.Status.Retired), p.Status.Free, err)
	}
	if got, err := ReturnMatured(ctx, c, "vni", "ns", size, 10*time.Minute, now.Add(11*time.Minute)); err != nil || got != n {
		t.Fatalf("after the quarantine all %d come back in one go: %d %v", n, got, err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "vni-0", Namespace: "ns"}, &p); err != nil || len(p.Status.Retired) != 0 || len(p.Status.Leases) != 0 || p.Status.Free != size-1 {
		t.Fatalf("pool after the return: %+v %v", p.Status.Free, err)
	}
	t.Logf("retired in batches and returned by the sweep: %d pool writes, %d conflicts for %d releases", after.writes.Load(), after.conflicts.Load(), n)
	if after.writes.Load()*10 > before.writes.Load() {
		t.Fatalf("batching must cut the writes at least tenfold: %d -> %d", before.writes.Load(), after.writes.Load())
	}
}

// A retired slot is not given out, a stale lease (the slot was retired or taken again) retires nothing, and a held slot without
// a lease can be retired by its number.
func TestRetireRespectsLeasesAndKeepsTheBitSet(t *testing.T) {
	ctx := context.Background()
	var w writeCounter
	c, leases := seededClient(t, 100, 3, &w)
	now := time.Now()
	stale := leases[0]
	stale.Generation = 99
	if failed, err := Retire(ctx, c, "vni", "ns", 100, []Retirement{{Index: 1, Lease: &stale}, {Index: 2, Lease: &leases[1]}, {Index: 50}}, now); err != nil || len(failed) != 0 {
		t.Fatal(failed, err)
	}
	var p allocationv1alpha1.Pool
	if err := c.Get(ctx, client.ObjectKey{Name: "vni-0", Namespace: "ns"}, &p); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Status.Leases["1"]; !ok {
		t.Error("a lease of another generation must not be retired")
	}
	if _, ok := p.Status.Retired["2"]; !ok {
		t.Error("the lease with the right owner and generation is retired")
	}
	if _, ok := p.Status.Retired["50"]; ok {
		t.Error("a clear bit has nothing to retire")
	}
	bm, _ := decodeBitmap(p.Status.BitMap, p.Spec.Size)
	if !bm.Test(2) {
		t.Error("a retired slot keeps its bit: it is not given out during the quarantine")
	}
	if _, err := PinExistingIndex(ctx, c, "vni", "ns", 100, 2, "new-owner"); err == nil {
		t.Error("a retired slot cannot be pinned")
	}
	held, err := HeldBits(ctx, c, "vni", "ns")
	if err != nil || len(held) != 0 {
		t.Errorf("leased and retired slots are not 'held': %v %v", held, err)
	}
}
