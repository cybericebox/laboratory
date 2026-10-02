package grpc

import (
	"context"
	"sort"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/errorlog"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// The laboratory's own error journal, as the agent sees it. The components publish their aggregated errors as Events
// (internal/errorlog); the agent reads them, turns the cumulative totals into deltas, keeps a bounded ring and hands each Monitoring
// stream what happened since its last message.
const (
	// errorRing is how many delta observations the agent keeps.
	errorRing = 5000
	// errorFirstWindow is how far back the first message of a stream looks.
	errorFirstWindow = 10 * time.Minute
	// certReportEvery is how often the certificate expiry is repeated when nothing else changes.
	certReportEvery = time.Hour
)

// errorObs is one observed change of an error group.
type errorObs struct {
	seq                                       int64
	at                                        time.Time
	component, instance, fp, kind, normalized string
	delta                                     int64
	first, last                               time.Time
	samples                                   []string
}

// errorCollector turns the cumulative totals the components publish into deltas.
type errorCollector struct {
	mu      sync.Mutex
	seq     int64
	ring    []errorObs
	last    map[string]int64 // component|instance|fingerprint -> the total last seen
	started time.Time
	now     func() time.Time
}

func newErrorCollector() *errorCollector {
	return &errorCollector{last: map[string]int64{}, started: time.Now(), now: time.Now}
}

// Observe takes one published group. The first time a group is seen its total is only the baseline, unless the group began after
// the agent did (then it is all new); a total that went down means the component restarted and counts again from zero.
func (c *errorCollector) Observe(o errorlog.Observation) {
	key := o.Component + "|" + o.Instance + "|" + o.Fingerprint
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, known := c.last[key]
	c.last[key] = o.Total
	var delta int64
	switch {
	case !known && o.First.Before(c.started.Add(-time.Minute)):
		return // a group older than this agent: its history is not ours, only the changes from now on
	case !known, o.Total < prev:
		delta = o.Total
	default:
		delta = o.Total - prev
	}
	if delta <= 0 {
		return
	}
	c.seq++
	c.ring = append(c.ring, errorObs{seq: c.seq, at: c.now(), component: o.Component, instance: o.Instance, fp: o.Fingerprint, kind: o.Kind,
		normalized: o.Normalized, delta: delta, first: o.First, last: o.Last, samples: o.Samples})
	if len(c.ring) > errorRing {
		c.ring = c.ring[len(c.ring)-errorRing:]
	}
}

// Cursor is the position after the newest observation.
func (c *errorCollector) Cursor() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq
}

// CursorAt is the position just before the first observation made at or after t.
func (c *errorCollector) CursorAt(t time.Time) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, o := range c.ring {
		if !o.at.Before(t) {
			return o.seq - 1
		}
	}
	return c.seq
}

// Since returns the observations after the cursor, merged per component instance and fingerprint, and the new cursor.
func (c *errorCollector) Since(cursor int64) ([]*protobuf.ComponentErrors, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	type key struct{ comp, inst, fp string }
	groups := map[key]*protobuf.ErrorGroup{}
	for _, o := range c.ring {
		if o.seq <= cursor {
			continue
		}
		k := key{o.component, o.instance, o.fp}
		g := groups[k]
		if g == nil {
			g = &protobuf.ErrorGroup{Fingerprint: o.fp, Kind: o.kind, Normalized: o.normalized, FirstUnixMs: o.first.UnixMilli()}
			groups[k] = g
		}
		g.Count += o.delta
		g.LastUnixMs = o.last.UnixMilli()
		g.Samples = o.samples
	}
	byComponent := map[[2]string]*protobuf.ComponentErrors{}
	for k, g := range groups {
		ck := [2]string{k.comp, k.inst}
		ce := byComponent[ck]
		if ce == nil {
			ce = &protobuf.ComponentErrors{Component: k.comp, Instance: k.inst}
			byComponent[ck] = ce
		}
		ce.Groups = append(ce.Groups, g)
	}
	out := make([]*protobuf.ComponentErrors, 0, len(byComponent))
	for _, ce := range byComponent {
		sort.Slice(ce.Groups, func(i, j int) bool { return ce.Groups[i].Fingerprint < ce.Groups[j].Fingerprint })
		out = append(out, ce)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Component != out[j].Component {
			return out[i].Component < out[j].Component
		}
		return out[i].Instance < out[j].Instance
	})
	return out, c.seq
}

