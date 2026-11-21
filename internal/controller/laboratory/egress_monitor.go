package laboratory

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/activity"
	"github.com/cybericebox/laboratory/internal/egressproxy"
)

// ConditionEgressAttention is on a Lab that the egress monitor flagged: many labs of the cluster hit one host name, or the lab sits at its
// caps. It is a hint for an administrator (the admin page «Потребує уваги» reads it through the daemon), never a block.
const ConditionEgressAttention = "EgressAttention"

var (
	egressLabsFlagged = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "laboratory_egress_labs_flagged",
		Help: "Labs that carry the EgressAttention condition now.",
	})
	egressMinutesPulled = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "laboratory_egress_minutes_pulled_total",
		Help: "Minutes of egress summaries pulled from the gateways.",
	})
	egressPullFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "laboratory_egress_pull_failures_total",
		Help: "Pulls of the egress summaries of a gateway that failed.",
	})
)

func init() {
	metrics.Registry.MustRegister(egressLabsFlagged, egressMinutesPulled, egressPullFailures)
}

// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labactivityreports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=laboratory.cybericebox.com,resources=labactivityreports/status,verbs=get;update;patch

// EgressLogRow is one line of the connection log: a lab's traffic to one host name in one minute. No content and no address of a person.
type EgressLogRow struct {
	Minute    time.Time `json:"minute"`
	Namespace string    `json:"namespace"`
	Lab       string    `json:"lab"`
	Host      string    `json:"host"`
	Conns     int64     `json:"conns"`
	Up        int64     `json:"up"`
	Down      int64     `json:"down"`
	Denied    int64     `json:"denied,omitempty"`
}

// EgressMonitor reads the per-minute summaries that the gateway of every group writes to its LabEgressReport, keeps the connection log,
// acknowledges what it has logged (spec.ackedThrough, so the gateway forgets it), and flags the labs that need an administrator's eyes: a
// host name that many labs hit, a lab that sits at its caps. It never talks to a gateway pod: it reads custom resources and writes Events,
// one condition on a Lab and the acknowledgement.
type EgressMonitor struct {
	Client   client.Client
	Reader   client.Reader
	Recorder record.EventRecorder
	Log      logr.Logger

	// Interval is the time between pulls (default 1 minute); the acknowledgement is written once per pull.
	Interval time.Duration
	// LogDir, when set, gets one file per day (egress-YYYY-MM-DD.jsonl); files older than LogRetention (default 30 days) are removed. The log
	// is always written to the operator's own log as well (logger "egress-log").
	LogDir       string
	LogRetention time.Duration
	// HostLabsThreshold is how many different labs hitting one host name within Window make it a signal (default 20); CapMinutes how many of
	// the minutes of the Window a lab must sit at a cap (default 3); Window defaults to 10 minutes.
	HostLabsThreshold int
	CapMinutes        int
	Window            time.Duration
	Now               func() time.Time
	// Activity receives, as raw facts, the source/destination/port rows the gateways report when activity reports are on (nil: off).
	Activity *activity.Hub

	mu      sync.Mutex
	last    map[string]time.Time // group namespace -> start of the newest minute logged
	minutes []monitorMinute
	flagged map[labKey]string // labs that carry the condition, with its message
	primed  bool
	cursors map[string]time.Time // namespace -> newest minute already in the log when this run started
	seen    string               // what the last pull saw (reports, skipped ones); logged when it changes
}

type labKey struct{ ns, lab string }

type monitorMinute struct {
	start time.Time
	hosts map[string]map[labKey]bool
	caps  map[labKey]bool
}

func (m *EgressMonitor) NeedLeaderElection() bool { return true }

