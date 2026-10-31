package pool

import (
	"context"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SlotState is what the pool says of an index, whoever the owner is.
type SlotState int

const (
	// SlotHeld: a lease record, or a set bit with no record (a legacy hold of an unknown owner). Someone may own the number.
	SlotHeld SlotState = iota
	// SlotRetired: its owner let it go and it waits out its quarantine; nobody owns it, and it is not yet issuable.
	SlotRetired
	// SlotFree: no lease, no retirement, a clear bit: back in the pool, issuable to anyone at any moment.
	SlotFree
)

// SlotStateOf reads the state of the index. The pool is read with the caller's reader (an uncached one for a decision that drops
// flows). An error is the failure of the question (the pool cannot be read, the index is outside every pool), never an answer.
func SlotStateOf(ctx context.Context, c client.Reader, prefix, namespace string, size, index uint) (SlotState, error) {
	p, bit, err := leasePool(ctx, c, prefix, namespace, size, index)
	if err != nil {
		return SlotHeld, err
	}
	key := strconv.FormatUint(uint64(bit), 10)
	if _, held := p.Status.Leases[key]; held {
		return SlotHeld, nil
	}
	if _, retired := p.Status.Retired[key]; retired {
		return SlotRetired, nil
	}
	bitmap, err := decodeBitmap(p.Status.BitMap, p.Spec.Size)
	if err != nil {
		return SlotHeld, err
	}
	if bitmap.Test(bit) {
		return SlotHeld, nil
	}
	return SlotFree, nil
}
