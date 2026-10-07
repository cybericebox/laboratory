package l7

import (
	"math"
	"sort"
	"sync"
	"time"
)

const (
	// MaxMeterKeys bounds distinct (namespace, user, lab) rows per replica.
	MaxMeterKeys  = 50_000
	MaxReportRows = 512
)

type meterKey struct{ client, lab string }
type meterRow struct {
	attempts, bytesIn, bytesOut       int64
	firstMs, lastMs, firstRespondedMs int64
}
type namespaceMeter struct {
	rows                                  map[meterKey]*meterRow
	active                                int
	truncated, partial, restored, retired bool
}
type Touch struct {
	Subject, Lab                         string
	Attempts, BytesIn, BytesOut          int64
	FirstSeenMs, LastSeenMs, RespondedMs int64
}

// Meter holds only cumulative counters and times. Each namespace has its own
// index, so publishing one group never scans the traffic of other groups.
type Meter struct {
	BootID     string
	Started    time.Time
	mu         sync.Mutex
	namespaces map[string]*namespaceMeter
	keys       int
}

func NewMeter(bootID string, started time.Time) *Meter {
	return &Meter{BootID: bootID, Started: started, namespaces: map[string]*namespaceMeter{}}
}
func (m *Meter) namespaceLocked(namespace string) *namespaceMeter {
	ns := m.namespaces[namespace]
	if ns == nil {
		ns = &namespaceMeter{rows: map[meterKey]*meterRow{}}
		m.namespaces[namespace] = ns
	}
	return ns
}
func (m *Meter) rowLocked(ns *namespaceMeter, key meterKey) *meterRow {
	if row := ns.rows[key]; row != nil {
		return row
	}
	if m.keys >= MaxMeterKeys {
		ns.truncated = true
		return nil
	}
	row := &meterRow{}
	ns.rows[key] = row
	m.keys++
	return row
}
func sumCounter(current, add int64, partial *bool) int64 {
	if add < 0 {
		*partial = true
		return current
	}
	if add > math.MaxInt64-current {
		*partial = true
		return math.MaxInt64
	}
	return current + add
}
func firstTime(current, add int64) int64 {
	if add > 0 && (current == 0 || add < current) {
		return add
	}
	return current
}

// RequestMeter makes a permitted request observable while it is still active.
// All updates and End share the Meter lock; duplicate completion is harmless.
type RequestMeter struct {
	meter     *Meter
	namespace string
	ns        *namespaceMeter
	row       *meterRow
	ended     bool
}

func (m *Meter) Begin(namespace, client, lab string, start time.Time) *RequestMeter {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns := m.namespaceLocked(namespace)
	ns.retired = false
	ns.active++
	row := m.rowLocked(ns, meterKey{client, lab})
	if row != nil {
		row.attempts = sumCounter(row.attempts, 1, &ns.partial)
		ms := start.UnixMilli()
		row.firstMs = firstTime(row.firstMs, ms)
		if ms > row.lastMs {
			row.lastMs = ms
		}
	}
	return &RequestMeter{meter: m, namespace: namespace, ns: ns, row: row}
}
func (r *RequestMeter) AddIn(n int64)  { r.add(n, true) }
func (r *RequestMeter) AddOut(n int64) { r.add(n, false) }
func (r *RequestMeter) add(n int64, in bool) {
	if r == nil {
		return
	}
	r.meter.mu.Lock()
	defer r.meter.mu.Unlock()
	if r.ended || r.row == nil {
		return
	}
	if in {
		r.row.bytesIn = sumCounter(r.row.bytesIn, n, &r.ns.partial)
	} else {
		r.row.bytesOut = sumCounter(r.row.bytesOut, n, &r.ns.partial)
	}
}
func (r *RequestMeter) Responded(now time.Time) {
	if r == nil {
		return
	}
	r.meter.mu.Lock()
	defer r.meter.mu.Unlock()
	if !r.ended && r.row != nil {
		r.row.firstRespondedMs = firstTime(r.row.firstRespondedMs, now.UnixMilli())
	}
}
func (r *RequestMeter) Incomplete() {
	if r == nil {
		return
	}
	r.meter.mu.Lock()
	defer r.meter.mu.Unlock()
	if !r.ended {
		r.ns.partial = true
	}
}
func (r *RequestMeter) End() {
	if r == nil {
		return
	}
	r.meter.mu.Lock()
	defer r.meter.mu.Unlock()
	if r.ended {
		return
	}
	r.ended = true
	r.ns.active--
	if r.ns.retired && r.ns.active == 0 {
		r.meter.removeLocked(r.namespace, r.ns)
	}
}

