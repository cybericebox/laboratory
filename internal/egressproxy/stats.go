package egressproxy

import (
	"encoding/json"
	"net/http"
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

// Minute is a finished minute.
type Minute struct {
	Start   time.Time `json:"start"`
	Rows    []Row     `json:"rows,omitempty"`
	Caps    []CapHit  `json:"caps,omitempty"`
	Denials []Denial  `json:"denials,omitempty"`
}

// Summaries is what the gateway serves.
type Summaries struct {
	// Gateway is the pod that answered (an identifier of the source, so a pulled minute is not counted twice when pods are renamed).
	Gateway string   `json:"gateway"`
	Minutes []Minute `json:"minutes"`
}

type rowKey struct{ lab, host string }
type capKey struct{ lab, reason string }

// Collector keeps per-minute summaries and serves the finished ones. It keeps the last Keep minutes (default 60, an hour) and nothing else:
// the operator pulls them every minute and keeps the log.
type Collector struct {
	Gateway string
	Keep    int
	Now     func() time.Time

	mu      sync.Mutex
	start   time.Time
	rows    map[rowKey]*Row
	caps    map[capKey]int64
	denials map[capKey]int64
	done    []Minute
}

func NewCollector(gateway string) *Collector {
	return &Collector{Gateway: gateway, Keep: 60, Now: time.Now}
}

func (c *Collector) rollLocked() {
	now := c.Now().UTC().Truncate(time.Minute)
	if c.rows != nil && now.Equal(c.start) {
		return
	}
	if c.rows != nil && (len(c.rows)+len(c.caps)+len(c.denials)) > 0 {
		m := Minute{Start: c.start}
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

// Handler serves GET /summaries?since=<RFC 3339>. It is plain HTTP on the pod network: the network policy of the group lets the operator
// in, and the lab side of the pod answers nothing (the INPUT policy).
func (c *Collector) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/summaries", func(w http.ResponseWriter, r *http.Request) {
		var since time.Time
		if s := r.URL.Query().Get("since"); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				http.Error(w, "since: "+err.Error(), http.StatusBadRequest)
				return
			}
			since = t
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c.Since(since))
	})
	return mux
}
