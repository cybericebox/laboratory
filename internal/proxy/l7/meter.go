package l7

import (
	"sort"
	"sync"
	"time"
)

const (
	// MaxMeterKeys bounds distinct (user, lab) rows per proxy replica.
	MaxMeterKeys = 50_000
	// MaxReportRows bounds one namespace's ledger so the custom resource stays small.
	MaxReportRows = 512
)

type meterKey struct{ namespace, client, lab string }

type meterRow struct {
	attempts, bytesIn, bytesOut       int64
	firstMs, lastMs, firstRespondedMs int64
}

// Touch is one cumulative row: what one LabGroupClient did against one lab
// device since the proxy replica started. Subject holds the client name, the
// same value the VPN reports.
type Touch struct {
	Subject, Lab                         string
	Attempts, BytesIn, BytesOut          int64
	FirstSeenMs, LastSeenMs, RespondedMs int64
}

// Meter counts requests per (namespace, user, lab). It keeps counts and
// times only: no path, query, header, address or user agent ever reaches it.
type Meter struct {
	BootID  string
	Started time.Time

	mu        sync.Mutex
	rows      map[meterKey]*meterRow
	truncated bool
}

func NewMeter(bootID string, started time.Time) *Meter {
	return &Meter{BootID: bootID, Started: started, rows: map[meterKey]*meterRow{}}
}

// Record adds one finished request. start is when the request began; responded
// is set when the lab itself answered (any status), not when the proxy failed
// to reach it. bytesIn is lab to client, bytesOut client to lab.
func (m *Meter) Record(namespace, client, lab string, start time.Time, responded bool, bytesIn, bytesOut int64) {
	key := meterKey{namespace, client, lab}
	ms := start.UnixMilli()
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.rows[key]
	if row == nil {
		if len(m.rows) >= MaxMeterKeys {
			m.truncated = true
			return
		}
		row = &meterRow{firstMs: ms, lastMs: ms}
		m.rows[key] = row
	}
	row.attempts++
	row.bytesIn += bytesIn
	row.bytesOut += bytesOut
	if ms < row.firstMs {
		row.firstMs = ms
	}
	if ms > row.lastMs {
		row.lastMs = ms
	}
	if responded && (row.firstRespondedMs == 0 || ms < row.firstRespondedMs) {
		row.firstRespondedMs = ms
	}
}

// Ledger returns the rows of one namespace, at most MaxReportRows (the busiest
// first when it has to cut), and whether anything was dropped.
func (m *Meter) Ledger(namespace string) (rows []Touch, truncated bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, row := range m.rows {
		if key.namespace != namespace {
			continue
		}
		rows = append(rows, Touch{
			Subject: key.client, Lab: key.lab,
			Attempts: row.attempts, BytesIn: row.bytesIn, BytesOut: row.bytesOut,
			FirstSeenMs: row.firstMs, LastSeenMs: row.lastMs, RespondedMs: row.firstRespondedMs,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Attempts != rows[j].Attempts {
			return rows[i].Attempts > rows[j].Attempts
		}
		return rows[i].Subject+rows[i].Lab < rows[j].Subject+rows[j].Lab
	})
	truncated = m.truncated
	if len(rows) > MaxReportRows {
		rows, truncated = rows[:MaxReportRows], true
	}
	return rows, truncated
}
