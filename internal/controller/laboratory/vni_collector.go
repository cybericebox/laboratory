package laboratory

import (
	"context"
	"strconv"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
)

// VNICollector releases the VNI leases whose owner is gone. A lease belongs to a Device or a Connection (by UID); when that
// object is deleted the lab controller releases the lease, but a lease can outlive it (a release that never ran: the operator was
// restarted at the wrong moment, runtime observation was switched off after the lab was gone, a lab removed by hand). 223 VNIs
// of a 65000 pool stayed leased that way. The collector sweeps the VNI pools every Interval and releases a lease only when
//   - no Device and no Connection of the cluster has its owner UID (read from the API server, not the cache), and
//   - no Lab still holds an unresolved native obligation on it (the lease is then the lab's to release), and
//   - it was seen like that by two sweeps at least Grace apart (an owner created a moment ago is never mistaken for a gone one).
//
// The release is the fenced ReleaseOwnedIndex (pool UID, owner, lease generation), so a lease taken again meanwhile is untouched.
// Bits set in a pool without a lease entry (legacy reservations) have no owner to check and are only counted.
type VNICollector struct {
	Client client.Client
	// Reader reads straight from the API server; nil means Client.
	Reader   client.Reader
	Interval time.Duration
	Grace    time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	firstSeen map[string]time.Time
}

// NeedLeaderElection makes the collector run on the elected operator only.
func (c *VNICollector) NeedLeaderElection() bool { return true }

// Start runs the sweep until ctx ends.
func (c *VNICollector) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("vni-collector")
	interval := c.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if released, err := c.Sweep(ctx); err != nil {
			log.Error(err, "vni sweep")
		} else if released > 0 {
			log.Info("released VNI leases of owners that are gone", "count", released)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (c *VNICollector) reader() client.Reader {
	if c.Reader != nil {
		return c.Reader
	}
	return c.Client
}

func leaseKey(pool string, bit string, l allocationv1alpha1.PoolLease) string {
	return pool + "/" + bit + "/" + l.OwnerUID + "/" + strconv.FormatInt(l.Generation, 10)
}

// Sweep does one pass and returns how many leases it released.
func (c *VNICollector) Sweep(ctx context.Context) (int, error) {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	grace := c.Grace
	if grace <= 0 {
		grace = 5 * time.Minute
	}
	if c.firstSeen == nil {
		c.firstSeen = map[string]time.Time{}
	}
	var pools allocationv1alpha1.PoolList
	if err := c.reader().List(ctx, &pools, client.InNamespace(names.SystemNamespace), client.MatchingLabels{poolpkg.PoolGroupLabel: names.VNIPoolPrefix}); err != nil {
		return 0, err
	}
	candidates := false
	for i := range pools.Items {
		candidates = candidates || len(pools.Items[i].Status.Leases) > 0
	}
	if !candidates {
		c.firstSeen = map[string]time.Time{}
		return 0, nil
	}
	owners, err := c.liveOwners(ctx)
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	released := 0
	for i := range pools.Items {
		p := &pools.Items[i]
		for bit, lease := range p.Status.Leases {
			if owners[lease.OwnerUID] {
				continue
			}
			key := leaseKey(p.Name, bit, lease)
			seen[key] = true
			first, ok := c.firstSeen[key]
			if !ok {
				c.firstSeen[key] = now
				continue
			}
			if now.Sub(first) < grace {
				continue
			}
			index, err := strconv.ParseUint(bit, 10, 32)
			if err != nil {
				continue
			}
			err = poolpkg.ReleaseOwnedIndex(ctx, c.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize,
				poolpkg.Lease{Index: uint(index) + p.Spec.Offset, PoolUID: string(p.UID), OwnerUID: lease.OwnerUID, Generation: lease.Generation})
			if err != nil {
				return released, err
			}
			delete(c.firstSeen, key)
			released++
		}
	}
	for key := range c.firstSeen {
		if !seen[key] {
			delete(c.firstSeen, key)
		}
	}
	return released, nil
}

// liveOwners is the set of UIDs that may still own a VNI lease: every Device and Connection, and every owner a Lab still has
// an unresolved native obligation for.
func (c *VNICollector) liveOwners(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	var devices laboratoryv1alpha1.DeviceList
	if err := c.reader().List(ctx, &devices); err != nil {
		return nil, err
	}
	for i := range devices.Items {
		out[string(devices.Items[i].UID)] = true
	}
	var connections laboratoryv1alpha1.ConnectionList
	if err := c.reader().List(ctx, &connections); err != nil {
		return nil, err
	}
	for i := range connections.Items {
		out[string(connections.Items[i].UID)] = true
	}
	var labs laboratoryv1alpha1.LabList
	if err := c.reader().List(ctx, &labs); err != nil {
		return nil, err
	}
	for i := range labs.Items {
		parent := &labs.Items[i]
		for _, scope := range parent.Status.ScopeInventory {
			for _, binding := range scope.VNIBindings {
				if !laboratoryv1alpha1.VNILeaseReleased(parent, binding) {
					out[binding.UID] = true
				}
			}
		}
	}
	return out, nil
}
