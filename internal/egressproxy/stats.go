package egressproxy

import (
	"sort"
	"sync"
	"time"
)

// Row is one lab's traffic to one host name in one minute. Bytes are counted when a connection ends, in the minute it ends; Conns when it
// starts. No address of a participant, no content: the lab (its name) and the host name asked for.
type Row struct {
	Lab    string `json:"lab"`
	Host   string `json:"host"`
	Conns  int64  `json:"conns"`
	Up     int64  `json:"up"`
	Down   int64  `json:"down"`
	Denied int64  `json:"denied,omitempty"`
}

// CapHit counts how often a lab sat at one of its caps in a minute (reason: lab-rate, dns-rate, ...).
type CapHit struct {
	Lab    string `json:"lab"`
	Reason string `json:"reason"`
	Hits   int64  `json:"hits"`
}

// Denial counts the refused connections of a lab by reason.
type Denial struct {
	Lab    string `json:"lab"`
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

// FlowRow is one lab's traffic from one source address to one destination address and port in one minute. It exists only when the
// gateway was told to keep activity reports (the chart value activityReports.enabled): the raw facts the platform may ask the agent for. The
// source address is what the lab used; which device owns it is for the platform to say (best effort: a lab may use any addresses).
type FlowRow struct {
	Lab     string `json:"lab"`
	Source  string `json:"src"`
	Dest    string `json:"dst"`
	Port    int    `json:"port"`
	Conns   int64  `json:"conns"`
	Up      int64  `json:"up"`
	Down    int64  `json:"down"`
	FirstMs int64  `json:"firstMs"`
	LastMs  int64  `json:"lastMs"`
}

// MaxFlowRows bounds the flow rows of a minute.
const MaxFlowRows = 5000

// Minute is a finished minute.
type Minute struct {
	Start   time.Time `json:"start"`
	Flows   []FlowRow `json:"flows,omitempty"`
	Rows    []Row     `json:"rows,omitempty"`
	Caps    []CapHit  `json:"caps,omitempty"`
	Denials []Denial  `json:"denials,omitempty"`
}

// Summaries is a set of finished minutes with the pod that made them (the shape the operator reads, built from a LabEgressReport).
type Summaries struct {
	// Gateway is the pod that answered (an identifier of the source, so a pulled minute is not counted twice when pods are renamed).
	Gateway string   `json:"gateway"`
	Minutes []Minute `json:"minutes"`
}

type rowKey struct{ lab, host string }
type flowKey struct {
	lab, src, dst string
	port          int
}
type capKey struct{ lab, reason string }

// Collector keeps per-minute summaries and serves the finished ones. It keeps the last Keep minutes (default 60, an hour) and nothing else:
// the ReportWriter takes them into the LabEgressReport and the operator keeps the log.
type Collector struct {
	Gateway string
	Keep    int
	Now     func() time.Time
	// Detail keeps the FlowRows of the minutes (activity reports); off by default.
	Detail bool

	mu      sync.Mutex
	start   time.Time
	rows    map[rowKey]*Row
	caps    map[capKey]int64
	denials map[capKey]int64
	done    []Minute
	flows   map[flowKey]*FlowRow
}

func NewCollector(gateway string) *Collector {
	return &Collector{Gateway: gateway, Keep: 60, Now: time.Now}
}

func (c *Collector) rollLocked() {
	now := c.Now().UTC().Truncate(time.Minute)
	if c.rows != nil && now.Equal(c.start) {
		return
	}
	if c.rows != nil && (len(c.rows)+len(c.caps)+len(c.denials)+len(c.flows)) > 0 {
		m := Minute{Start: c.start}
		for _, f := range c.flows {
			m.Flows = append(m.Flows, *f)
		}
		sort.Slice(m.Flows, func(i, j int) bool {
			a, b := m.Flows[i], m.Flows[j]
			if a.Lab != b.Lab {
				return a.Lab < b.Lab
			}
			if a.Source != b.Source {
				return a.Source < b.Source
			}
			if a.Dest != b.Dest {
				return a.Dest < b.Dest
			}
			return a.Port < b.Port
		})
		for _, r := range c.rows {
			m.Rows = append(m.Rows, *r)
		}
		sort.Slice(m.Rows, func(i, j int) bool {
			if m.Rows[i].Lab != m.Rows[j].Lab {
				return m.Rows[i].Lab < m.Rows[j].Lab
			}
			return m.Rows[i].Host < m.Rows[j].Host
		})
		for k, n := range c.caps {
			m.Caps = append(m.Caps, CapHit{Lab: k.lab, Reason: k.reason, Hits: n})
		}
		for k, n := range c.denials {
			m.Denials = append(m.Denials, Denial{Lab: k.lab, Reason: k.reason, Count: n})
		}
		sort.Slice(m.Caps, func(i, j int) bool { return m.Caps[i].Lab+m.Caps[i].Reason < m.Caps[j].Lab+m.Caps[j].Reason })
		sort.Slice(m.Denials, func(i, j int) bool {
			return m.Denials[i].Lab+m.Denials[i].Reason < m.Denials[j].Lab+m.Denials[j].Reason
		})
		c.done = append(c.done, m)
		if keep := max(c.Keep, 1); len(c.done) > keep {
			c.done = c.done[len(c.done)-keep:]
		}
	}
	c.start = now
	c.rows, c.caps, c.denials = map[rowKey]*Row{}, map[capKey]int64{}, map[capKey]int64{}
	if c.Detail {
		c.flows = map[flowKey]*FlowRow{}
	}
}

func (c *Collector) row(lab, host string) *Row {
	c.rollLocked()
	k := rowKey{lab, host}
	r := c.rows[k]
	if r == nil {
		if len(c.rows) >= 20000 { // a lab cannot grow the table without end
			k = rowKey{lab, "(other)"}
			if r = c.rows[k]; r == nil {
				r = &Row{Lab: lab, Host: "(other)"}
				c.rows[k] = r
			}
			return r
		}
		r = &Row{Lab: lab, Host: host}
		c.rows[k] = r
	}
	return r
}

// Start counts a connection of a lab to a host.
func (c *Collector) Start(lab, host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.row(lab, host).Conns++
}

// End adds the bytes of a finished connection (up: from the lab, down: to the lab).
func (c *Collector) End(lab, host string, up, down int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.row(lab, host)
	r.Up += up
	r.Down += down
}

// Flow adds a finished connection of a lab to the raw facts of the minute (only with Detail). up: from the lab, down: to the lab.
func (c *Collector) Flow(lab, src, dst string, port int, up, down int64) {
	if !c.Detail {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollLocked()
	k := flowKey{lab, src, dst, port}
	f := c.flows[k]
	if f == nil {
		if len(c.flows) >= MaxFlowRows {
			return
		}
		f = &FlowRow{Lab: lab, Source: src, Dest: dst, Port: port, FirstMs: c.Now().UnixMilli()}
		c.flows[k] = f
	}
	f.Conns++
	f.Up += up
	f.Down += down
	f.LastMs = c.Now().UnixMilli()
}

// Denied counts a refused connection or query.
func (c *Collector) Denied(lab, host, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if host != "" {
		c.row(lab, host).Denied++
	}
	c.rollLocked()
	c.denials[capKey{lab, reason}]++
}

// Cap counts a time a lab sat at one of its caps.
func (c *Collector) Cap(lab, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollLocked()
	c.caps[capKey{lab, reason}]++
}

// Since returns the finished minutes that start after t.
func (c *Collector) Since(t time.Time) Summaries {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollLocked()
	out := Summaries{Gateway: c.Gateway}
	for _, m := range c.done {
		if m.Start.After(t) {
			out.Minutes = append(out.Minutes, m)
		}
	}
	return out
}
