package pool

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"sort"

	"github.com/bits-and-blooms/bitset"
	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	PoolStateLabel  = "allocation.cybericebox.com/state"
	LatestPoolLabel = "allocation.cybericebox.com/latest"
	PoolGroupLabel  = "allocation.cybericebox.com/group"

	PoolStateEmpty   = "empty"
	PoolStatePartial = "partial"
	PoolStateFull    = "full"

	// PoolTypeLabel carries semantic pool type (vni, vpn-clients, lab-subnets).
	PoolTypeLabel = "pool.cybericebox.com/type"

	// Semantic pool type values.
	PoolTypeVPNClients = "vpn-clients"
	PoolTypeLabSubnets = "lab-subnets"
)

type (
	allocator struct {
		client.Client
		poolNamePrefix string
		poolSize       uint
		namespace      string
		labelRequests  labelRequests
	}

	labelRequests struct {
		NotFull *labels.Requirement
		Latest  *labels.Requirement
	}

	Allocator interface {
		AllocateIndex(ctx context.Context) (uint, error)
		ReleaseIndex(ctx context.Context, index uint) error
	}
)

// InitBitmap initialises a pool's bitmap and returns (encodedBitmap, freeCount).
// Bit 0 is reserved when offset == 0 to avoid assigning the network-address index.
func InitBitmap(size, offset uint) (string, uint) {
	bm := bitset.New(size).Compact()
	free := size
	if offset == 0 {
		bm.Set(0)
		free--
	}
	return encodeBitmap(bm), free
}

func NewAllocator(c client.Client, poolNamePrefix, namespace string, poolSize uint) Allocator {
	lr := labelRequests{}
	var err error
	lr.NotFull, err = labels.NewRequirement(PoolStateLabel, selection.NotEquals, []string{PoolStateFull})
	if err != nil {
		panic(fmt.Sprintf("build NotFull label requirement: %v", err))
	}
	lr.Latest, err = labels.NewRequirement(LatestPoolLabel, selection.Equals, []string{"true"})
	if err != nil {
		panic(fmt.Sprintf("build Latest label requirement: %v", err))
	}
	return &allocator{
		Client:         c,
		poolNamePrefix: poolNamePrefix,
		poolSize:       poolSize,
		namespace:      namespace,
		labelRequests:  lr,
	}
}

func (a *allocator) AllocateIndex(ctx context.Context) (uint, error) {
	pools, err := a.listPools(ctx, *a.labelRequests.NotFull)
	if err != nil {
		return 0, fmt.Errorf("list pools for allocation: %w", err)
	}

	var selected *allocationv1alpha1.Pool
	if len(pools.Items) > 0 {
		// Fill the most-occupied pool first (least free slots).
		sort.Slice(pools.Items, func(i, j int) bool {
			return pools.Items[i].Status.Free < pools.Items[j].Status.Free
		})
		selected = &pools.Items[0]
	} else {
		selected, err = a.createPool(ctx)
		if err != nil {
			return 0, fmt.Errorf("create new pool: %w", err)
		}
	}

	bitmap, err := decodeBitmap(selected.Status.BitMap, selected.Spec.Size)
	if err != nil {
		return 0, fmt.Errorf("decode bitmap for pool %s: %w", selected.Name, err)
	}

	bit, found := bitmap.NextClear(0)
	if !found {
		return 0, fmt.Errorf("pool %s has no free slots despite state label", selected.Name)
	}
	bitmap.Set(bit)
	selected.Status.Free--
	selected.Status.BitMap = encodeBitmap(bitmap)

	if err = a.Status().Update(ctx, selected); err != nil {
		return 0, fmt.Errorf("update pool status: %w", err)
	}

	// Update state label on a fresh copy to avoid resourceVersion conflict.
	if err = a.syncStateLabel(ctx, selected.Name, selected.Status.Free); err != nil {
		// Label is best-effort; allocation already succeeded.
		logf.FromContext(ctx).Error(err, "failed to sync pool state label", "pool", selected.Name)
	}

	return uint(bit) + selected.Spec.Offset, nil
}

func (a *allocator) ReleaseIndex(ctx context.Context, index uint) error {
	log := logf.FromContext(ctx)

	poolID := index / a.poolSize
	var pool allocationv1alpha1.Pool
	if err := a.Get(ctx, client.ObjectKey{
		Name:      fmt.Sprintf("%s-%d", a.poolNamePrefix, poolID),
		Namespace: a.namespace,
	}, &pool); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get pool for release index %d: %w", index, err)
	}

	if index < pool.Spec.Offset {
		return fmt.Errorf("index %d is below pool %s offset %d", index, pool.Name, pool.Spec.Offset)
	}
	bit := index - pool.Spec.Offset
	if bit >= pool.Spec.Size {
		return fmt.Errorf("index %d maps to bit %d outside pool %s size %d", index, bit, pool.Name, pool.Spec.Size)
	}

	bitmap, err := decodeBitmap(pool.Status.BitMap, pool.Spec.Size)
	if err != nil {
		return fmt.Errorf("decode bitmap for pool %s: %w", pool.Name, err)
	}

	if !bitmap.Test(bit) {
		log.Info("index already free, skipping release", "index", index, "pool", pool.Name)
		return nil
	}
	bitmap.Clear(bit)
	pool.Status.Free++
	pool.Status.BitMap = encodeBitmap(bitmap)

	if err = a.Status().Update(ctx, &pool); err != nil {
		return fmt.Errorf("update pool status on release: %w", err)
	}

	if err = a.syncStateLabel(ctx, pool.Name, pool.Status.Free); err != nil {
		logf.FromContext(ctx).Error(err, "failed to sync pool state label after release", "pool", pool.Name)
	}

	// Lazy GC: keep at most one empty pool per group as buffer (spec §11 —
	// "буфер из одного пустого пула, чтобы не дребезжать"). The "latest" pool
	// is preserved so newly-created allocations land contiguously; any other
	// empty pool is collected.
	if err = a.collectRedundantEmptyPools(ctx); err != nil {
		logf.FromContext(ctx).Error(err, "failed to collect redundant empty pools")
	}

	return nil
}

