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

// VNISweeper is the one way a VNI comes back to the pool. Normal releases only retire the VNI (VNIRetirer: a mark in the pool's
// status, many in one update); the sweeper, every Interval,
//  1. retires the leases whose owner is gone (a release that never ran: the operator restarted at the wrong moment, an object
//     removed by hand or its finalizer bypassed) and the held slots nobody references (an allocation whose record was never
//     written), and
//  2. returns, in one update per pool, every VNI that has been retired for Quarantine.
//
// A lease is retired only when no Device and no Connection of the cluster has its owner UID (read from the API server, not the
// cache), no Lab still holds an unresolved native obligation on it (when runtime observation is on), and two sweeps in a row
// saw it so. A held slot without a lease is retired only when no Device or Connection has it as its VNI, in two sweeps in a row
// at least Grace apart. The quarantine keeps the number from a new owner while a flow of the old one may still be on a node.
type VNISweeper struct {
	Client client.Client
	// Reader reads straight from the API server; nil means Client.
	Reader client.Reader
	// Interval is the time between sweeps (VNI_SWEEP_INTERVAL), Quarantine how long a retired VNI waits (VNI_QUARANTINE),
	// Grace how long an orphan must have been seen before it is retired.
	Interval, Quarantine, Grace time.Duration
	// RuntimeObservation: native obligations of Labs hold leases back. With the observation off nobody resolves them.
	RuntimeObservation bool
	// Retirer, when set, is flushed first so that what the controller queued is in the pool before the sweep looks.
	Retirer *VNIRetirer
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	firstSeen map[string]time.Time
}

// NeedLeaderElection makes the sweeper run on the elected operator only.
func (c *VNISweeper) NeedLeaderElection() bool { return true }

