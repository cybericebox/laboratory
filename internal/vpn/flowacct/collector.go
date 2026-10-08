// Package flowacct counts, and never inspects, the connections that VPN
// clients open to the lab networks of one group. It reads conntrack (addresses,
// ports, flags and counters only), folds the flows into one cumulative
// aggregate per (client, lab, destination) and hands the aggregates to a
// reporter. The source address of a client is resolved to the client name here
// and never leaves the collector.
package flowacct

import (
	"fmt"
	"math"
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
	Mark       uint32
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
	Index  uint16
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
	partial     bool
	gap         bool
	checkpoints map[string]PairCounters
	// resumedUntil is the last moment a previous run had reported (zero on a
	// fresh start), so flows it already counted are not counted again.
	resumedUntil time.Time
}

// New builds a collector. bootID must change on every process start.
func New(source Source, topology func() Topology, bootID string, pollEvery time.Duration) *Collector {
	return &Collector{
		source: source, topology: topology, bootID: bootID,
		gapAfter: 3 * pollEvery,
		ledger:   map[Key]*Touch{}, flows: map[flowKey]*tracked{}, checkpoints: map[string]PairCounters{},
	}
}

// Resume seeds the ledger from the totals a previous run persisted and the
// moment it last reported. Call it before the first Poll.
func (c *Collector) Resume(ledger []Touch, reportedUntil time.Time, checkpoints ...PairCounters) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range ledger {
		row := t
		if len(c.ledger) >= MaxRows {
			c.truncated = true
			break
		}
		c.ledger[row.Key] = &row
	}
	c.resumedUntil = reportedUntil
	for _, cp := range checkpoints {
		if len(c.checkpoints) >= MaxRows {
			c.truncated = true
			break
		}
		c.checkpoints[cp.BindingID] = cp
	}
	if len(ledger) > 0 && len(checkpoints) == 0 {
		c.partial = true
		c.gap = true
	}
}

// MarkPartial keeps accumulated totals while exposing an unobserved interval.
func (c *Collector) MarkPartial(_ time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.partial = true
	c.gap = true
}

// ObserveCounters folds the sole packet/initiative authority. Checkpoints and
// public totals are updated under one lock and persisted in one report write.
func (c *Collector) ObserveCounters(snapshot CounterSnapshot) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if snapshot.Partial {
		c.partial = true
		c.gap = true
	}
	// Validate the complete sample before mutating any totals.
	deltas := make([]PairCounters, 0, len(snapshot.Rows))
	seen := map[string]bool{}
	for _, cur := range snapshot.Rows {
		if seen[cur.BindingID] {
			c.partial = true
			return fmt.Errorf("duplicate counter binding")
		}
		seen[cur.BindingID] = true
		d, partial, err := PairCounterDelta(cur, c.checkpoints[cur.BindingID])
		if err != nil {
			c.partial = true
			c.gap = true
			return err
		}
		if partial {
			c.partial = true
			c.gap = true
		}
		deltas = append(deltas, d)
	}
	if c.lastGood.IsZero() || c.gap || snapshot.At.Sub(c.lastGood) > c.gapAfter {
		c.coveredFrom = snapshot.At
	}
	c.gap = false
	c.lastGood = snapshot.At
	for i, d := range deltas {
		cur := snapshot.Rows[i]
		row := c.ledger[d.Key]
		hasTraffic := d.PacketsOut > 0 || d.PacketsIn > 0 || d.Attempts > 0 || d.LabInitiatedAttempts > 0
		if row == nil && hasTraffic {
			if len(c.ledger) >= MaxRows {
				c.truncated = true
				continue
			}
			row = &Touch{Key: d.Key}
			c.ledger[d.Key] = row
		}
		// Empty allowed pairs also need a checkpoint for a retained namespace.
		if _, ok := c.checkpoints[cur.BindingID]; !ok && len(c.checkpoints) >= MaxRows {
			c.truncated = true
			continue
		}
		c.checkpoints[cur.BindingID] = cur
		if row == nil {
			continue
		}
		add := func(dst *int64, n uint64) {
			if *dst < 0 || n > uint64(math.MaxInt64-*dst) {
				*dst = math.MaxInt64
				c.partial = true
				return
			}
			*dst += int64(n)
		}
		add(&row.PacketsOut, d.PacketsOut)
		add(&row.PacketsIn, d.PacketsIn)
		add(&row.BytesOut, d.BytesOut)
		add(&row.BytesIn, d.BytesIn)
		add(&row.Attempts, d.Attempts)
		add(&row.LabInitiatedAttempts, d.LabInitiatedAttempts)
		if d.Attempts > 0 {
			if row.FirstSeenMs == 0 {
				row.FirstSeenMs = snapshot.At.UnixMilli()
			}
			row.LastSeenMs = snapshot.At.UnixMilli()
		}
	}
	return nil
}

