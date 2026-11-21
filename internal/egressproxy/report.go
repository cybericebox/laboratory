package egressproxy

import (
	"context"
	"sort"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// The bounds of a LabEgressReport. The CRD carries the same numbers as maxItems (a test compares them): an object over them is refused
// by the API server, so the writer keeps below them.
const (
	MaxReportMinutes        = 30
	MaxReportRows           = 500
	MaxReportCaps           = 500
	MaxReportDenials        = 500
	MaxReportFlows          = 200
	MaxReportRowsAllMinutes = 4000

	// ReportEvery is the least time between two writes of the report.
	ReportEvery = 20 * time.Second
)

// ReportMinute converts a finished minute to the shape of the report and applies the per-minute bounds: when a list is over its bound the
// busiest entries are kept (rows by connections, then bytes), so what is cut is the least informative.
func ReportMinute(m Minute) laboratoryv1alpha1.LabEgressMinute {
	out := laboratoryv1alpha1.LabEgressMinute{Start: metav1.NewTime(m.Start.UTC())}
	rows := append([]Row(nil), m.Rows...)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Conns != b.Conns {
			return a.Conns > b.Conns
		}
		return a.Up+a.Down > b.Up+b.Down
	})
	if len(rows) > MaxReportRows {
		rows = rows[:MaxReportRows]
	}
	for _, r := range rows {
		out.Rows = append(out.Rows, laboratoryv1alpha1.LabEgressRow{Lab: r.Lab, Host: r.Host, Conns: r.Conns, Up: r.Up, Down: r.Down, Denied: r.Denied})
	}
	caps := append([]CapHit(nil), m.Caps...)
	sort.SliceStable(caps, func(i, j int) bool { return caps[i].Hits > caps[j].Hits })
	if len(caps) > MaxReportCaps {
		caps = caps[:MaxReportCaps]
	}
	for _, c := range caps {
		out.Caps = append(out.Caps, laboratoryv1alpha1.LabEgressCap{Lab: c.Lab, Reason: c.Reason, Hits: c.Hits})
	}
	denials := append([]Denial(nil), m.Denials...)
	sort.SliceStable(denials, func(i, j int) bool { return denials[i].Count > denials[j].Count })
	if len(denials) > MaxReportDenials {
		denials = denials[:MaxReportDenials]
	}
	for _, d := range denials {
		out.Denials = append(out.Denials, laboratoryv1alpha1.LabEgressDenial{Lab: d.Lab, Reason: d.Reason, Count: d.Count})
	}
	flows := append([]FlowRow(nil), m.Flows...)
	sort.SliceStable(flows, func(i, j int) bool { return flows[i].Up+flows[i].Down > flows[j].Up+flows[j].Down })
	if len(flows) > MaxReportFlows {
		flows = flows[:MaxReportFlows]
	}
	for _, f := range flows {
		out.Flows = append(out.Flows, laboratoryv1alpha1.LabEgressFlow{Lab: f.Lab, Source: f.Source, Dest: f.Dest, Port: int32(f.Port), Conns: f.Conns, Up: f.Up, Down: f.Down, FirstMs: f.FirstMs, LastMs: f.LastMs})
	}
	return out
}

// SummariesOfReport is the report as the Summaries the operator has always read, so the same limits (accept) apply to it.
func SummariesOfReport(r *laboratoryv1alpha1.LabEgressReport) *Summaries {
	s := &Summaries{Gateway: r.Status.Instance}
	for _, m := range r.Status.Minutes {
		out := Minute{Start: m.Start.UTC()}
		for _, x := range m.Rows {
			out.Rows = append(out.Rows, Row{Lab: x.Lab, Host: x.Host, Conns: x.Conns, Up: x.Up, Down: x.Down, Denied: x.Denied})
		}
		for _, x := range m.Caps {
			out.Caps = append(out.Caps, CapHit{Lab: x.Lab, Reason: x.Reason, Hits: x.Hits})
		}
		for _, x := range m.Denials {
			out.Denials = append(out.Denials, Denial{Lab: x.Lab, Reason: x.Reason, Count: x.Count})
		}
		for _, x := range m.Flows {
			out.Flows = append(out.Flows, FlowRow{Lab: x.Lab, Source: x.Source, Dest: x.Dest, Port: int(x.Port), Conns: x.Conns, Up: x.Up, Down: x.Down, FirstMs: x.FirstMs, LastMs: x.LastMs})
		}
		s.Minutes = append(s.Minutes, out)
	}
	return s
}

