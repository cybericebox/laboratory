package laboratory

import (
	"context"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
)

// VNIRetirer collects the VNIs the lab controller is done with and retires them in the pool in batches (one update per pool per
// flush) instead of one pool write per deleted device or connection. What is queued and lost with the operator is not lost for good:
// the sweep (VNISweeper) retires every lease whose owner is gone and every held slot nobody references.
type VNIRetirer struct {
	Client client.Client
	// Interval is how often the queue is flushed (VNI_RETIRE_FLUSH).
	Interval time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	queue []poolpkg.Retirement
}

// NeedLeaderElection makes the flush run on the elected operator only.
func (q *VNIRetirer) NeedLeaderElection() bool { return true }

func (q *VNIRetirer) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// Add queues a retirement.
func (q *VNIRetirer) Add(r poolpkg.Retirement) {
	q.mu.Lock()
	q.queue = append(q.queue, r)
	q.mu.Unlock()
}

// Pending is how many retirements wait for the next flush.
func (q *VNIRetirer) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queue)
}

// Flush retires everything queued, one pool update per pool; what could not be retired stays queued.
func (q *VNIRetirer) Flush(ctx context.Context) error {
	q.mu.Lock()
	batch := q.queue
	q.queue = nil
	q.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	failed, err := poolpkg.Retire(ctx, q.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, batch, q.now())
	if len(failed) > 0 {
		q.mu.Lock()
		q.queue = append(failed, q.queue...)
		q.mu.Unlock()
	}
	return err
}

// Start flushes every Interval until ctx ends, and once more at the end.
func (q *VNIRetirer) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("vni-retire")
	interval := q.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := q.Flush(flush); err != nil {
				log.Error(err, "final flush of the VNI retirements")
			}
			return nil
		case <-t.C:
			if err := q.Flush(ctx); err != nil {
				log.Error(err, "retire VNIs")
			}
		}
	}
}

// retireLease gives a leased VNI back: queued when the controller has a retirer, else retired at once.
func (r *LabReconciler) retireLease(ctx context.Context, l poolpkg.Lease) error {
	return r.retire(ctx, poolpkg.Retirement{Index: l.Index, Lease: &l})
}

// retireIndex gives back a VNI that has no lease.
func (r *LabReconciler) retireIndex(ctx context.Context, index uint) error {
	return r.retire(ctx, poolpkg.Retirement{Index: index})
}

func (r *LabReconciler) retire(ctx context.Context, rt poolpkg.Retirement) error {
	if r.VNI != nil {
		r.VNI.Add(rt)
		return nil
	}
	_, err := poolpkg.Retire(ctx, r.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, []poolpkg.Retirement{rt}, time.Now())
	return err
}