// ForgetBindings removes checkpoints only after their final observation has
// been folded. Historical public totals remain; a future epoch starts anew.
func (c *Collector) ForgetBindings(ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		delete(c.checkpoints, id)
	}
}

// Poll enriches client-origin timing only. Denied/unmarked and lab-origin
// flows never create a ledger row or contribute packet/initiative counts.
func (c *Collector) Poll(now time.Time) error {
	if c.source == nil {
		return nil
	}
	flows, err := c.source.Dump()
	if err != nil {
		c.MarkPartial(now)
		return err
	}
	topo := c.topology()
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[flowKey]bool{}
	allowedEndpoints := map[Key]map[netip.Addr]bool{}
	for _, cp := range c.checkpoints {
		prefix, err := netip.ParsePrefix(cp.ClientCIDR)
		if err != nil || prefix.Bits() != 32 {
			continue
		}
		if allowedEndpoints[cp.Key] == nil {
			allowedEndpoints[cp.Key] = map[netip.Addr]bool{}
		}
		allowedEndpoints[cp.Key][prefix.Addr()] = true
	}

	for _, f := range flows {
		if f.Mark&0x80000000 == 0 {
			continue
		}
		subject, ok := topo.Clients[f.Src]
		if !ok {
			continue
		}
		lab, ok := topo.lab(f.Dst)
		if !ok {
			continue
		}
		for _, l := range topo.Labs {
			if l.Name == lab && l.Index != uint16((f.Mark&0x00ffff00)>>8) {
				ok = false
			}
		}
		if !ok {
			continue
		}
		if !allowedEndpoints[Key{subject, lab}][f.Src] {
			continue
		}
		fk := flowKey{f.ID, f.Proto, f.Src, f.Dst, f.SrcPort, f.DstPort}
		seen[fk] = true
		trackedFlow := c.flows[fk]
		if trackedFlow == nil {
			trackedFlow = &tracked{key: Key{subject, lab}}
			c.flows[fk] = trackedFlow
		}
		row := c.ledger[trackedFlow.key]
		if row == nil || row.Attempts == 0 {
			continue
		}
		start := firstSeen(f.Start, now).UnixMilli()
		if row.FirstSeenMs == 0 || start < row.FirstSeenMs {
			row.FirstSeenMs = start
		}
		if !trackedFlow.responded && f.Replied {
			trackedFlow.responded = true
			if row.FirstRespondMs == 0 {
				row.FirstRespondMs = now.UnixMilli()
			}
		}
	}
	for fk := range c.flows {
		if !seen[fk] {
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
	report := Report{BootID: c.bootID, Truncated: c.truncated, Partial: c.partial, CoveredToMs: c.lastGood.UnixMilli()}
	if c.lastGood.IsZero() {
		report.CoveredToMs = 0
	}
	for _, cp := range c.checkpoints {
		report.KernelCheckpoints = append(report.KernelCheckpoints, cp)
	}
	sort.Slice(report.KernelCheckpoints, func(i, j int) bool {
		return report.KernelCheckpoints[i].BindingID < report.KernelCheckpoints[j].BindingID
	})
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
