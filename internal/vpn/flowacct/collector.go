// Package flowacct counts, and never inspects, the connections that VPN
// clients open to the lab networks of one group. It reads conntrack (addresses,
// ports, flags and counters only), folds the flows into one cumulative
// aggregate per (client, lab, destination) and hands the aggregates to a
// reporter. The source address of a client is resolved to the client name here
// and never leaves the collector.
package flowacct

import (
	"net/netip"
	"sort"
	"sync"
	"time"
)

const (
	// MaxRows bounds the whole ledger (one row per client and lab) so the custom
	// resource stays small.
	MaxRows = 512
)

// Flow is the payload-free view of one conntrack entry, in the original
// (client to lab) direction.
type Flow struct {
	ID         uint32
	Proto      string
	Src, Dst   netip.Addr
	SrcPort    uint16
	DstPort    uint16
	Start      time.Time // zero when the kernel has no timestamp
	Replied    bool      // the lab answered (SEEN_REPLY or reply packets)
	PacketsOut uint64
	BytesOut   uint64
	PacketsIn  uint64
	BytesIn    uint64
}

// Source lists the current flows of the pod network namespace.
type Source interface {
	Dump() ([]Flow, error)
}

// LabNet is the /24 of one lab as seen from the VPN pod.
type LabNet struct {
	Name   string
	Prefix netip.Prefix
}

// Topology maps addresses to identities. It is rebuilt from the cluster state.
type Topology struct {
	Clients map[netip.Addr]string // assigned VPN address -> LabGroupClient name
	Labs    []LabNet
}

func (t Topology) lab(addr netip.Addr) (string, bool) {
	for _, lab := range t.Labs {
		if lab.Prefix.Contains(addr) {
			return lab.Name, true
		}
	}
	return "", false
}

// Key is the aggregate identity: one VPN client against one lab. Any traffic
// from the client to any address of the lab's network counts as access to the lab.
type Key struct {
	Subject string
	Lab     string
}

// Touch is one cumulative aggregate. Times are Unix milliseconds.
type Touch struct {
	Key
	Attempts             int64
	LabInitiatedAttempts int64
	PacketsOut           int64
	PacketsIn            int64
	BytesOut             int64
	BytesIn              int64
	FirstSeenMs          int64
	LastSeenMs           int64
	FirstRespondMs       int64
}

// Report is the state handed to the reporter.
type Report struct {
	BootID            string
	CoveredFromMs     int64
	CoveredToMs       int64
	Truncated         bool
	Partial           bool
	KernelCheckpoints []PairCounters
	Ledger            []Touch
}

type flowKey struct {
	id       uint32
	proto    string
	src, dst netip.Addr
	sport    uint16
	dport    uint16
}

type tracked struct {
	key       Key
	counters  [4]uint64 // packets out, packets in, bytes out, bytes in
	responded bool
}

// Collector is safe for concurrent Poll and Snapshot.
type Collector struct {
	source   Source
	topology func() Topology
	bootID   string
	// gapAfter is the pause between two good dumps after which the covered span
	// restarts: flows that lived only inside the pause were not observed.
	gapAfter time.Duration

	mu          sync.Mutex
	ledger      map[Key]*Touch
	flows       map[flowKey]*tracked
	coveredFrom time.Time
	lastGood    time.Time
	truncated   bool
	// resumedUntil is the last moment a previous run had reported (zero on a
	// fresh start), so flows it already counted are not counted again.
	resumedUntil time.Time
}

// New builds a collector. bootID must change on every process start.
func New(source Source, topology func() Topology, bootID string, pollEvery time.Duration) *Collector {
	return &Collector{
		source: source, topology: topology, bootID: bootID,
		gapAfter: 3 * pollEvery,
		ledger:   map[Key]*Touch{}, flows: map[flowKey]*tracked{},
	}
}

// Resume seeds the ledger from the totals a previous run persisted and the
// moment it last reported. Call it before the first Poll.
func (c *Collector) Resume(ledger []Touch, reportedUntil time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range ledger {
		row := t
		c.ledger[row.Key] = &row
	}
	c.resumedUntil = reportedUntil
}

