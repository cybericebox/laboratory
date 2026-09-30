package flowacct

import (
	"errors"
	"fmt"
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
		Labs:    []LabNet{{Name: "c-1", Prefix: netip.MustParsePrefix("10.9.1.0/24")}},
	}
}

func newCollector(src *fakeSource) *Collector {
	return New(src, topo, "boot-1", 5*time.Second)
}

func flow(id uint32, src netip.Addr, dst netip.Addr, port uint16) Flow {
	return Flow{ID: id, Proto: "tcp", Src: src, Dst: dst, SrcPort: 40000 + uint16(id), DstPort: port}
}

func findRow(t *testing.T, r Report, subject string, port uint16) Touch {
	t.Helper()
	for _, row := range r.Ledger {
		if row.Subject == subject && row.DstPort == port {
			return row
		}
	}
	t.Fatalf("no row for %s:%d in %+v", subject, port, r.Ledger)
	return Touch{}
}

func TestFiltersOutsideClientToLabTraffic(t *testing.T) {
	src := &fakeSource{flows: []Flow{
		flow(1, netip.MustParseAddr("203.0.113.9"), netip.MustParseAddr("10.8.0.1"), 51820), // outer WireGuard
		flow(2, clientA, netip.MustParseAddr("10.8.0.1"), 8080),                             // probe, not a lab
		flow(3, clientA, netip.MustParseAddr("10.9.7.7"), 80),                               // unknown network
		flow(4, clientA, labIP, 80),
	}}
	c := newCollector(src)
	if err := c.Poll(t0); err != nil {
		t.Fatal(err)
	}
	r := c.Snapshot(t0)
	if len(r.Ledger) != 1 || r.Ledger[0].Subject != "p-a" || r.Ledger[0].Lab != "c-1" || r.Ledger[0].DstIP != "10.9.1.5" {
		t.Fatalf("unexpected ledger: %+v", r.Ledger)
	}
}

func TestAttemptsFirstLastAndResponded(t *testing.T) {
	src := &fakeSource{}
	c := newCollector(src)

	f1 := flow(1, clientA, labIP, 22)
	f1.Start = t0.Add(-2 * time.Second)
	src.flows = []Flow{f1}
	_ = c.Poll(t0)

	row := findRow(t, c.Snapshot(t0), "p-a", 22)
	if row.Attempts != 1 || row.FirstSeenMs != t0.Add(-2*time.Second).UnixMilli() || row.FirstRespondMs != 0 {
		t.Fatalf("after syn: %+v", row)
	}

	f1.Replied, f1.PacketsOut, f1.PacketsIn, f1.BytesOut, f1.BytesIn = true, 4, 3, 300, 200
	f2 := flow(2, clientA, labIP, 22)
	src.flows = []Flow{f1, f2}
	_ = c.Poll(t0.Add(5 * time.Second))

	row = findRow(t, c.Snapshot(t0), "p-a", 22)
	if row.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", row.Attempts)
	}
	if row.FirstRespondMs != t0.Add(5*time.Second).UnixMilli() || row.BytesIn != 200 || row.PacketsOut != 4 {
		t.Fatalf("after reply: %+v", row)
	}
	if row.LastSeenMs != t0.Add(5*time.Second).UnixMilli() {
		t.Fatalf("last seen = %d", row.LastSeenMs)
	}
}

func TestCountersAreDeltasAndSurviveTheFlowLeaving(t *testing.T) {
	src := &fakeSource{}
	c := newCollector(src)
	f := flow(1, clientA, labIP, 80)
	f.BytesOut = 100
	src.flows = []Flow{f}
	_ = c.Poll(t0)
	f.BytesOut = 250
	src.flows = []Flow{f}
	_ = c.Poll(t0.Add(5 * time.Second))
	src.flows = nil // conntrack entry expired
	_ = c.Poll(t0.Add(10 * time.Second))
	// The same tuple and id showing up again is a new connection.
	f.BytesOut = 10
	src.flows = []Flow{f}
	_ = c.Poll(t0.Add(15 * time.Second))

	row := findRow(t, c.Snapshot(t0), "p-a", 80)
	if row.Attempts != 2 || row.BytesOut != 260 {
		t.Fatalf("got attempts=%d bytesOut=%d", row.Attempts, row.BytesOut)
	}
}

