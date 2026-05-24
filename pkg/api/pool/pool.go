package api

import (
	"k8s.io/apimachinery/pkg/labels"
	"fmt"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sort"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"github.com/bits-and-blooms/bitset"
	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"encoding/base64"
	"bytes"
	"k8s.io/apimachinery/pkg/selection"
	"context"
	"k8s.io/apimachinery/pkg/api/errors"
)

const (
	// PoolStateLabel is the label used to mark a Pool as available for allocation
	PoolStateLabel = "allocation.cybericebox.com/state"
	// LatestPoolLabel is the label used to mark the latest Pool created
	LatestPoolLabel = "allocation.cybericebox.com/latest"
	// PoolGroupLabel is the label used to group Pools together
	PoolGroupLabel = "allocation.cybericebox.com/group"
	
	PoolStateEmpty   = "empty"
	PoolStatePartial = "partial"
	PoolStateFull    = "full"
)

type (
	allocator struct {
	client.Client
	
	poolNamePrefix string
	poolSize       uint
	namespace      string
	
	labelRequests labelRequests
}
	
	labelRequests struct {
		// Label for selecting Pools that are not full
		NotFull *labels.Requirement
		// Label for selecting the latest Pool
		Latest *labels.Requirement
		
	}
	
	Allocator interface {
		// AllocateIndex allocates a new index from the Pool
		AllocateIndex(ctx context.Context) (uint, error)
		// ReleaseIndex releases an index back to the Pool
		ReleaseIndex(ctx context.Context, index uint) error
	}
)



func NewAllocator(c client.Client, poolNamePrefix string, poolSize uint) Allocator {
	lr := labelRequests{}
	var err error
	lr.NotFull, err = labels.NewRequirement(PoolStateLabel, selection.NotEquals, []string{PoolStateFull})
	if err != nil {
		logf.Log.Error(err, "Failed to create label requirement for PoolStateLabel")
		return nil
	}
	
	lr.Latest, err = labels.NewRequirement(LatestPoolLabel, selection.Equals, []string{"true"})
	if err != nil {
		logf.Log.Error(err, "Failed to create label requirement for LatestPoolLabel")
		return nil
	}
	
	return &allocator{
		Client:         c,
		poolNamePrefix: poolNamePrefix,
		poolSize:       poolSize,
		labelRequests:  lr,
	}
}

func (a *allocator) AllocateIndex(ctx context.Context) (uint, error) {
	// Find available Pools for Connection ID allocation
	
	pools, err := a.listPools(ctx, *a.labelRequests.NotFull)
	if err != nil {
		return 0, fmt.Errorf("failed to list Pools for Connection ID allocation: %w", err)
	}
	
	selectedPool := &allocationv1alpha1.Pool{}
	
	if len(pools.Items) > 0 {
		// sort pools by availability
		sort.Slice(pools.Items, func(i, j int) bool {
			return pools.Items[i].Spec.Available < pools.Items[j].Spec.Available
		})
		
		selectedPool = &pools.Items[0] // Choose the first pool with minimum available ids
		
	} else {
		selectedPool, err = a.createConnectionIDPool(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to create new Pool for allocation: %w", err)
		}
	}
	
	// Decode bitmap
	bitmap, err := decodedBitmap(selectedPool.Spec.BitMap, selectedPool.Spec.Size)
	if err != nil {
		return 0, err
	}
	
	allocated, found := bitmap.NextClear(0) // Find the first available ID
	
	if found {
		bitmap.Set(allocated) // Mark it as allocated
		
		// Update the Pool status
		selectedPool.Spec.Available = selectedPool.Spec.Available - 1 // Decrease the available count
		selectedPool.Spec.BitMap = encodedBitmap(bitmap)
		
		// If the pool is not available anymore, mark it as not available
		if selectedPool.Spec.Available == 0 {
			selectedPool.Labels[PoolStateLabel] = PoolStateFull // Mark the pool as not available
			
		}
		
		// If the pool is not available anymore, mark it as not available
		if bitmap.Any() && selectedPool.Labels[PoolStateLabel] != PoolStateFull {
			selectedPool.Labels[PoolStateLabel] = PoolStatePartial // Mark the pool as not available
		}
		
		if err = a.Update(ctx, selectedPool); err != nil {
			return 0, fmt.Errorf("failed to update Pool availability: %w", err)
		}
		
		return allocated +selectedPool.Spec.Offset, nil
	}
	
	return 0, fmt.Errorf("no available allocation IDs in Pool %s", selectedPool.Name)
	
}