func reportRows(m laboratoryv1alpha1.LabEgressMinute) int {
	return len(m.Rows) + len(m.Caps) + len(m.Denials) + len(m.Flows)
}

// ReportWriter keeps the LabEgressReport of the gateway's namespace: it takes the finished minutes of the Collector, keeps the ones the
// operator has not acknowledged (spec.ackedThrough), and writes them to the status, never more often than Every and only when something
// changed. It is the only writer of the status. Minutes of a previous run of the pod that are still in the status are kept.
type ReportWriter struct {
	// Reader reads without a cache (the gateway's role has no list or watch); Writer creates the object and patches its status.
	Reader    client.Reader
	Writer    client.Client
	Namespace string
	Instance  string
	BootID    string
	Collector *Collector
	Every     time.Duration
	Now       func() time.Time
	Log       logr.Logger

	loaded  bool
	kept    []laboratoryv1alpha1.LabEgressMinute
	taken   time.Time // start of the newest minute that is in kept (or was, before it was acknowledged)
	dropped int64
}

func (w *ReportWriter) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Run syncs every Every until ctx ends.
func (w *ReportWriter) Run(ctx context.Context) {
	every := w.Every
	if every <= 0 {
		every = ReportEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := w.Sync(ctx); err != nil {
			w.Log.Error(err, "write the egress report")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sync is one step: read the report, take the new minutes, drop the acknowledged ones, apply the bounds, write when the minutes differ from
// what the status holds.
func (w *ReportWriter) Sync(ctx context.Context) error {
	var cur laboratoryv1alpha1.LabEgressReport
	key := client.ObjectKey{Namespace: w.Namespace, Name: laboratoryv1alpha1.LabEgressReportName}
	exists := true
	if err := w.Reader.Get(ctx, key, &cur); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		exists = false
	}
	if !w.loaded {
		if exists {
			w.kept = append(w.kept, cur.Status.Minutes...)
			w.dropped = cur.Status.DroppedMinutes
			for _, m := range w.kept {
				if m.Start.After(w.taken) {
					w.taken = m.Start.Time
				}
			}
		}
		w.loaded = true
	}
	var acked time.Time
	if exists && cur.Spec.AckedThrough != nil {
		acked = cur.Spec.AckedThrough.Time
	}
	// the minutes the Collector finished since the last one taken
	from := w.taken
	if acked.After(from) {
		from = acked
	}
	for _, m := range w.Collector.Since(from).Minutes {
		if m.Start.After(w.taken) {
			w.taken = m.Start
		}
		w.kept = append(w.kept, ReportMinute(m))
	}
	// forget what the operator has logged
	live := w.kept[:0]
	for _, m := range w.kept {
		if m.Start.After(acked) {
			live = append(live, m)
		}
	}
	w.kept = live
	// the bounds: the oldest minute goes first
	rows := 0
	for _, m := range w.kept {
		rows += reportRows(m)
	}
	for len(w.kept) > 0 && (len(w.kept) > MaxReportMinutes || rows > MaxReportRowsAllMinutes) {
		rows -= reportRows(w.kept[0])
		w.kept = w.kept[1:]
		w.dropped++
	}
	if !w.changed(&cur, exists) {
		return nil
	}
	if !exists {
		cur = laboratoryv1alpha1.LabEgressReport{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
		if err := w.Writer.Create(ctx, &cur); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	base := cur.DeepCopy()
	now := metav1.NewTime(w.now().UTC())
	cur.Status.Instance, cur.Status.BootID = w.Instance, w.BootID
	cur.Status.Minutes = append([]laboratoryv1alpha1.LabEgressMinute(nil), w.kept...)
	cur.Status.DroppedMinutes = w.dropped
	cur.Status.UpdatedAt = &now
	return w.Writer.Status().Patch(ctx, &cur, client.MergeFrom(base))
}

// changed is true when the status does not hold the minutes the writer keeps.
func (w *ReportWriter) changed(cur *laboratoryv1alpha1.LabEgressReport, exists bool) bool {
	if !exists {
		return len(w.kept) > 0
	}
	if cur.Status.DroppedMinutes != w.dropped || len(cur.Status.Minutes) != len(w.kept) {
		return true
	}
	for i := range w.kept {
		if !cur.Status.Minutes[i].Start.Equal(&w.kept[i].Start) {
			return true
		}
	}
	return false
}
