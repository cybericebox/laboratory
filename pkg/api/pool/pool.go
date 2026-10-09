package pool

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"

	"github.com/bits-and-blooms/bitset"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
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
	PoolTypeVNI        = "vni"
)

type (
	allocator struct {
		client.Client
		poolNamePrefix string
		poolSize       uint
		namespace      string
		labelRequests  labelRequests
		// rotate hands out the lowest free index AFTER the one handed out last (wrapping around), not the lowest free one: a released index
		// is not given again until the whole pool has been walked, so something that still holds its old meaning (a flow, a cache) never
		// meets a new owner of the number at once.
		rotate   bool
		ownerUID string
		lease    Lease
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

// CursorAnnotation on a Pool remembers where a rotating allocator handed out last.
const CursorAnnotation = "allocation.cybericebox.com/cursor"

// NewRotatingAllocator is NewAllocator with indexes that are not reused until the pool has been walked (see allocator.rotate): the
// VNIs of the lab networks, where the same number meeting a new lab at once could inherit stale flows on a node.
func NewRotatingAllocator(c client.Client, poolNamePrefix, namespace string, poolSize uint) Allocator {
	a := NewAllocator(c, poolNamePrefix, namespace, poolSize).(*allocator)
	a.rotate = true
	return a
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
		sort.Slice(
			pools.Items, func(i, j int) bool {
				return pools.Items[i].Status.Free < pools.Items[j].Status.Free
			},
		)
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

	var start uint
	if a.rotate {
		if n, err := strconv.ParseUint(selected.Annotations[CursorAnnotation], 10, 32); err == nil && uint(n) < selected.Spec.Size {
			start = uint(n)
		}
	}
	bit, found := nextClearWithinPool(bitmap, start, selected.Spec.Size)
	if !found && start > 0 {
		bit, found = nextClearWithinPool(bitmap, 0, selected.Spec.Size)
	}
	if !found {
		return 0, fmt.Errorf("pool %s has no free slots despite state label", selected.Name)
	}
	bitmap.Set(bit)
	if a.ownerUID != "" {
		if selected.UID == "" || selected.Status.NextLeaseGeneration == int64(^uint64(0)>>1) {
			return 0, fmt.Errorf("pool lease identity/generation unavailable")
		}
		selected.Status.NextLeaseGeneration++
		if selected.Status.Leases == nil {
			selected.Status.Leases = map[string]allocationv1alpha1.PoolLease{}
		}
		selected.Status.Leases[strconv.FormatUint(uint64(bit), 10)] = allocationv1alpha1.PoolLease{OwnerUID: a.ownerUID, Generation: selected.Status.NextLeaseGeneration}
		a.lease = Lease{Index: bit + selected.Spec.Offset, PoolUID: string(selected.UID), OwnerUID: a.ownerUID, Generation: selected.Status.NextLeaseGeneration}
	}
	selected.Status.Free--
	selected.Status.BitMap = encodeBitmap(bitmap)

	if err = a.Status().Update(ctx, selected); err != nil {
		return 0, fmt.Errorf("update pool status: %w", err)
	}
	if a.rotate {
		// Best effort: a lost cursor only means the next index may be a lower one.
		if err := a.saveCursor(ctx, selected.Name, bit+1); err != nil {
			logf.FromContext(ctx).Error(err, "failed to save the allocation cursor", "pool", selected.Name)
		}
	}

	// Update state label on a fresh copy to avoid resourceVersion conflict.
	if err = a.syncStateLabel(ctx, selected.Name, selected.Status.Free); err != nil {
		// Label is best-effort; allocation already succeeded.
		logf.FromContext(ctx).Error(err, "failed to sync pool state label", "pool", selected.Name)
	}

	return bit + selected.Spec.Offset, nil
}

// Compact bitmaps omit the unused tail. Its absent bits are free, but the
// immutable pool size still bounds the search, including older oversized data.
func nextClearWithinPool(bitmap *bitset.BitSet, start, size uint) (uint, bool) {
	if start >= size {
		return 0, false
	}
	if bit, found := bitmap.NextClear(start); found && bit < size {
		return bit, true
	}
	if tail := max(start, bitmap.Len()); tail < size {
		return tail, true
	}
	return 0, false
}

func (a *allocator) saveCursor(ctx context.Context, poolName string, cursor uint) error {
	var pool allocationv1alpha1.Pool
	if err := a.Get(ctx, client.ObjectKey{Name: poolName, Namespace: a.namespace}, &pool); err != nil {
		return err
	}
	orig := pool.DeepCopy()
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	pool.Annotations[CursorAnnotation] = strconv.FormatUint(uint64(cursor), 10)
	return a.Patch(ctx, &pool, client.MergeFrom(orig))
}

func (a *allocator) ReleaseIndex(ctx context.Context, index uint) error {
	log := logf.FromContext(ctx)

	poolID := index / a.poolSize
	var pool allocationv1alpha1.Pool
	if err := a.Get(
		ctx, client.ObjectKey{
			Name:      fmt.Sprintf("%s-%d", a.poolNamePrefix, poolID),
			Namespace: a.namespace,
		}, &pool,
	); err != nil {
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

	if _, owned := pool.Status.Leases[strconv.FormatUint(uint64(bit), 10)]; owned {
		return fmt.Errorf("owned pool lease requires exact owner/generation release")
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

	// Lazy GC: keep at most one empty pool per group as a buffer (spec §11 —
	// "one empty pool as a buffer, to avoid thrashing"). The "latest" pool
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
		return client.IgnoreNotFound(err)
	}
	// Capacity accounts for the reserved bit-0 in the first pool (offset == 0).
	capacity := pool.Spec.Size
	if pool.Spec.Offset == 0 {
		capacity--
	}
	state := PoolStatePartial
	switch free {
	case 0:
		state = PoolStateFull
	case capacity:
		state = PoolStateEmpty
	}
	if pool.Labels[PoolStateLabel] == state {
		return nil
	}
	// A merge patch of the one label (no resourceVersion precondition): the pool's status is written by the same
	// callers at the same time, and a full Update here lost that race with a conflict.
	orig := pool.DeepCopy()
	if pool.Labels == nil {
		pool.Labels = map[string]string{}
	}
	pool.Labels[PoolStateLabel] = state
	return client.IgnoreNotFound(a.Patch(ctx, &pool, client.MergeFrom(orig)))
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
		sort.Slice(
			pools.Items, func(i, j int) bool {
				return pools.Items[i].Spec.Offset > pools.Items[j].Spec.Offset
			},
		)
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
	if err = a.List(
		ctx, &list, &client.ListOptions{
			Namespace:     a.namespace,
			LabelSelector: sel,
		},
	); err != nil {
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

// Lease is an immutable reservation epoch. Its UID survives caller/process
// restart; PoolUID also fences pool deletion/recreation with a reset counter.
type Lease struct {
	Index             uint
	PoolUID, OwnerUID string
	Generation        int64
}

func FindOwnerLease(ctx context.Context, c client.Reader, prefix, namespace, uid string) (*Lease, error) {
	var pools allocationv1alpha1.PoolList
	if err := c.List(ctx, &pools, client.InNamespace(namespace), client.MatchingLabels{PoolGroupLabel: prefix}); err != nil {
		return nil, err
	}
	var found *Lease
	for _, p := range pools.Items {
		for key, owner := range p.Status.Leases {
			if owner.OwnerUID != uid {
				continue
			}
			bit, err := strconv.ParseUint(key, 10, 32)
			if err != nil {
				return nil, err
			}
			if found != nil {
				return nil, fmt.Errorf("owner has conflicting pool leases")
			}
			found = &Lease{Index: uint(bit) + p.Spec.Offset, PoolUID: string(p.UID), OwnerUID: uid, Generation: owner.Generation}
		}
	}
	return found, nil
}
func AllocateOwnedIndex(ctx context.Context, c client.Client, prefix, namespace string, size uint, uid string) (*Lease, error) {
	if uid == "" {
		return nil, fmt.Errorf("actual lease owner UID unavailable")
	}
	if prior, err := FindOwnerLease(ctx, c, prefix, namespace, uid); err != nil || prior != nil {
		return prior, err
	}
	a := NewRotatingAllocator(c, prefix, namespace, size).(*allocator)
	a.ownerUID = uid
	if _, err := a.AllocateIndex(ctx); err != nil {
		return nil, err
	}
	return &a.lease, nil
}
func leasePool(ctx context.Context, c client.Reader, prefix, namespace string, size, index uint) (*allocationv1alpha1.Pool, uint, error) {
	var p allocationv1alpha1.Pool
	if err := c.Get(ctx, client.ObjectKey{Name: fmt.Sprintf("%s-%d", prefix, index/size), Namespace: namespace}, &p); err != nil {
		return nil, 0, err
	}
	if index < p.Spec.Offset || index-p.Spec.Offset >= p.Spec.Size {
		return nil, 0, fmt.Errorf("pool lease outside declared extent")
	}
	return &p, index - p.Spec.Offset, nil
}

// PinExistingIndex upgrades a currently held legacy reservation. It never binds
// a clear bit or a differently owned lease; all subsequent releases are fenced.
func PinExistingIndex(ctx context.Context, c client.Client, prefix, namespace string, size, index uint, uid string) (*Lease, error) {
	if uid == "" {
		return nil, fmt.Errorf("actual lease owner UID unavailable")
	}
	p, bit, err := leasePool(ctx, c, prefix, namespace, size, index)
	if err != nil {
		return nil, err
	}
	bitmap, err := decodeBitmap(p.Status.BitMap, p.Spec.Size)
	if err != nil {
		return nil, err
	}
	if !bitmap.Test(bit) || p.UID == "" {
		return nil, fmt.Errorf("pool reservation is absent or identity unavailable")
	}
	key := strconv.FormatUint(uint64(bit), 10)
	if prior, ok := p.Status.Leases[key]; ok {
		if prior.OwnerUID != uid {
			return nil, fmt.Errorf("pool slot belongs to another owner")
		}
		return &Lease{Index: index, PoolUID: string(p.UID), OwnerUID: uid, Generation: prior.Generation}, nil
	}
	if p.Status.NextLeaseGeneration == int64(^uint64(0)>>1) {
		return nil, fmt.Errorf("pool lease generation exhausted")
	}
	p.Status.NextLeaseGeneration++
	if p.Status.Leases == nil {
		p.Status.Leases = map[string]allocationv1alpha1.PoolLease{}
	}
	p.Status.Leases[key] = allocationv1alpha1.PoolLease{OwnerUID: uid, Generation: p.Status.NextLeaseGeneration}
	if err := c.Status().Update(ctx, p); err != nil {
		return nil, err
	}
	return &Lease{Index: index, PoolUID: string(p.UID), OwnerUID: uid, Generation: p.Status.NextLeaseGeneration}, nil
}
func ValidateLease(ctx context.Context, c client.Reader, prefix, namespace string, size uint, lease Lease) error {
	p, bit, err := leasePool(ctx, c, prefix, namespace, size, lease.Index)
	if err != nil {
		return err
	}
	owner, ok := p.Status.Leases[strconv.FormatUint(uint64(bit), 10)]
	if !ok || string(p.UID) != lease.PoolUID || owner.OwnerUID != lease.OwnerUID || owner.Generation != lease.Generation || lease.Generation < 1 {
		return fmt.Errorf("current pool lease owner/generation changed")
	}
	bitmap, err := decodeBitmap(p.Status.BitMap, p.Spec.Size)
	if err != nil {
		return err
	}
	if !bitmap.Test(bit) {
		return fmt.Errorf("current pool reservation absent")
	}
	return nil
}
func ReleaseOwnedIndex(ctx context.Context, c client.Client, prefix, namespace string, size uint, lease Lease) error {
	p, bit, err := leasePool(ctx, c, prefix, namespace, size, lease.Index)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	key := strconv.FormatUint(uint64(bit), 10)
	owner, ok := p.Status.Leases[key]
	// An old successful ACK is idempotent, including after a later reservation.
	if !ok || string(p.UID) != lease.PoolUID || owner.OwnerUID != lease.OwnerUID || owner.Generation != lease.Generation {
		return nil
	}
	if lease.OwnerUID == "" || lease.Generation < 1 {
		return fmt.Errorf("incomplete pool lease")
	}
	bitmap, err := decodeBitmap(p.Status.BitMap, p.Spec.Size)
	if err != nil {
		return err
	}
	if bitmap.Test(bit) {
		bitmap.Clear(bit)
		p.Status.Free++
	}
	delete(p.Status.Leases, key)
	p.Status.BitMap = encodeBitmap(bitmap)
	if err := c.Status().Update(ctx, p); err != nil {
		return err
	}
	a := NewRotatingAllocator(c, prefix, namespace, size).(*allocator)
	return a.syncStateLabel(ctx, p.Name, p.Status.Free)
}
