package pool

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
)

// Giving a slot back one by one writes the hot pool object once (twice with the label, a third time for the empty-pool collection)
// per slot, and the writers conflict with each other and with the allocators. A slot is instead RETIRED: marked in the pool's status
// with the lease it had and the time (the bit stays set, so the slot is not given out), many marks in one update; after the
// quarantine the sweep returns all the slots that passed it in one more update. The quarantine also keeps a number from meeting a
// new owner while an old flow or cache that still means the old one may be about (a VNI on a node).

// Retirement is one slot to retire. Lease is nil for a slot that never had a lease (a legacy reservation).
type Retirement struct {
	Index uint
	Lease *Lease
}

func poolName(prefix string, size, index uint) string {
	return fmt.Sprintf("%s-%d", prefix, index/size)
}

// Retire marks the slots retired at `now`, one update per pool. A lease that is no longer the slot's (released before, taken again
// by a new owner) is skipped: it is an old acknowledgement. It returns the retirements it could not make (a failed update of their
// pool), to be tried again.
func Retire(ctx context.Context, c client.Client, prefix, namespace string, size uint, rs []Retirement, now time.Time) (failed []Retirement, err error) {
	byPool := map[string][]Retirement{}
	for _, r := range rs {
		n := poolName(prefix, size, r.Index)
		byPool[n] = append(byPool[n], r)
	}
	names := make([]string, 0, len(byPool))
	for n := range byPool {
		names = append(names, n)
	}
	sort.Strings(names)
	var firstErr error
	for _, n := range names {
		if err := retirePool(ctx, c, namespace, n, byPool[n], now); err != nil {
			failed = append(failed, byPool[n]...)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return failed, firstErr
}

func retirePool(ctx context.Context, c client.Client, namespace, name string, rs []Retirement, now time.Time) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var p allocationv1alpha1.Pool
		if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &p); err != nil {
			return client.IgnoreNotFound(err)
		}
		bitmap, err := decodeBitmap(p.Status.BitMap, p.Spec.Size)
		if err != nil {
			return err
		}
		changed := false
		for _, r := range rs {
			if r.Index < p.Spec.Offset || r.Index-p.Spec.Offset >= p.Spec.Size {
				continue
			}
			bit := r.Index - p.Spec.Offset
			key := strconv.FormatUint(uint64(bit), 10)
			if _, done := p.Status.Retired[key]; done || !bitmap.Test(bit) {
				continue
			}
			lease, leased := p.Status.Leases[key]
			entry := allocationv1alpha1.PoolRetired{RetiredAt: metav1.NewTime(now)}
			switch {
			case r.Lease != nil:
				if !leased || string(p.UID) != r.Lease.PoolUID || lease.OwnerUID != r.Lease.OwnerUID || lease.Generation != r.Lease.Generation {
					continue
				}
				entry.OwnerUID, entry.Generation = lease.OwnerUID, lease.Generation
			case leased:
				continue // a leased slot is retired by its lease, not by its number
			}
			delete(p.Status.Leases, key)
			if p.Status.Retired == nil {
				p.Status.Retired = map[string]allocationv1alpha1.PoolRetired{}
			}
			p.Status.Retired[key] = entry
			changed = true
		}
		if !changed {
			return nil
		}
		return c.Status().Update(ctx, &p)
	})
}

// ReturnMatured gives back, in one update per pool, every retired slot whose quarantine has passed, and returns how many.
func ReturnMatured(ctx context.Context, c client.Client, prefix, namespace string, size uint, quarantine time.Duration, now time.Time) (int, error) {
	var pools allocationv1alpha1.PoolList
	if err := c.List(ctx, &pools, client.InNamespace(namespace), client.MatchingLabels{PoolGroupLabel: prefix}); err != nil {
		return 0, err
	}
	a := NewAllocator(c, prefix, namespace, size).(*allocator)
	total := 0
	var firstErr error
	for i := range pools.Items {
		if len(pools.Items[i].Status.Retired) == 0 {
			continue
		}
		var free uint
		returned := 0
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var p allocationv1alpha1.Pool
			if err := c.Get(ctx, client.ObjectKey{Name: pools.Items[i].Name, Namespace: namespace}, &p); err != nil {
				return client.IgnoreNotFound(err)
			}
			bitmap, err := decodeBitmap(p.Status.BitMap, p.Spec.Size)
			if err != nil {
				return err
			}
			returned = 0
			for key, r := range p.Status.Retired {
				if now.Sub(r.RetiredAt.Time) < quarantine {
					continue
				}
				bit, err := strconv.ParseUint(key, 10, 32)
				if err != nil {
					delete(p.Status.Retired, key)
					continue
				}
				if bitmap.Test(uint(bit)) {
					bitmap.Clear(uint(bit))
					p.Status.Free++
				}
				delete(p.Status.Retired, key)
				returned++
			}
			if returned == 0 {
				return nil
			}
			p.Status.BitMap = encodeBitmap(bitmap)
			free = p.Status.Free
			return c.Status().Update(ctx, &p)
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if returned > 0 {
			total += returned
			if err := a.syncStateLabel(ctx, pools.Items[i].Name, free); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	if total > 0 {
		if err := a.collectRedundantEmptyPools(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return total, firstErr
}

// HeldBits lists, per pool, the set bits that have neither a lease nor a retirement: the slots someone took without a lease
// (a legacy reservation) and the ones whose owner forgot them. Index = bit + the pool's offset.
func HeldBits(ctx context.Context, c client.Reader, prefix, namespace string) ([]uint, error) {
	var pools allocationv1alpha1.PoolList
	if err := c.List(ctx, &pools, client.InNamespace(namespace), client.MatchingLabels{PoolGroupLabel: prefix}); err != nil {
		return nil, err
	}
	var out []uint
	for i := range pools.Items {
		p := &pools.Items[i]
		bitmap, err := decodeBitmap(p.Status.BitMap, p.Spec.Size)
		if err != nil {
			return nil, err
		}
		for bit, ok := bitmap.NextSet(0); ok && bit < p.Spec.Size; bit, ok = bitmap.NextSet(bit + 1) {
			if bit == 0 && p.Spec.Offset == 0 {
				continue // reserved
			}
			key := strconv.FormatUint(uint64(bit), 10)
			if _, leased := p.Status.Leases[key]; leased {
				continue
			}
			if _, retired := p.Status.Retired[key]; retired {
				continue
			}
			out = append(out, bit+p.Spec.Offset)
		}
	}
	return out, nil
}

// SetBitsForTest sets bits of an encoded bitmap (and lowers free for each one that was clear); it is for tests that seed a pool.
func SetBitsForTest(encoded string, free uint, bits ...uint) (string, uint, error) {
	bm, err := decodeBitmap(encoded, 1<<20)
	if err != nil {
		return "", 0, err
	}
	for _, b := range bits {
		if !bm.Test(b) {
			bm.Set(b)
			free--
		}
	}
	return encodeBitmap(bm), free, nil
}