// Start runs the sweep until ctx ends.
func (c *VNISweeper) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("vni-sweeper")
	interval := c.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if retired, returned, err := c.Sweep(ctx); err != nil {
			log.Error(err, "vni sweep")
		} else if retired+returned > 0 {
			log.Info("VNI sweep", "retiredOrphans", retired, "returned", returned)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (c *VNISweeper) reader() client.Reader {
	if c.Reader != nil {
		return c.Reader
	}
	return c.Client
}

func (c *VNISweeper) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Sweep does one pass; it returns how many orphans it retired and how many VNIs it gave back to the pool.
func (c *VNISweeper) Sweep(ctx context.Context) (retired, returned int, err error) {
	if c.Retirer != nil {
		if err := c.Retirer.Flush(ctx); err != nil {
			return 0, 0, err
		}
	}
	now := c.now()
	grace := c.Grace
	if grace <= 0 {
		grace = 5 * time.Minute
	}
	quarantine := c.Quarantine
	if quarantine <= 0 {
		quarantine = 10 * time.Minute
	}
	if c.firstSeen == nil {
		c.firstSeen = map[string]time.Time{}
	}
	var pools allocationv1alpha1.PoolList
	if err := c.reader().List(ctx, &pools, client.InNamespace(names.SystemNamespace), client.MatchingLabels{poolpkg.PoolGroupLabel: names.VNIPoolPrefix}); err != nil {
		return 0, 0, err
	}
	held, err := poolpkg.HeldBits(ctx, c.reader(), names.VNIPoolPrefix, names.SystemNamespace)
	if err != nil {
		return 0, 0, err
	}
	leases := false
	for i := range pools.Items {
		leases = leases || len(pools.Items[i].Status.Leases) > 0
	}
	seen := map[string]bool{}
	var orphans []poolpkg.Retirement
	if leases || len(held) > 0 {
		owners, referenced, err := c.liveOwners(ctx)
		if err != nil {
			return 0, 0, err
		}
		for i := range pools.Items {
			p := &pools.Items[i]
			for bit, lease := range p.Status.Leases {
				if owners[lease.OwnerUID] {
					continue
				}
				key := p.Name + "/" + bit + "/" + lease.OwnerUID + "/" + strconv.FormatInt(lease.Generation, 10)
				seen[key] = true
				first, ok := c.firstSeen[key]
				if !ok {
					c.firstSeen[key] = now
					continue
				}
				if now.Sub(first) < grace {
					continue
				}
				n, err := strconv.ParseUint(bit, 10, 32)
				if err != nil {
					continue
				}
				l := poolpkg.Lease{Index: uint(n) + p.Spec.Offset, PoolUID: string(p.UID), OwnerUID: lease.OwnerUID, Generation: lease.Generation}
				orphans = append(orphans, poolpkg.Retirement{Index: l.Index, Lease: &l})
				delete(c.firstSeen, key)
			}
		}
		for _, index := range held {
			if referenced[index] {
				continue
			}
			key := "held/" + strconv.FormatUint(uint64(index), 10)
			seen[key] = true
			first, ok := c.firstSeen[key]
			if !ok {
				c.firstSeen[key] = now
				continue
			}
			if now.Sub(first) < grace {
				continue
			}
			orphans = append(orphans, poolpkg.Retirement{Index: index})
			delete(c.firstSeen, key)
		}
	}
	for key := range c.firstSeen {
		if !seen[key] {
			delete(c.firstSeen, key)
		}
	}
	if len(orphans) > 0 {
		failed, err := poolpkg.Retire(ctx, c.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, orphans, now)
		retired = len(orphans) - len(failed)
		if err != nil {
			return retired, 0, err
		}
	}
	hold, err := c.unacknowledgedReleases(ctx)
	if err != nil {
		return retired, 0, err
	}
	returned, err = poolpkg.ReturnMaturedUnless(ctx, c.Client, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, quarantine, now, hold)
	return retired, returned, err
}

// unacknowledgedReleases says, for a retired lease, whether a Lab still records a VNI binding of it that no node has acknowledged as
// released (the node has not yet dropped the flows of the VNI). Such a slot stays retired past its quarantine: the number must not go to
// another lab while a node may carry the old owner's flows. Without runtime observation nobody acknowledges, so nothing is held.
func (c *VNISweeper) unacknowledgedReleases(ctx context.Context) (func(uint, string, int64) bool, error) {
	if !c.RuntimeObservation {
		return nil, nil
	}
	var labs laboratoryv1alpha1.LabList
	if err := c.reader().List(ctx, &labs); err != nil {
		return nil, err
	}
	type slot struct {
		index      uint
		owner      string
		generation int64
	}
	pending := map[slot]bool{}
	for i := range labs.Items {
		parent := &labs.Items[i]
		for _, scope := range parent.Status.ScopeInventory {
			for _, binding := range scope.VNIBindings {
				if !laboratoryv1alpha1.VNILeaseReleased(parent, binding) {
					pending[slot{binding.VNI, binding.UID, binding.LeaseGeneration}] = true
				}
			}
		}
	}
	return func(index uint, owner string, generation int64) bool { return pending[slot{index, owner, generation}] }, nil
}

// liveOwners is the set of UIDs that may still own a VNI lease (every Device and Connection, and the owner of every native
// obligation a Lab still has when runtime observation is on) and the set of VNIs the Devices and Connections record.
func (c *VNISweeper) liveOwners(ctx context.Context) (owners map[string]bool, referenced map[uint]bool, err error) {
	owners, referenced = map[string]bool{}, map[uint]bool{}
	var devices laboratoryv1alpha1.DeviceList
	if err := c.reader().List(ctx, &devices); err != nil {
		return nil, nil, err
	}
	for i := range devices.Items {
		owners[string(devices.Items[i].UID)] = true
		if v := devices.Items[i].Status.VNI; v != nil {
			referenced[*v] = true
		}
	}
	var connections laboratoryv1alpha1.ConnectionList
	if err := c.reader().List(ctx, &connections); err != nil {
		return nil, nil, err
	}
	for i := range connections.Items {
		owners[string(connections.Items[i].UID)] = true
		if v := connections.Items[i].Status.VNI; v != nil {
			referenced[*v] = true
		}
	}
	if !c.RuntimeObservation {
		return owners, referenced, nil
	}
	var labs laboratoryv1alpha1.LabList
	if err := c.reader().List(ctx, &labs); err != nil {
		return nil, nil, err
	}
	for i := range labs.Items {
		parent := &labs.Items[i]
		for _, scope := range parent.Status.ScopeInventory {
			for _, binding := range scope.VNIBindings {
				if !laboratoryv1alpha1.VNILeaseReleased(parent, binding) {
					owners[binding.UID] = true
					referenced[binding.VNI] = true
				}
			}
		}
	}
	return owners, referenced, nil
}