// Record preserves the finished-request API while HTTP wrappers migrate to Begin.
func (m *Meter) Record(namespace, client, lab string, start time.Time, responded bool, bytesIn, bytesOut int64) {
	r := m.Begin(namespace, client, lab, start)
	r.AddIn(bytesIn)
	r.AddOut(bytesOut)
	if responded {
		r.Responded(start)
	}
	r.End()
}
func (m *Meter) Ledger(namespace string) ([]Touch, bool) {
	rows, truncated, _ := m.Snapshot(namespace)
	return rows, truncated
}
func (m *Meter) Snapshot(namespace string) (rows []Touch, truncated, partial bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns := m.namespaces[namespace]
	if ns == nil {
		return nil, false, false
	}
	rows = make([]Touch, 0, len(ns.rows))
	for key, row := range ns.rows {
		rows = append(rows, Touch{Subject: key.client, Lab: key.lab, Attempts: row.attempts, BytesIn: row.bytesIn, BytesOut: row.bytesOut, FirstSeenMs: row.firstMs, LastSeenMs: row.lastMs, RespondedMs: row.firstRespondedMs})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Attempts != rows[j].Attempts {
			return rows[i].Attempts > rows[j].Attempts
		}
		if rows[i].Subject != rows[j].Subject {
			return rows[i].Subject < rows[j].Subject
		}
		return rows[i].Lab < rows[j].Lab
	})
	truncated, partial = ns.truncated, ns.partial
	if len(rows) > MaxReportRows {
		rows, truncated = rows[:MaxReportRows], true
	}
	return
}

// Restore folds a successfully loaded durable baseline once. Traffic counted
// before the API became available remains additive, never replaced by old rows.
func (m *Meter) Restore(namespace string, rows []Touch, truncated, partial bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns := m.namespaceLocked(namespace)
	if ns.restored {
		return
	}
	ns.restored = true
	ns.truncated = ns.truncated || truncated
	ns.partial = ns.partial || partial
	for _, touch := range rows {
		row := m.rowLocked(ns, meterKey{touch.Subject, touch.Lab})
		if row == nil {
			continue
		}
		row.attempts = sumCounter(row.attempts, touch.Attempts, &ns.partial)
		row.bytesIn = sumCounter(row.bytesIn, touch.BytesIn, &ns.partial)
		row.bytesOut = sumCounter(row.bytesOut, touch.BytesOut, &ns.partial)
		row.firstMs = firstTime(row.firstMs, touch.FirstSeenMs)
		row.firstRespondedMs = firstTime(row.firstRespondedMs, touch.RespondedMs)
		if touch.LastSeenMs > row.lastMs {
			row.lastMs = touch.LastSeenMs
		}
	}
}
func (m *Meter) removeLocked(namespace string, ns *namespaceMeter) {
	if m.namespaces[namespace] != ns {
		return
	}
	m.keys -= len(ns.rows)
	delete(m.namespaces, namespace)
}

// RetireNamespaces reclaims disappeared groups only after their live requests
// finish; these rows cannot be needed by the final publication of active groups.
func (m *Meter) RetireNamespaces(active map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, ns := range m.namespaces {
		ns.retired = !active[name]
		if ns.retired && ns.active == 0 {
			m.removeLocked(name, ns)
		}
	}
}