// Poll reads the flows once and folds them into the ledger. A failed dump
// changes nothing, so the covered span shows the hole.
func (c *Collector) Poll(now time.Time) error {
	flows, err := c.source.Dump()
	if err != nil {
		return err
	}
	topo := c.topology()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastGood.IsZero() || now.Sub(c.lastGood) > c.gapAfter {
		c.coveredFrom = now
	}
	c.lastGood = now

	seen := make(map[flowKey]struct{}, len(flows))
	for i := range flows {
		f := &flows[i]
		// The destination filter drops everything that is not client to lab, in
		// particular the outer WireGuard flows that carry users' public addresses.
		subject, ok := topo.Clients[f.Src]
		if !ok {
			continue
		}
		lab, ok := topo.lab(f.Dst)
		if !ok {
			continue
		}
		fk := flowKey{f.ID, f.Proto, f.Src, f.Dst, f.SrcPort, f.DstPort}
		seen[fk] = struct{}{}
		t := c.flows[fk]
		if t == nil {
			key := Key{Subject: subject, Lab: lab}
			t = &tracked{key: key}
			c.flows[fk] = t
			// A flow that started before the last report of a previous run was
			// counted by that run: keep it, add nothing.
			counted := !f.Start.IsZero() && !c.resumedUntil.IsZero() && !f.Start.After(c.resumedUntil)
			if counted {
				t.counters = [4]uint64{f.PacketsOut, f.PacketsIn, f.BytesOut, f.BytesIn}
				t.responded = f.Replied || f.PacketsIn > 0
				continue
			}
			row := c.row(key, firstSeen(f.Start, now))
			if row == nil {
				delete(c.flows, fk)
				continue
			}
			row.Attempts++
		}
		row := c.ledger[t.key]
		if row == nil {
			continue
		}
		cur := [4]uint64{f.PacketsOut, f.PacketsIn, f.BytesOut, f.BytesIn}
		row.PacketsOut += delta(cur[0], t.counters[0])
		row.PacketsIn += delta(cur[1], t.counters[1])
		row.BytesOut += delta(cur[2], t.counters[2])
		row.BytesIn += delta(cur[3], t.counters[3])
		t.counters = cur
		if !t.responded && (f.Replied || f.PacketsIn > 0) {
			t.responded = true
			if row.FirstRespondMs == 0 {
				row.FirstRespondMs = now.UnixMilli()
			}
		}
		if ms := now.UnixMilli(); ms > row.LastSeenMs {
			row.LastSeenMs = ms
		}
	}
	for fk := range c.flows {
		if _, ok := seen[fk]; !ok {
			delete(c.flows, fk)
		}
	}
	return nil
}

func (c *Collector) row(key Key, first time.Time) *Touch {
	if row, ok := c.ledger[key]; ok {
		if ms := first.UnixMilli(); ms < row.FirstSeenMs {
			row.FirstSeenMs = ms
		}
		return row
	}
	if len(c.ledger) >= MaxRows {
		c.truncated = true
		return nil
	}
	row := &Touch{Key: key, FirstSeenMs: first.UnixMilli(), LastSeenMs: first.UnixMilli()}
	c.ledger[key] = row
	return row
}

// Snapshot copies the current ledger.
func (c *Collector) Snapshot(now time.Time) Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	report := Report{BootID: c.bootID, Truncated: c.truncated, CoveredToMs: now.UnixMilli()}
	if !c.coveredFrom.IsZero() {
		report.CoveredFromMs = c.coveredFrom.UnixMilli()
	}
	report.Ledger = make([]Touch, 0, len(c.ledger))
	for _, row := range c.ledger {
		report.Ledger = append(report.Ledger, *row)
	}
	sort.Slice(report.Ledger, func(i, j int) bool {
		a, b := report.Ledger[i].Key, report.Ledger[j].Key
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		if a.Lab != b.Lab {
			return a.Lab < b.Lab
		}
		return false
	})
	return report
}

func firstSeen(start, now time.Time) time.Time {
	if start.IsZero() || start.After(now) {
		return now
	}
	return start
}

// delta is a counter growth; a smaller value means the entry was replaced.
func delta(cur, prev uint64) int64 {
	if cur < prev {
		return int64(cur)
	}
	return int64(cur - prev)
}
