package nodeagent

import (
	"fmt"
	"sync"

	"github.com/cybericebox/laboratory/internal/bandwidth"
)

// The OVS meters that keep the Geneve traffic of the node within its class (see package bandwidth). A lab that is split between nodes
// (its frames are flooded to a remote VTEP) goes through two meters in a row: the lab's own, then the node's.
//
//	t6  (local origin, VNI)   deliver locally, goto 7
//	t7  metadata=VNI          meter(lab)  goto 8
//	t8  metadata=VNI          meter(node) then the tunnel outputs
//
// Rates change with mod-meter (MeterSet), never with a delete: deleting a meter deletes the flows that use it. A meter is made
// before the flow that names it.
const (
	nodeMeterID  = 1
	labMeterBase = 16 // lab meter id = base + VNI; VNIs are below 65000 and the kernel datapath has 200000 meters
	geneveTable1 = 7
	geneveTable2 = 8
	// minBurstKbit covers a 64 kB segment: the kernel meter counts a segmentation-offloaded frame whole, and a burst smaller than
	// the frame would drop every one of them.
	minBurstKbit = 2048
)

func labMeterID(vni uint) uint32 { return labMeterBase + uint32(vni) }

// meterBackend is what the meters need of the OpenFlow connection.
type meterBackend interface {
	MeterSet(id, kbps, burstKbit uint32) error
	MeterDelete(id uint32) error
}

func burstFor(kbps int64) uint32 { return uint32(max(kbps/10, minBurstKbit)) }

// geneveMeters keeps the rates of the lab meters and the node meter. All methods are safe for concurrent use.
type geneveMeters struct {
	mu     sync.Mutex
	be     meterBackend
	limits bandwidth.Limits
	vteps  map[uint]int   // split labs: the VNI and the number of remote nodes it floods to
	lab    map[uint]int64 // the rate each lab meter has now
	node   int64          // the rate the node meter has now (0 = not made)
}

func newGeneveMeters(be meterBackend) *geneveMeters {
	return &geneveMeters{be: be, vteps: map[uint]int{}, lab: map[uint]int64{}}
}

// Enabled says the Geneve class is set: split labs go through meters.
func (g *geneveMeters) Enabled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limits.GeneveKbps > 0
}

// SetLimits takes new limits; it returns whether the meters turned on or off (the flows of the split labs then have to be built
// again) and applies the new rates to the meters that exist.
func (g *geneveMeters) SetLimits(l bandwidth.Limits) (toggled bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	was := g.limits.GeneveKbps > 0
	g.limits = l
	is := l.GeneveKbps > 0
	if was != is {
		return true, nil
	}
	if !is {
		return false, nil
	}
	return false, g.refreshLocked()
}

// maxVTEPs is the most remote nodes any split lab floods to.
func (g *geneveMeters) maxVTEPsLocked() int {
	most := 1
	for _, v := range g.vteps {
		most = max(most, v)
	}
	return most
}

func (g *geneveMeters) wantLabLocked(vni uint) int64 {
	return bandwidth.LabGeneveRate(g.limits.GeneveKbps, g.limits.LabCapKbps, len(g.vteps), g.vteps[vni])
}

// refreshLocked brings the node meter and the meters of the split labs to the rates they should have; only a meter whose rate
// changed is touched. The first error is returned, the rest are still tried.
func (g *geneveMeters) refreshLocked() error {
	if g.limits.GeneveKbps <= 0 {
		return nil // off: nothing to set (Clear removes what is left)
	}
	var first error
	remember := func(err error) {
		if first == nil {
			first = err
		}
	}
	if want := bandwidth.NodeGeneveKbps(bandwidth.NodeLimits{GeneveKbps: g.limits.GeneveKbps}, g.maxVTEPsLocked()); want != g.node {
		if err := g.be.MeterSet(nodeMeterID, uint32(want), burstFor(want)); err != nil {
			g.node = 0
			remember(fmt.Errorf("node Geneve meter: %w", err))
		} else {
			g.node = want
		}
	}
	for vni := range g.vteps {
		want := g.wantLabLocked(vni)
		if g.lab[vni] == want {
			continue
		}
		if err := g.be.MeterSet(labMeterID(vni), uint32(want), burstFor(want)); err != nil {
			delete(g.lab, vni)
			remember(fmt.Errorf("the Geneve meter of VNI %d: %w", vni, err))
		} else {
			g.lab[vni] = want
		}
	}
	return first
}

// Split makes sure the lab has a meter, with the share its class gives it, before its flows are built. The other split labs get
// their new (smaller) share too.
func (g *geneveMeters) Split(vni uint, vteps int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limits.GeneveKbps <= 0 || vteps < 1 {
		return nil
	}
	g.vteps[vni] = vteps
	delete(g.lab, vni) // always set: the switch may have lost it (a restart of OVS)
	g.node = 0
	return g.refreshLocked()
}

// Unsplit removes the lab's meter after its flows are gone, and gives its share to the others.
func (g *geneveMeters) Unsplit(vni uint) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.vteps[vni]; !ok {
		return nil
	}
	delete(g.vteps, vni)
	delete(g.lab, vni)
	err := g.be.MeterDelete(labMeterID(vni))
	if rerr := g.refreshLocked(); err == nil {
		err = rerr
	}
	return err
}

// Clear removes every meter (the limits were switched off and the flows no longer use them).
func (g *geneveMeters) Clear() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	var first error
	for vni := range g.vteps {
		if err := g.be.MeterDelete(labMeterID(vni)); err != nil && first == nil {
			first = err
		}
	}
	if g.node != 0 {
		if err := g.be.MeterDelete(nodeMeterID); err != nil && first == nil {
			first = err
		}
	}
	g.vteps, g.lab, g.node = map[uint]int{}, map[uint]int64{}, 0
	return first
}

// Rates are the rates now, for the logs and the tests.
func (g *geneveMeters) Rates() (node int64, labs map[uint]int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	labs = make(map[uint]int64, len(g.lab))
	for k, v := range g.lab {
		labs[k] = v
	}
	return g.node, labs
}