func TestSubjectsAreSeparate(t *testing.T) {
	src := &fakeSource{flows: []Flow{flow(1, clientA, labIP, 80), flow(2, clientB, labIP, 80), flow(3, clientB, labIP, 80)}}
	c := newCollector(src)
	_ = c.Poll(t0)
	r := c.Snapshot(t0)
	if findRow(t, r, "p-a", 80).Attempts != 1 || findRow(t, r, "p-b", 80).Attempts != 2 {
		t.Fatalf("ledger: %+v", r.Ledger)
	}
}

func TestPortScanFoldsIntoOneRow(t *testing.T) {
	src := &fakeSource{}
	for port := 1; port <= 1000; port++ {
		src.flows = append(src.flows, flow(uint32(port), clientA, labIP, uint16(port)))
	}
	c := newCollector(src)
	_ = c.Poll(t0)
	r := c.Snapshot(t0)
	if len(r.Ledger) != MaxKeysPerPair+1 {
		t.Fatalf("rows = %d, want %d", len(r.Ledger), MaxKeysPerPair+1)
	}
	var overflow *Touch
	var total int64
	for i := range r.Ledger {
		total += r.Ledger[i].Attempts
		if r.Ledger[i].Proto == OverflowProto {
			overflow = &r.Ledger[i]
		}
	}
	if overflow == nil || overflow.Attempts != 1000-MaxKeysPerPair || total != 1000 {
		t.Fatalf("overflow=%+v total=%d", overflow, total)
	}
}

func TestLedgerCapMarksTruncated(t *testing.T) {
	clients := map[netip.Addr]string{}
	src := &fakeSource{}
	for i := 0; i < MaxRows+10; i++ {
		a := netip.AddrFrom4([4]byte{10, 8, byte(i / 250), byte(i%250 + 2)})
		clients[a] = fmt.Sprintf("p-%d", i)
		src.flows = append(src.flows, flow(uint32(i+1), a, labIP, 80))
	}
	c := New(src, func() Topology {
		return Topology{Clients: clients, Labs: topo().Labs}
	}, "b", time.Second)
	_ = c.Poll(t0)
	r := c.Snapshot(t0)
	if len(r.Ledger) != MaxRows || !r.Truncated {
		t.Fatalf("rows=%d truncated=%v", len(r.Ledger), r.Truncated)
	}
}

func TestFailedDumpChangesNothingAndGapRestartsCoverage(t *testing.T) {
	src := &fakeSource{}
	c := newCollector(src)
	_ = c.Poll(t0)
	if got := c.Snapshot(t0).CoveredFromMs; got != t0.UnixMilli() {
		t.Fatalf("coveredFrom = %d", got)
	}
	_ = c.Poll(t0.Add(5 * time.Second))
	if got := c.Snapshot(t0).CoveredFromMs; got != t0.UnixMilli() {
		t.Fatalf("contiguous polls moved coveredFrom to %d", got)
	}

	src.err = errors.New("netlink busy")
	if err := c.Poll(t0.Add(10 * time.Second)); err == nil {
		t.Fatal("expected the dump error")
	}
	src.err = nil
	late := t0.Add(60 * time.Second)
	_ = c.Poll(late)
	r := c.Snapshot(late)
	if r.CoveredFromMs != late.UnixMilli() || r.CoveredToMs != late.UnixMilli() {
		t.Fatalf("after a gap coveredFrom=%d coveredTo=%d", r.CoveredFromMs, r.CoveredToMs)
	}
}

func TestReissuedAddressKeepsOldFlowsWithOldOwner(t *testing.T) {
	names := map[netip.Addr]string{clientA: "p-old"}
	src := &fakeSource{flows: []Flow{flow(1, clientA, labIP, 80)}}
	c := New(src, func() Topology { return Topology{Clients: names, Labs: topo().Labs} }, "b", 5*time.Second)
	_ = c.Poll(t0)
	names[clientA] = "p-new"
	src.flows = []Flow{flow(1, clientA, labIP, 80), flow(2, clientA, labIP, 80)}
	_ = c.Poll(t0.Add(5 * time.Second))
	r := c.Snapshot(t0)
	if findRow(t, r, "p-old", 80).Attempts != 1 || findRow(t, r, "p-new", 80).Attempts != 1 {
		t.Fatalf("ledger: %+v", r.Ledger)
	}
}
