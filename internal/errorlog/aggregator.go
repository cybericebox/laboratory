package errorlog

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// MaxGroups bounds the distinct errors one process keeps; past it new ones count into the "overflow" group.
const MaxGroups = 200

// MaxSamples is how many recent messages a group keeps.
const MaxSamples = 3

// OverflowFingerprintKind is the kind of the group that takes the errors past MaxGroups.
const OverflowKind = "overflow"

// Fingerprint is the stable id of an error: the component, the kind of the error path and the normalized message.
func Fingerprint(component, kind, normalized string) string {
	sum := sha256.Sum256([]byte(component + "\x00" + kind + "\x00" + normalized))
	return hex.EncodeToString(sum[:8])
}

// Group is one distinct error of one component.
type Group struct {
	Fingerprint string
	Kind        string
	Normalized  string
	// Total counts the occurrences since the process started; the reader takes the difference between two looks.
	Total       int64
	First, Last time.Time
	Samples     []string
}

// Aggregator counts the errors of one process (one component instance) by fingerprint. It is safe for concurrent use.
type Aggregator struct {
	Component string
	Instance  string

	mu     sync.Mutex
	groups map[string]*Group
	dirty  map[string]bool
	now    func() time.Time
}

// New makes an aggregator for a component instance (a node name or a pod name, never an address).
func New(component, instance string) *Aggregator {
	return &Aggregator{Component: component, Instance: instance, groups: map[string]*Group{}, dirty: map[string]bool{}, now: time.Now}
}

// Record counts one error of the given kind (a short code of the error path: reconcile, ovs, registry, ...).
func (a *Aggregator) Record(kind, msg string) {
	norm := Normalize(msg)
	sample := Redact(msg)
	fp := Fingerprint(a.Component, kind, norm)
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.groups[fp]
	if !ok {
		if len(a.groups) >= MaxGroups {
			kind, norm = OverflowKind, "too many distinct errors"
			fp = Fingerprint(a.Component, kind, norm)
			if g, ok = a.groups[fp]; !ok {
				g = &Group{Fingerprint: fp, Kind: kind, Normalized: norm, First: now}
				a.groups[fp] = g
			}
		} else {
			g = &Group{Fingerprint: fp, Kind: kind, Normalized: norm, First: now}
			a.groups[fp] = g
		}
	}
	g.Total++
	g.Last = now
	g.Samples = append(g.Samples, sample)
	if len(g.Samples) > MaxSamples {
		g.Samples = g.Samples[len(g.Samples)-MaxSamples:]
	}
	a.dirty[g.Fingerprint] = true
}

// Dirty returns the groups that changed since the last call (copies) and forgets the mark.
func (a *Aggregator) Dirty() []Group {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Group, 0, len(a.dirty))
	for fp := range a.dirty {
		out = append(out, copyGroup(a.groups[fp]))
	}
	a.dirty = map[string]bool{}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	return out
}

// Snapshot returns every group (copies).
func (a *Aggregator) Snapshot() []Group {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Group, 0, len(a.groups))
	for _, g := range a.groups {
		out = append(out, copyGroup(g))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	return out
}

func copyGroup(g *Group) Group {
	c := *g
	c.Samples = append([]string(nil), g.Samples...)
	return c
}