// collectRedundantEmptyPools deletes empty non-latest pools when an empty
// latest pool already exists. The latest pool serves as the single-empty
// buffer.
func (a *allocator) collectRedundantEmptyPools(ctx context.Context) error {
	emptyReq, err := labels.NewRequirement(PoolStateLabel, selection.Equals, []string{PoolStateEmpty})
	if err != nil {
		return err
	}
	pools, err := a.listPools(ctx, *emptyReq)
	if err != nil {
		return err
	}
	if len(pools.Items) < 2 {
		return nil
	}
	for i := range pools.Items {
		p := &pools.Items[i]
		if p.Labels[LatestPoolLabel] == "true" {
			continue
		}
		if err := a.Delete(ctx, p); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete empty pool %s: %w", p.Name, err)
		}
	}
	return nil
}

// syncStateLabel updates allocation.cybericebox.com/state on the pool after a bitmap change.
// Uses a fresh Get to avoid resourceVersion conflict with the Status().Update() call.
func (a *allocator) syncStateLabel(ctx context.Context, poolName string, free uint) error {
	var pool allocationv1alpha1.Pool
	if err := a.Get(ctx, client.ObjectKey{Name: poolName, Namespace: a.namespace}, &pool); err != nil {
		return err
	}
	// Capacity accounts for the reserved bit-0 in the first pool (offset == 0).
	capacity := pool.Spec.Size
	if pool.Spec.Offset == 0 {
		capacity--
	}
	state := PoolStatePartial
	if free == 0 {
		state = PoolStateFull
	} else if free == capacity {
		state = PoolStateEmpty
	}
	if pool.Labels == nil {
		pool.Labels = map[string]string{}
	}
	if pool.Labels[PoolStateLabel] == state {
		return nil
	}
	pool.Labels[PoolStateLabel] = state
	return a.Update(ctx, &pool)
}

func (a *allocator) createPool(ctx context.Context) (*allocationv1alpha1.Pool, error) {
	pools, err := a.listPools(ctx, *a.labelRequests.Latest)
	if err != nil {
		return nil, err
	}
	if len(pools.Items) == 0 {
		pools, err = a.listPools(ctx)
		if err != nil {
			return nil, err
		}
	}

	var offset uint
	if len(pools.Items) > 0 {
		sort.Slice(pools.Items, func(i, j int) bool {
			return pools.Items[i].Spec.Offset > pools.Items[j].Spec.Offset
		})
		latest := pools.Items[0]
		offset = latest.Spec.Offset + latest.Spec.Size

		latest.Labels[LatestPoolLabel] = "false"
		if err = a.Update(ctx, &latest); err != nil {
			return nil, fmt.Errorf("clear latest label from pool %s: %w", latest.Name, err)
		}
	}

	poolIndex := offset / a.poolSize
	bitmapStr, free := InitBitmap(a.poolSize, offset)

	newPool := &allocationv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", a.poolNamePrefix, poolIndex),
			Namespace: a.namespace,
			Labels: map[string]string{
				PoolStateLabel:  PoolStateEmpty,
				LatestPoolLabel: "true",
				PoolGroupLabel:  a.poolNamePrefix,
			},
		},
		Spec: allocationv1alpha1.PoolSpec{
			Size:   a.poolSize,
			Offset: offset,
		},
	}

	if err = a.Create(ctx, newPool); err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	// Status must be set via subresource update after the object exists.
	newPool.Status.Free = free
	newPool.Status.BitMap = bitmapStr
	if err = a.Status().Update(ctx, newPool); err != nil {
		return nil, fmt.Errorf("init pool status: %w", err)
	}

	return newPool, nil
}

func (a *allocator) listPools(ctx context.Context, reqs ...labels.Requirement) (*allocationv1alpha1.PoolList, error) {
	groupReq, err := labels.NewRequirement(PoolGroupLabel, selection.Equals, []string{a.poolNamePrefix})
	if err != nil {
		return nil, err
	}
	sel := labels.NewSelector().Add(append(reqs, *groupReq)...)

	var list allocationv1alpha1.PoolList
	if err = a.List(ctx, &list, &client.ListOptions{
		Namespace:     a.namespace,
		LabelSelector: sel,
	}); err != nil {
		return nil, fmt.Errorf("list pools: %w", err)
	}
	return &list, nil
}

func decodeBitmap(encoded string, size uint) (*bitset.BitSet, error) {
	bs := bitset.New(size).Compact()
	if encoded == "" {
		return bs, nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if _, err = bs.ReadFrom(bytes.NewReader(data)); err != nil {
		return nil, err
	}
	return bs, nil
}

func encodeBitmap(bs *bitset.BitSet) string {
	var buf bytes.Buffer
	if _, err := bs.WriteTo(&buf); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