// ErrorJournalConfig tells the agent where the components' events are.
type ErrorJournalConfig struct {
	// ReleaseNamespace holds the events of the operator, node-agents and proxy; the group namespaces hold those of the VPN and
	// gateway pods. Empty: only the group namespaces are read.
	ReleaseNamespace string
	// Interval is how often the release namespace is read; GroupInterval how often the group namespaces are (one list each).
	Interval, GroupInterval time.Duration
	// Self is the agent's own aggregator (its logger feeds it); nil when it has none.
	Self *errorlog.Aggregator
}

// SetErrorJournal configures the journal; call before Run.
func (h *Handler) SetErrorJournal(c ErrorJournalConfig) {
	h.errJournal = c
	if h.errors == nil {
		h.errors = newErrorCollector()
	}
}

// RunErrorJournal reads the components' events (and the agent's own errors) until ctx ends.
func (h *Handler) RunErrorJournal(ctx context.Context) {
	if h.errors == nil {
		h.errors = newErrorCollector()
	}
	interval, groupInterval := h.errJournal.Interval, h.errJournal.GroupInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if groupInterval <= 0 {
		groupInterval = time.Minute
	}
	var lastGroups time.Time
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		h.pollErrors(ctx, time.Since(lastGroups) >= groupInterval)
		if time.Since(lastGroups) >= groupInterval {
			lastGroups = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pollErrors reads one round: the agent's own groups, the release namespace and, when groups is set, every group namespace.
func (h *Handler) pollErrors(ctx context.Context, groups bool) {
	if self := h.errJournal.Self; self != nil {
		for _, g := range self.Dirty() {
			h.errors.Observe(errorlog.Observation{Component: self.Component, Instance: self.Instance, Fingerprint: g.Fingerprint, Kind: g.Kind,
				Normalized: g.Normalized, Total: g.Total, First: g.First, Last: g.Last, Samples: g.Samples})
		}
	}
	if h.k8s == nil {
		return
	}
	read := func(ns string) {
		evs, err := h.k8s.CoreV1().Events(ns).List(ctx, metav1.ListOptions{LabelSelector: errorlog.Label + "=true"})
		if err != nil {
			return
		}
		for i := range evs.Items {
			if o, ok := errorlog.ParseEvent(&evs.Items[i]); ok {
				h.errors.Observe(o)
			}
		}
	}
	if ns := h.errJournal.ReleaseNamespace; ns != "" {
		read(ns)
	}
	if !groups {
		return
	}
	list, err := h.cs.LaboratoryV1alpha1().LabGroups().List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	for i := range list.Items {
		ns := list.Items[i].Status.Namespace
		if ns == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			read(ns)
		}()
	}
	wg.Wait()
}

// errorStream is what one Monitoring stream remembers of the journal.
type errorStream struct {
	cursor     int64
	windowFrom time.Time
	started    bool
	// failed is what was reported as failed: device key -> reason, so a failure is sent once and again only when it changes.
	failed map[string]string
	// groupOf maps a group namespace to the group's id, learned from the groups the stream has seen.
	groupOf   map[string]string
	certSent  time.Time
	certValue int64
}

func newErrorStream() *errorStream {
	return &errorStream{failed: map[string]string{}, groupOf: map[string]string{}}
}

// fill adds the journal to a Monitoring update: the deploy failures of the tenant's own labs, the laboratory's components for a
// tenant that may see them, and the certificate expiry. It sets nothing when there is nothing to say.
func (h *Handler) fillErrors(ctx context.Context, st *errorStream, u *protobuf.MonitoringUpdate) {
	now := time.Now()
	j := &protobuf.ErrorJournal{WindowEndUnixMs: now.UnixMilli()}
	if !st.started {
		st.windowFrom = now.Add(-errorFirstWindow)
		if h.errors != nil {
			st.cursor = h.errors.CursorAt(st.windowFrom)
		}
	}
	j.WindowStartUnixMs = st.windowFrom.UnixMilli()

	for _, g := range u.GetGroups() {
		if ns := g.GetStatus().GetNamespace(); ns != "" {
			st.groupOf[ns] = g.GetName()
		}
	}
	j.DeployFailures = st.newFailures(u, now)

	if h.errors != nil {
		if h.mayReadComponentErrors(ctx) {
			j.Components, st.cursor = h.errors.Since(st.cursor)
		} else {
			st.cursor = h.errors.Cursor()
		}
	}
	// the certificate expiry: with the first message, when it changes, and about hourly
	if exp := callerCertNotAfter(ctx); exp != 0 && (!st.started || exp != st.certValue || now.Sub(st.certSent) >= certReportEvery) {
		j.ClientCertNotAfterUnix = exp
		st.certValue, st.certSent = exp, now
	}
	st.started = true
	st.windowFrom = now
	if len(j.Components) == 0 && len(j.DeployFailures) == 0 && j.ClientCertNotAfterUnix == 0 {
		return
	}
	u.Errors = j
}

// mayReadComponentErrors is whether the caller's tenant is one that may see the laboratory's own errors.
func (h *Handler) mayReadComponentErrors(ctx context.Context) bool {
	ten, err := h.tenantObject(ctx, tenantOf(ctx))
	return err == nil && ten != nil && ten.Spec.ReceivesLabErrors
}

// newFailures derives the failed deploys in an update: a device or group pod whose scheduling says it failed, and a lab that is
// Failed or in Error without a device to blame. A failure is reported once, and again only if its reason changes.
func (st *errorStream) newFailures(u *protobuf.MonitoringUpdate, now time.Time) []*protobuf.DeployFailure {
	var out []*protobuf.DeployFailure
	report := func(key, group, lab, device, reason, msg string, at int64) {
		if st.failed[key] == reason {
			return
		}
		st.failed[key] = reason
		if at == 0 {
			at = now.UnixMilli()
		}
		out = append(out, &protobuf.DeployFailure{LabGroup: group, Lab: lab, Device: device, ReasonCode: reason, Message: errorlog.Redact(msg), AtUnixMs: at})
	}
	clear := func(key string) { delete(st.failed, key) }
	for _, g := range u.GetGroups() {
		for _, p := range g.GetStatus().GetPods() {
			key := "group|" + g.GetName() + "|" + p.GetName()
			if f := p.GetScheduling().GetFailure(); f != nil {
				report(key, g.GetName(), "", p.GetName(), f.GetReason(), f.GetMessage(), f.GetAtUnixMs())
			} else {
				clear(key)
			}
		}
	}
	for _, l := range u.GetLabs() {
		group := l.GetLabGroupName()
		if group == "" {
			group = st.groupOf[l.GetNamespace()]
		}
		blamed := false
		for _, d := range l.GetStatus().GetDevices() {
			key := "lab|" + l.GetNamespace() + "|" + l.GetName() + "|" + d.GetName()
			if f := d.GetScheduling().GetFailure(); f != nil {
				blamed = true
				report(key, group, l.GetName(), d.GetName(), f.GetReason(), f.GetMessage(), f.GetAtUnixMs())
			} else {
				clear(key)
			}
		}
		key := "lab|" + l.GetNamespace() + "|" + l.GetName()
		switch phase := l.GetStatus().GetPhase(); {
		case blamed:
			clear(key)
		case phase == string(laboratoryv1alpha1.PhaseFailed):
			report(key, group, l.GetName(), "", "LabFailed", l.GetStatus().GetImageWarning(), 0)
		case phase == string(laboratoryv1alpha1.PhaseError):
			report(key, group, l.GetName(), "", "LabError", l.GetStatus().GetImageWarning(), 0)
		default:
			clear(key)
		}
	}
	return out
}

// errorsDue says whether an update carrying the journal should go out now although no record changed: the components have new
// errors this stream's tenant may see, or the certificate expiry is due again.
func (h *Handler) errorsDue(ctx context.Context, st *errorStream) bool {
	if !st.started {
		return false // the first message is made by the stream itself
	}
	if h.errors != nil && h.errors.Cursor() > st.cursor && h.mayReadComponentErrors(ctx) {
		return true
	}
	return callerCertNotAfter(ctx) != 0 && time.Since(st.certSent) >= certReportEvery
}
