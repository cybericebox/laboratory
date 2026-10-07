package flowacct

import (
	"net/netip"
	"testing"
	"time"
)

type fakeSource struct {
	flows []Flow
	err   error
}

func (f *fakeSource) Dump() ([]Flow, error) { return f.flows, f.err }

var (
	t0      = time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	clientA = netip.MustParseAddr("10.8.0.2")
	clientB = netip.MustParseAddr("10.8.0.3")
	labIP   = netip.MustParseAddr("10.9.1.5")
)

func topo() Topology {
	return Topology{
		Clients: map[netip.Addr]string{clientA: "p-a", clientB: "p-b"},
		Labs:    []LabNet{{Name: "c-1", Index: 1, Prefix: netip.MustParsePrefix("10.9.1.0/24")}},
	}
}

func newCollector(src *fakeSource) *Collector {
	return New(src, topo, "boot-1", 5*time.Second)
}

func flow(id uint32, src netip.Addr, dst netip.Addr, port uint16) Flow {
	return Flow{ID: id, Proto: "tcp", Src: src, Dst: dst, SrcPort: 40000 + uint16(id), DstPort: port}
}

func findRow(t *testing.T, r Report, subject string, _ uint16) Touch {
	t.Helper()
	for _, row := range r.Ledger {
		if row.Subject == subject && row.Lab == "c-1" {
			return row
		}
	}
	t.Fatalf("no row for %s in %+v", subject, r.Ledger)
	return Touch{}
}

func TestMetadataOnlyEnrichesPermittedClientOrigin(t *testing.T) {
	src := &fakeSource{flows: []Flow{flow(1, clientA, labIP, 80), flow(2, labIP, clientA, 80), flow(3, netip.MustParseAddr("203.0.113.1"), clientA, 51820)}}
	c := newCollector(src)
	observe(t, c, sample(2), t0)
	src.flows[0].Mark = 0x80000100
	src.flows[0].Start = t0.Add(-time.Second)
	src.flows[0].Replied = true
	for i := 0; i < 2; i++ {
		if err := c.Poll(t0); err != nil {
			t.Fatal(err)
		}
	}
	row := findRow(t, c.Snapshot(t0), "p-a", 80)
	if row.Attempts != 1 || row.PacketsOut != 2 || row.FirstSeenMs != t0.Add(-time.Second).UnixMilli() || row.FirstRespondMs != t0.UnixMilli() {
		t.Fatalf("metadata/counts %+v", row)
	}
	if len(c.Snapshot(t0).Ledger) != 1 {
		t.Fatal("privacy filter failed")
	}
}
func TestCounterReadGapRetainsTotalsAndRestartsCoverage(t *testing.T) {
	c := newCollector(&fakeSource{})
	observe(t, c, sample(3), t0)
	c.MarkPartial(t0.Add(time.Second))
	before := c.Snapshot(t0.Add(time.Minute))
	if !before.Partial || before.CoveredToMs != t0.UnixMilli() {
		t.Fatalf("gap hidden %+v", before)
	}
	later := t0.Add(time.Minute)
	observe(t, c, sample(4), later)
	r := c.Snapshot(later)
	if r.Ledger[0].PacketsOut != 4 || r.CoveredFromMs != later.UnixMilli() {
		t.Fatalf("gap recovery %+v", r)
	}
}
func TestBindingRetirementAndNewOwnerKeepSeparateTotals(t *testing.T) {
	c := newCollector(&fakeSource{})
	observe(t, c, sample(3), t0)
	c.ForgetBindings([]string{"binding"})
	cur := sample(2)
	cur.BindingID = "new-binding"
	cur.Subject = "p-new"
	observe(t, c, cur, t0)
	r := c.Snapshot(t0)
	if len(r.Ledger) != 2 || findRow(t, r, "p-a", 0).PacketsOut != 3 || findRow(t, r, "p-new", 0).PacketsOut != 2 {
		t.Fatalf("identity crossed %+v", r)
	}
}
