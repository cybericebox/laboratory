package pool

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bits-and-blooms/bitset"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
)

// A ForwardAllocator gives out the indexes of a small pool that lives and dies with its group (the subnets of the labs and the tunnel
// addresses of the clients of a group: 254 values in the group's namespace) only forward: it never takes an index back. A deleted lab
// or client returns nothing, the pool goes with the group, and every allocation is ONE write of the pool (the bitmap, the free count
// and the cursor in the same status update).
//
// Insurance for a group that outlives its indexes: when fewer than two indexes are free, once, the indexes that nothing refers to any
// more are taken back (InUse tells which are referred to, read from the API server) and the allocation goes on round the pool. That
// code does not run before the pool has come to its end. It never takes back an index handed out in the last Recent (the object
// that records it may not be written yet), and not in the first Recent of the process.
type ForwardAllocator struct {
	Client client.Client
	// Prefix and Namespace name the pool "<Prefix>-0" in the group's namespace; Size is the number of values.
	Prefix, Namespace string
	Size              uint
	// InUse returns the indexes something refers to (read straight from the API server).
	InUse func(ctx context.Context) (map[uint]bool, error)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Recent is how long an index handed out is safe from being taken back.
const Recent = 2 * time.Minute

var (
	forwardStart  = time.Now()
	forwardMu     sync.Mutex
	forwardRecent = map[string]time.Time{} // "<namespace>/<prefix>/<index>" -> handed out at
)

func (a *ForwardAllocator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *ForwardAllocator) poolName() string { return a.Prefix + "-0" }

func (a *ForwardAllocator) recentKey(index uint) string {
	return fmt.Sprintf("%s/%s/%d", a.Namespace, a.Prefix, index)
}

func (a *ForwardAllocator) get(ctx context.Context) (*allocationv1alpha1.Pool, error) {
	var p allocationv1alpha1.Pool
	err := a.Client.Get(ctx, client.ObjectKey{Name: a.poolName(), Namespace: a.Namespace}, &p)
	if err == nil {
		return &p, nil
	}
	if !errors.IsNotFound(err) {
		return nil, err
	}
	bitmap, free := InitBitmap(a.Size, 0)
	p = allocationv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{Name: a.poolName(), Namespace: a.Namespace, Labels: map[string]string{
			PoolGroupLabel: a.Prefix, PoolStateLabel: PoolStatePartial, LatestPoolLabel: "true"}},
		Spec: allocationv1alpha1.PoolSpec{Size: a.Size},
	}
	if err := a.Client.Create(ctx, &p); err != nil && !errors.IsAlreadyExists(err) {
		return nil, err
	}
	if err := a.Client.Get(ctx, client.ObjectKey{Name: a.poolName(), Namespace: a.Namespace}, &p); err != nil {
		return nil, err
	}
	if p.Status.BitMap == "" {
		p.Status.BitMap, p.Status.Free = bitmap, free
		if err := a.Client.Status().Update(ctx, &p); err != nil {
			return nil, err
		}
	}
	return &p, nil
}

// AllocateIndex hands out the next index with one write of the pool.
func (a *ForwardAllocator) AllocateIndex(ctx context.Context) (uint, error) {
	forwardMu.Lock()
	defer forwardMu.Unlock()
	var out uint
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		p, err := a.get(ctx)
		if err != nil {
			return err
		}
		bitmap, err := decodeBitmap(p.Status.BitMap, p.Spec.Size)
		if err != nil {
			return err
		}
		if p.Status.Free < 2 {
			if err := a.reclaim(ctx, p, bitmap); err != nil {
				return err
			}
		}
		if p.Status.Free == 0 {
			return fmt.Errorf("pool %s/%s is full: every index is in use", a.Namespace, a.poolName())
		}
		start := p.Status.Cursor
		if start >= p.Spec.Size {
			start = 0
		}
		bit, found := nextClearWithinPool(bitmap, start, p.Spec.Size)
		if !found {
			bit, found = nextClearWithinPool(bitmap, 0, p.Spec.Size)
		}
		if !found {
			return fmt.Errorf("pool %s/%s has no free index despite its count", a.Namespace, a.poolName())
		}
		bitmap.Set(bit)
		p.Status.Free--
		p.Status.Cursor = bit + 1
		p.Status.BitMap = encodeBitmap(bitmap)
		if err := a.Client.Status().Update(ctx, p); err != nil {
			return err
		}
		out = bit
		return nil
	})
	if err != nil {
		return 0, err
	}
	forwardRecent[a.recentKey(out)] = a.now()
	return out, nil
}

// reclaim takes back, in the pool object p (the caller writes it), the indexes nothing refers to. forwardMu is held by the caller.
func (a *ForwardAllocator) reclaim(ctx context.Context, p *allocationv1alpha1.Pool, bitmap *bitset.BitSet) error {
	if a.InUse == nil || a.now().Sub(forwardStart) < Recent {
		return nil
	}
	used, err := a.InUse(ctx)
	if err != nil {
		return err
	}
	now := a.now()
	freed := uint(0)
	for bit := uint(1); bit < p.Spec.Size; bit++ {
		if !bitmap.Test(bit) || used[bit] {
			continue
		}
		if at, ok := forwardRecent[a.recentKey(bit)]; ok && now.Sub(at) < Recent {
			continue
		}
		bitmap.Clear(bit)
		delete(forwardRecent, a.recentKey(bit))
		freed++
	}
	p.Status.Free += freed
	return nil
}
