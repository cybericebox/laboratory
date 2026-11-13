package pool

import (
	"context"

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
)

// GrowPool raises the size of the pool name in the namespace to size and counts the new slots as free. A pool already that big (or
// bigger) keeps its size: a pool never shrinks, an index handed out stays handed out. The bitmap needs no change: the bits of the
// new slots are clear, and a bitmap shorter than the pool reads as clear beyond its end.
//
// The size is written first, the free count second (the status is a subresource), and the free count is the number of clear bits
// below the size, not "old plus added": a call that stops between the two writes is finished by the next call. An allocator that runs
// between them sees more slots and a free count that is too small, which only makes it run its insurance (reclaim) earlier.
func GrowPool(ctx context.Context, c client.Client, namespace, name string, size uint) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var p allocationv1alpha1.Pool
		if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &p); err != nil {
			return err
		}
		if p.Spec.Size < size {
			p.Spec.Size = size
			if err := c.Update(ctx, &p); err != nil {
				return err
			}
		}
		bitmap, err := decodeBitmap(p.Status.BitMap, p.Spec.Size)
		if err != nil {
			return err
		}
		taken := bitmap.Count()
		if taken > p.Spec.Size {
			taken = p.Spec.Size
		}
		if free := p.Spec.Size - taken; p.Status.Free < free {
			p.Status.Free = free
			return c.Status().Update(ctx, &p)
		}
		return nil
	})
}
