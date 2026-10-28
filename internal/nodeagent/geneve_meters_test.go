package nodeagent

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cybericebox/laboratory/internal/bandwidth"
)

type fakeMeters struct {
	rates   map[uint32]uint32
	calls   []string
	failSet map[uint32]bool
}

func newFakeMeters() *fakeMeters {
	return &fakeMeters{rates: map[uint32]uint32{}, failSet: map[uint32]bool{}}
}

func (f *fakeMeters) MeterSet(id, kbps, _ uint32) error {
	f.calls = append(f.calls, fmt.Sprintf("set %d %d", id, kbps))
	if f.failSet[id] {
		return errors.New("refused")
	}
	f.rates[id] = kbps
	return nil
}

func (f *fakeMeters) MeterDelete(id uint32) error {
	f.calls = append(f.calls, fmt.Sprintf("del %d", id))
	delete(f.rates, id)
	return nil
}

func limits() bandwidth.Limits { return bandwidth.Limits{GeneveKbps: 240000, LabCapKbps: 100000} }

func TestNoMetersWithoutLimits(t *testing.T) {
	be := newFakeMeters()
	g := newGeneveMeters(be)
	if g.Enabled() {
		t.Fatal("no limits, no meters")
	}
	if err := g.Split(7, 1); err != nil || len(be.calls) != 0 {
		t.Fatalf("a split lab without limits touches no meter: %v %v", err, be.calls)
	}
}

func TestSplitLabsShareTheClassAndAreCapped(t *testing.T) {
	be := newFakeMeters()
	g := newGeneveMeters(be)
	if toggled, err := g.SetLimits(limits()); !toggled || err != nil {
		t.Fatalf("turning on: %v %v", toggled, err)
	}
	if err := g.Split(7, 1); err != nil {
		t.Fatal(err)
	}
	if got := be.rates[labMeterID(7)]; got != 100000 {
		t.Fatalf("one split lab is held to the lab cap, got %d", got)
	}
	if got := be.rates[nodeMeterID]; got != 240000 {
		t.Fatalf("the node meter is the class, got %d", got)
	}
	for vni := uint(8); vni <= 13; vni++ {
		if err := g.Split(vni, 1); err != nil {
			t.Fatal(err)
		}
	}
	// 7 split labs: 240000/7
	for vni := uint(7); vni <= 13; vni++ {
		if got := be.rates[labMeterID(vni)]; got != 34285 {
			t.Fatalf("VNI %d has %d, want an equal part of the class", vni, got)
		}
	}
	// the sum of what the labs may take stays within the class
	var sum uint32
	for vni := uint(7); vni <= 13; vni++ {
		sum += be.rates[labMeterID(vni)]
	}
	if sum > 240000 {
		t.Fatalf("lab meters add up to %d, over the class", sum)
	}
}

func TestAFloodToTwoNodesHalvesTheRateAndTheNodeMeter(t *testing.T) {
	be := newFakeMeters()
	g := newGeneveMeters(be)
	g.SetLimits(limits())
	g.Split(7, 2)
	if got := be.rates[labMeterID(7)]; got != 50000 {
		t.Fatalf("a frame copied to two nodes counts twice: %d", got)
	}
	if got := be.rates[nodeMeterID]; got != 120000 {
		t.Fatalf("node meter: %d", got)
	}
}

func TestUnsplitDeletesTheLabMeterAndGivesTheShareBack(t *testing.T) {
	be := newFakeMeters()
	g := newGeneveMeters(be)
	g.SetLimits(limits())
	g.Split(7, 1)
	g.Split(8, 1)
	g.Split(9, 1)
	if got := be.rates[labMeterID(7)]; got != 80000 {
		t.Fatalf("three labs: %d", got)
	}
	if err := g.Unsplit(9); err != nil {
		t.Fatal(err)
	}
	if _, ok := be.rates[labMeterID(9)]; ok {
		t.Fatal("the meter of the lab that is no longer split must go")
	}
	if got := be.rates[labMeterID(7)]; got != 100000 { // 240000/2 = 120000, capped at 100000
		t.Fatalf("two labs now: %d", got)
	}
	if err := g.Unsplit(9); err != nil {
		t.Fatal("unsplitting twice is not an error")
	}
}

func TestARateChangeNeverDeletesAMeter(t *testing.T) {
	be := newFakeMeters()
	g := newGeneveMeters(be)
	g.SetLimits(limits())
	g.Split(7, 1)
	g.Split(8, 1)
	be.calls = nil
	if _, err := g.SetLimits(bandwidth.Limits{GeneveKbps: 100000, LabCapKbps: 100000}); err != nil {
		t.Fatal(err)
	}
	for _, c := range be.calls {
		if len(c) >= 3 && c[:3] == "del" {
			t.Fatalf("a delete would delete the flows that use the meter: %v", be.calls)
		}
	}
	if be.rates[labMeterID(7)] != 50000 || be.rates[nodeMeterID] != 100000 {
		t.Fatalf("new rates: %v", be.rates)
	}
}

func TestOnlyAChangedRateIsWritten(t *testing.T) {
	be := newFakeMeters()
	g := newGeneveMeters(be)
	g.SetLimits(limits())
	g.Split(7, 1)
	be.calls = nil
	if _, err := g.SetLimits(limits()); err != nil {
		t.Fatal(err)
	}
	if len(be.calls) != 0 {
		t.Fatalf("the same limits again must not touch the switch: %v", be.calls)
	}
}

func TestTurningTheLimitsOffClearsTheMeters(t *testing.T) {
	be := newFakeMeters()
	g := newGeneveMeters(be)
	g.SetLimits(limits())
	g.Split(7, 1)
	toggled, err := g.SetLimits(bandwidth.Limits{})
	if !toggled || err != nil || g.Enabled() {
		t.Fatalf("%v %v %v", toggled, err, g.Enabled())
	}
	if err := g.Clear(); err != nil {
		t.Fatal(err)
	}
	if len(be.rates) != 0 {
		t.Fatalf("meters left: %v", be.rates)
	}
}

func TestAFailedMeterIsRetriedAtTheNextChange(t *testing.T) {
	be := newFakeMeters()
	be.failSet[labMeterID(7)] = true
	g := newGeneveMeters(be)
	g.SetLimits(limits())
	if err := g.Split(7, 1); err == nil {
		t.Fatal("the failure must be reported")
	}
	be.failSet[labMeterID(7)] = false
	if err := g.Split(7, 1); err != nil {
		t.Fatal(err)
	}
	if be.rates[labMeterID(7)] != 100000 {
		t.Fatalf("%v", be.rates)
	}
}

func TestBurstCoversASegment(t *testing.T) {
	if burstFor(100) < 2048 || burstFor(1_000_000) != 100000 {
		t.Fatalf("burst: %d %d", burstFor(100), burstFor(1_000_000))
	}
}