func (a *allocator) ReleaseIndex(ctx context.Context, index uint) error {
	log := logf.FromContext(ctx)
	
	
	poolID := index / a.poolSize // Calculate the Pool ID based on the Connection ID
	
	// Get the Pool that contains this Connection ID
	var pool allocationv1alpha1.Pool
	if err := a.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("%s%d", a.poolNamePrefix, poolID),
		Namespace: a.namespace,
	}, &pool); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get Pool release index %d: %w", index, err)
	}
	
	// Decode bitmap
	bitmap, err := decodedBitmap(pool.Spec.BitMap, pool.Spec.Size)
	if err != nil {
		return fmt.Errorf("failed to decode bitmap for Pool %s: %w", pool.Name, err)
	}
	
	// Mark the Index as free
	id := index - pool.Spec.Offset
	if id < 0 || id >= pool.Spec.Size {
		return fmt.Errorf("invalid index %d for Pool %s with size %d", index, pool.Name, pool.Spec.Size)
	}
	
	// Check if the ID is already free
	if !bitmap.Test(id) {
		log.Info("Index already free", "index", index, "pool", pool.Name)
		return nil
	}
	
	bitmap.Clear(id) // Mark the ID as free
	
	pool.Spec.Available = pool.Spec.Available + 1 // Increase the available count
	pool.Spec.BitMap = encodedBitmap(bitmap)
	
	if bitmap.None() && pool.Labels[PoolStateLabel] != PoolStateEmpty {
		// If the bitmap is empty, mark the pool as empty
		pool.Labels[PoolStateLabel] = PoolStateEmpty // Mark the pool as empty
	}
	
	if pool.Spec.Available > 0 && pool.Labels[PoolStateLabel] != PoolStatePartial {
		// If the bitmap is not empty, mark the pool as partial
		pool.Labels[PoolStateLabel] = PoolStatePartial // Mark the pool as partial
	}
	
	if err = a.Update(ctx, &pool); err != nil {
		return fmt.Errorf("failed to update Pool availability: %w", err)
	}
	
	return nil
}

func (a *allocator) createConnectionIDPool(ctx context.Context) (*allocationv1alpha1.Pool, error) {
	// Find the latest Pool
	
	pools, err := a.listPools(ctx, *a.labelRequests.Latest)
	if err != nil {
		return nil, fmt.Errorf("failed to list Pools by label %s: %w", LatestPoolLabel, err)
	}
	
	// If no pools are found with the LatestPoolLabel, list all pools to find the latest one
	if len(pools.Items) == 0 {
		pools, err = a.listPools(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list Pools by label %s: %w", LatestPoolLabel, err)
		}
	}
	
	var newPool *allocationv1alpha1.Pool
	
	if len(pools.Items) == 0 {
		// If no pools are found, create the first Pool
		newPool, err = a.createNewPool(ctx, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to create initial Pool: %w", err)
		}
		logf.FromContext(ctx).Info("Created initial Pool", "pool", newPool.Name)
		return newPool, nil
	} else {
		if len(pools.Items) > 1 {
			// If there are multiple pools, sort them by Offset to find the latest one (latest is first in the sorted list)
			sort.Slice(pools.Items, func(i, j int) bool {
				return pools.Items[i].Spec.Offset > pools.Items[j].Spec.Offset
			})
		}
		
		latestPool := pools.Items[0]
		// Create a new Pool based on the latest one
		newOffset := latestPool.Spec.Offset + latestPool.Spec.Size
		
		newPool, err = a.createNewPool(ctx, newOffset)
		if err != nil {
			return nil, fmt.Errorf("failed to create new Pool based on latest Pool: %w", err)
		}
		
		// Remove the latest label from the old pool
		latestPool.Labels[LatestPoolLabel] = "false"
		if err = a.Update(ctx, &latestPool); err != nil {
			return nil, fmt.Errorf("failed to update latest pool: %w", err)
		}
		
		logf.FromContext(ctx).Info("Created new Pool based on latest Pool", "newPool", newPool.Name, "latestPool", latestPool.Name)
		return newPool, nil
	}
}

func (a *allocator) createNewPool(ctx context.Context, offset uint) (*allocationv1alpha1.Pool, error) {
	newBitMap := bitset.New(a.poolSize).Compact()
	available := a.poolSize
	
	if offset == 0 {
		available--
		newBitMap.Set(0) // Reserve the first ID (0) for the first Pool
	}
	
	newPool := &allocationv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s%d", a.poolNamePrefix, offset/a.poolSize),
			Labels: map[string]string{
				PoolStateLabel:  PoolStateEmpty,
				LatestPoolLabel: "true",
				PoolGroupLabel:  a.poolNamePrefix,
			},
			Namespace: a.namespace,
		},
		Spec: allocationv1alpha1.PoolSpec{
			Size:      a.poolSize,
			Offset:    offset,
			Available: available,
			BitMap:    encodedBitmap(newBitMap),
		},
	}
	
	if err := a.Create(ctx, newPool); err != nil {
		return nil, fmt.Errorf("failed to create new Pool: %w", err)
	}
	
	return newPool, nil
}

func (a *allocator) listPools(ctx context.Context, labelReqs ...labels.Requirement) (*allocationv1alpha1.PoolList, error) {
	var pools allocationv1alpha1.PoolList
	
	// List all pools in the same namespace that have the specified label
	ops := &client.ListOptions{
		Namespace: a.namespace,
	}
	
	prefixReq, err := labels.NewRequirement(PoolGroupLabel, selection.Equals, []string{a.poolNamePrefix})
	if err != nil {
		return nil, fmt.Errorf("failed to create label requirement for pool name prefix: %w", err)
	}
	
	labelReqs = append(labelReqs, *prefixReq)
	
	ops.LabelSelector = labels.NewSelector().Add(labelReqs...)
	
	if err = a.List(ctx, &pools, ops); err != nil {
		return nil, fmt.Errorf("failed to list Pools with label %s: %w", labelReqs, err)
	}
	
	return &pools, nil
}

// Helpers

func decodedBitmap(encoded string, size uint) (*bitset.BitSet, error) {
	bs := bitset.New(size).Compact()
	data := make([]byte, 0)
	if encoded != "" {
		var err error
		data, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, err
		}
		_, err = bs.ReadFrom(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
	}
	return bs, nil
}

func encodedBitmap(bs *bitset.BitSet) string {
	var buf bytes.Buffer
	_, err := bs.WriteTo(&buf)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