func (m *EgressMonitor) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Start pulls every Interval until ctx ends.
func (m *EgressMonitor) Start(ctx context.Context) error {
	every := m.Interval
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := m.Pull(ctx); err != nil {
			m.Log.Error(err, "pull the egress summaries")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Limits on what a gateway may tell the operator: the operator believes only what a report of its own groups says, and only so much of it.
const (
	maxMinutesPerPull = 60
	maxRowsPerMinute  = 5000
	maxNameLen        = 253
	maxKeptMinutes    = 200
	summaryMaxAge     = time.Hour
	summaryMaxFuture  = time.Minute
)

// noteSeen logs, once per change, how many reports the monitor reads and why the other ones were passed over, so a monitor that sees
// nothing says so.
func (m *EgressMonitor) noteSeen(found int, skipped map[string]int) {
	keys := make([]string, 0, len(skipped))
	for k := range skipped {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	state := fmt.Sprintf("reports=%d", found)
	for _, k := range keys {
		state += fmt.Sprintf(" %s=%d", k, skipped[k])
	}
	m.mu.Lock()
	changed := state != m.seen
	m.seen = state
	m.mu.Unlock()
	if changed {
		m.Log.Info("egress monitor sees", "reports", found, "skipped", skipped)
	}
}

// accept keeps the minutes of a pull that can be true: after what was asked for, not from the future, not older than an hour, at most
// maxMinutesPerPull of them, with at most maxRowsPerMinute rows each and names of a sane length.
func accept(s *egressproxy.Summaries, since, now time.Time) []egressproxy.Minute {
	var out []egressproxy.Minute
	for _, min := range s.Minutes {
		if len(out) >= maxMinutesPerPull {
			break
		}
		if !min.Start.After(since) || min.Start.After(now.Add(summaryMaxFuture)) || min.Start.Before(now.Add(-summaryMaxAge)) {
			continue
		}
		if len(min.Rows) > maxRowsPerMinute {
			min.Rows = min.Rows[:maxRowsPerMinute]
		}
		if len(min.Caps) > maxRowsPerMinute {
			min.Caps = min.Caps[:maxRowsPerMinute]
		}
		if len(min.Denials) > maxRowsPerMinute {
			min.Denials = min.Denials[:maxRowsPerMinute]
		}
		if len(min.Flows) > egressproxy.MaxFlowRows {
			min.Flows = min.Flows[:egressproxy.MaxFlowRows]
		}
		flows := min.Flows[:0:0]
		for _, f := range min.Flows {
			if len(f.Lab) > maxNameLen || len(f.Source) > 45 || len(f.Dest) > 45 || f.Conns < 0 || f.Up < 0 || f.Down < 0 || f.Port < 0 || f.Port > 65535 {
				continue
			}
			flows = append(flows, f)
		}
		min.Flows = flows
		rows := min.Rows[:0:0]
		for _, r := range min.Rows {
			if len(r.Lab) > maxNameLen || len(r.Host) > maxNameLen || r.Conns < 0 || r.Up < 0 || r.Down < 0 {
				continue
			}
			rows = append(rows, r)
		}
		min.Rows = rows
		caps := min.Caps[:0:0]
		for _, c := range min.Caps {
			if len(c.Lab) <= maxNameLen {
				caps = append(caps, c)
			}
		}
		min.Caps = caps
		out = append(out, min)
	}
	return out
}

// firstSince is where reading a group starts when this run has not logged any minute of it: after the newest minute of its group that the
// log already holds (a restarted operator takes up the minutes it missed; the second result says the log was the source), else the last
// hour. The acknowledgement in the report moves it further when it is newer.
func (m *EgressMonitor) firstSince(ns string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cursors == nil {
		m.cursors = map[string]time.Time{}
		if m.LogDir != "" {
			cur, err := egressLogCursors(m.LogDir, m.now())
			if err != nil {
				m.Log.Error(err, "read where the egress connection log ends")
			}
			if cur != nil {
				m.cursors = cur
			}
		}
	}
	if t, ok := m.cursors[ns]; ok {
		return t, true
	}
	return m.now().Add(-time.Hour), false
}

// Pull reads the new minutes of every group's report, writes the log, acknowledges what is logged, and re-evaluates the signals.
func (m *EgressMonitor) Pull(ctx context.Context) error {
	reports, skipped, err := m.groupReports(ctx)
	if err != nil {
		return err
	}
	m.noteSeen(len(reports), skipped)
	m.mu.Lock()
	if m.last == nil {
		m.last = map[string]time.Time{}
	}
	m.mu.Unlock()
	var rows []EgressLogRow
	var pending []ackTarget
	before := map[string]*time.Time{} // where each group stood before this pull, to go back when the log cannot be written
	for i := range reports {
		r := &reports[i]
		ns := r.Namespace
		m.mu.Lock()
		since, ok := m.last[ns]
		m.mu.Unlock()
		if ok {
			prev := since
			before[ns] = &prev
		} else {
			before[ns] = nil
			var fromLog bool
			since, fromLog = m.firstSince(ns)
			if r.Spec.AckedThrough != nil && r.Spec.AckedThrough.After(since) {
				since, fromLog = r.Spec.AckedThrough.Time, true
			}
			if fromLog {
				// what the log or the acknowledgement already holds is logged: it may be acknowledged (a crash between the two)
				m.mu.Lock()
				m.last[ns] = since
				m.mu.Unlock()
			}
		}
		for _, min := range accept(egressproxy.SummariesOfReport(r), since, m.now()) {
			egressMinutesPulled.Inc()
			mm := monitorMinute{start: min.Start, hosts: map[string]map[labKey]bool{}, caps: map[labKey]bool{}}
			for _, row := range min.Rows {
				rows = append(rows, EgressLogRow{Minute: min.Start, Namespace: ns, Lab: row.Lab, Host: row.Host, Conns: row.Conns, Up: row.Up, Down: row.Down, Denied: row.Denied})
				if row.Conns > 0 {
					if mm.hosts[row.Host] == nil {
						mm.hosts[row.Host] = map[labKey]bool{}
					}
					mm.hosts[row.Host][labKey{ns, row.Lab}] = true
				}
			}
			for _, c := range min.Caps {
				mm.caps[labKey{ns, c.Lab}] = true
			}
			for _, f := range min.Flows {
				if m.Activity == nil {
					break
				}
				m.Activity.Add(ns, laboratoryv1alpha1.ActivityEvent{
					AtMs: min.Start.Add(time.Minute).UnixMilli(), Kind: laboratoryv1alpha1.ActivityEgress, LabName: f.Lab, SourceAddr: f.Source, DstAddr: f.Dest,
					DstPort: int32(f.Port), Protocol: "tcp", BytesUp: f.Up, BytesDown: f.Down, Connections: f.Conns, FirstSeenMs: f.FirstMs, LastSeenMs: f.LastMs,
				})
			}
			m.mu.Lock()
			m.minutes = append(m.minutes, mm)
			if len(m.minutes) > maxKeptMinutes {
				m.minutes = m.minutes[len(m.minutes)-maxKeptMinutes:]
			}
			if min.Start.After(m.last[ns]) {
				m.last[ns] = min.Start
			}
			m.mu.Unlock()
		}
		m.mu.Lock()
		pending = append(pending, ackTarget{report: r, through: m.last[ns]})
		m.mu.Unlock()
	}
	// forget the groups that are gone
	m.mu.Lock()
	alive := map[string]bool{}
	for i := range reports {
		alive[reports[i].Namespace] = true
	}
	for k := range m.last {
		if !alive[k] {
			delete(m.last, k)
		}
	}
	m.mu.Unlock()
	// The log first, the acknowledgement after it: the gateway forgets a minute only once the operator has written it down. When the log
	// cannot be written nothing is acknowledged, and the gateway keeps the minutes for the next pull.
	if err := m.writeLog(rows); err != nil {
		m.Log.Error(err, "write the egress connection log")
		// nothing of this pull is logged: read it again next time (the report still holds it) and acknowledge nothing
		m.mu.Lock()
		for ns, prev := range before {
			if prev == nil {
				delete(m.last, ns)
			} else {
				m.last[ns] = *prev
			}
		}
		m.mu.Unlock()
	} else {
		for _, a := range pending {
			if err := m.acknowledge(ctx, a.report, a.through); err != nil {
				egressPullFailures.Inc()
				m.Log.Info("cannot acknowledge the egress report", "namespace", a.report.Namespace, "error", err.Error())
			}
		}
	}
	return m.evaluate(ctx)
}

func (m *EgressMonitor) writeLog(rows []EgressLogRow) error {
	if m.LogDir == "" {
		// without a directory the operator's own log is the connection log
		log := m.Log.WithName("egress-log")
		for _, r := range rows {
			log.Info("egress", "minute", r.Minute.Format(time.RFC3339), "namespace", r.Namespace, "lab", r.Lab, "host", r.Host,
				"conns", r.Conns, "up", r.Up, "down", r.Down, "denied", r.Denied)
		}
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	if err := os.MkdirAll(m.LogDir, 0o750); err != nil {
		return err
	}
	byDay := map[string][]EgressLogRow{}
	for _, r := range rows {
		d := r.Minute.UTC().Format("2006-01-02")
		byDay[d] = append(byDay[d], r)
	}
	for day, list := range byDay {
		f, err := os.OpenFile(filepath.Join(m.LogDir, "egress-"+day+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
		if err != nil {
			return err
		}
		w := bufio.NewWriter(f)
		enc := json.NewEncoder(w)
		for _, r := range list {
			if err := enc.Encode(r); err != nil {
				_ = f.Close()
				return err
			}
		}
		if err := w.Flush(); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return m.pruneLog()
}

// pruneLog gzips the files of the days that are over and removes the ones older than the retention.
func (m *EgressMonitor) pruneLog() error {
	keep := m.LogRetention
	if keep <= 0 {
		keep = 30 * 24 * time.Hour
	}
	if err := compressEgressLogDays(m.LogDir, m.now()); err != nil {
		return err
	}
	return removeEgressLogDaysBefore(m.LogDir, m.now().UTC().Add(-keep).Format(egressLogDay))
}

// signals are the labs to flag, with the reason text, from the minutes of the window.
func (m *EgressMonitor) signals() map[labKey]string {
	win := m.Window
	if win <= 0 {
		win = 10 * time.Minute
	}
	hostThreshold := m.HostLabsThreshold
	if hostThreshold <= 0 {
		hostThreshold = 20
	}
	capMinutes := m.CapMinutes
	if capMinutes <= 0 {
		capMinutes = 3
	}
	cutoff := m.now().Add(-win)
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.minutes[:0]
	for _, mm := range m.minutes {
		if mm.start.After(cutoff) {
			kept = append(kept, mm)
		}
	}
	m.minutes = kept
	hosts := map[string]map[labKey]bool{}
	capCount := map[labKey]int{}
	for _, mm := range kept {
		for h, labs := range mm.hosts {
			if hosts[h] == nil {
				hosts[h] = map[labKey]bool{}
			}
			for l := range labs {
				hosts[h][l] = true
			}
		}
		for l := range mm.caps {
			capCount[l]++
		}
	}
	out := map[labKey]string{}
	var hot []string
	for h, labs := range hosts {
		if len(labs) >= hostThreshold {
			hot = append(hot, h)
		}
	}
	sort.Strings(hot)
	for _, h := range hot {
		for l := range hosts[h] {
			if _, seen := out[l]; !seen {
				out[l] = fmt.Sprintf("%d labs of the cluster reached %s within %s", len(hosts[h]), h, win)
			}
		}
	}
	for l, n := range capCount {
		if n >= capMinutes {
			if prev, ok := out[l]; ok {
				out[l] = prev + "; and the lab sat at its egress caps"
			} else {
				out[l] = fmt.Sprintf("the lab sat at its egress caps in %d of the last %s", n, win)
			}
		}
	}
	return out
}

// evaluate sets the condition on the labs the signals name and takes it off the ones that are clear now.
func (m *EgressMonitor) evaluate(ctx context.Context) error {
	if err := m.prime(ctx); err != nil {
		return err
	}
	want := m.signals()
	m.mu.Lock()
	have := map[labKey]string{}
	for k, v := range m.flagged {
		have[k] = v
	}
	m.mu.Unlock()
	var first error
	for k, msg := range want {
		if have[k] == msg {
			continue
		}
		if err := m.setCondition(ctx, k, true, msg); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if m.Recorder != nil {
			m.Recorder.Event(&laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: k.lab, Namespace: k.ns}}, corev1.EventTypeWarning, "EgressAttention", msg)
		}
		m.mu.Lock()
		m.flagged[k] = msg
		m.mu.Unlock()
	}
	for k := range have {
		if _, still := want[k]; still {
			continue
		}
		if err := m.setCondition(ctx, k, false, "no longer"); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		m.mu.Lock()
		delete(m.flagged, k)
		m.mu.Unlock()
	}
	m.mu.Lock()
	egressLabsFlagged.Set(float64(len(m.flagged)))
	m.mu.Unlock()
	return first
}

// prime learns, once, which labs already carry the condition (an operator that restarted must still be able to take it off).
func (m *EgressMonitor) prime(ctx context.Context) error {
	m.mu.Lock()
	done := m.primed
	m.mu.Unlock()
	if done {
		return nil
	}
	var labs laboratoryv1alpha1.LabList
	if err := m.Reader.List(ctx, &labs); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flagged = map[labKey]string{}
	for i := range labs.Items {
		if c := meta.FindStatusCondition(labs.Items[i].Status.Conditions, ConditionEgressAttention); c != nil && c.Status == metav1.ConditionTrue {
			m.flagged[labKey{labs.Items[i].Namespace, labs.Items[i].Name}] = c.Message
		}
	}
	m.primed = true
	return nil
}

func (m *EgressMonitor) setCondition(ctx context.Context, k labKey, on bool, msg string) error {
	var l laboratoryv1alpha1.Lab
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: k.ns, Name: k.lab}, &l); err != nil {
		return client.IgnoreNotFound(err)
	}
	base := l.DeepCopy()
	if on {
		meta.SetStatusCondition(&l.Status.Conditions, metav1.Condition{Type: ConditionEgressAttention, Status: metav1.ConditionTrue, Reason: "EgressSignal", Message: msg, ObservedGeneration: l.Generation})
	} else if !meta.RemoveStatusCondition(&l.Status.Conditions, ConditionEgressAttention) {
		return nil
	}
	return m.Client.Status().Patch(ctx, &l, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
