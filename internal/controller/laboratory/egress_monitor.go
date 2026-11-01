package laboratory

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/egressproxy"
	"github.com/cybericebox/laboratory/internal/names"
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

// EgressMonitor pulls the per-minute summaries of every gateway pod, keeps the connection log, and flags the labs that need an
// administrator's eyes: a host name that many labs hit, a lab that sits at its caps. It only reads the gateways (plain HTTP on the pod
// network, a port the network policy opens to the operator) and writes Events and one condition on a Lab.
type EgressMonitor struct {
	Client   client.Client
	Reader   client.Reader
	Recorder record.EventRecorder
	Log      logr.Logger

	// Interval is the time between pulls (default 1 minute); Port the port of the summaries page of a gateway (default 9188).
	Interval time.Duration
	Port     int
	HTTP     *http.Client
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

	mu      sync.Mutex
	last    map[string]time.Time // gateway pod UID -> start of the last minute taken
	minutes []monitorMinute
	flagged map[labKey]string // labs that carry the condition, with its message
	primed  bool
	cursors map[string]time.Time // namespace -> newest minute already in the log when this run started
	seen    string               // what the last pull saw (gateways, skipped pods); logged when it changes
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

// Limits on what a gateway may tell the operator: the operator believes only what a pod of its own says, and only so much of it.
const (
	maxSummaryBody    = 4 << 20
	maxMinutesPerPull = 60
	maxRowsPerMinute  = 5000
	maxNameLen        = 253
	maxKeptMinutes    = 200
	summaryMaxAge     = time.Hour
	summaryMaxFuture  = time.Minute
	gatewayDeployment = names.ComponentGateway
)

// gatewayPods are the running pods of the gateway Deployments that the operator itself made, in the namespaces of its LabGroups: a pod that
// carries the label but is not owned (through its ReplicaSet) by the Deployment "gateway" in a namespace labelled as a group's is not asked.
func (m *EgressMonitor) gatewayPods(ctx context.Context) ([]corev1.Pod, error) {
	pods, skipped, err := m.selectGatewayPods(ctx)
	m.noteSeen(len(pods), skipped)
	return pods, err
}

// noteSeen logs, once per change, how many gateways the monitor found and why the other pods with the gateway label were passed over, so
// a monitor that sees nothing says so.
func (m *EgressMonitor) noteSeen(found int, skipped map[string]int) {
	keys := make([]string, 0, len(skipped))
	for k := range skipped {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	state := fmt.Sprintf("gateways=%d", found)
	for _, k := range keys {
		state += fmt.Sprintf(" %s=%d", k, skipped[k])
	}
	m.mu.Lock()
	changed := state != m.seen
	m.seen = state
	m.mu.Unlock()
	if changed {
		m.Log.Info("egress monitor sees", "gateways", found, "skipped", skipped)
	}
}

func (m *EgressMonitor) selectGatewayPods(ctx context.Context) ([]corev1.Pod, map[string]int, error) {
	skipped := map[string]int{}
	var nss corev1.NamespaceList
	if err := m.Reader.List(ctx, &nss, client.HasLabels{names.LabelGroup}); err != nil {
		return nil, skipped, err
	}
	groups := map[string]bool{}
	for i := range nss.Items {
		groups[nss.Items[i].Name] = true
	}
	var pods corev1.PodList
	if err := m.Reader.List(ctx, &pods, client.MatchingLabels{labelApp: names.ComponentGateway}); err != nil {
		return nil, skipped, err
	}
	var out []corev1.Pod
	owned := map[string]bool{} // ReplicaSet (namespace/name) -> owned by the gateway Deployment
	for _, p := range pods.Items {
		switch {
		case !groups[p.Namespace]:
			skipped["not-a-group-namespace"]++
			continue
		case p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" || !p.DeletionTimestamp.IsZero():
			skipped["not-running"]++
			continue
		}
		rs := controllerRef(p.OwnerReferences, "ReplicaSet")
		if rs == nil {
			skipped["no-replicaset-owner"]++
			continue
		}
		key := p.Namespace + "/" + rs.Name + "/" + string(rs.UID)
		ok, seen := owned[key]
		if !seen {
			var r appsv1.ReplicaSet
			if err := m.Reader.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: rs.Name}, &r); err != nil {
				skipped["replicaset-unreadable"]++
				continue
			}
			d := controllerRef(r.OwnerReferences, "Deployment")
			ok = r.UID == rs.UID && d != nil && d.Name == gatewayDeployment
			owned[key] = ok
		}
		if !ok {
			skipped["not-the-gateway-deployment"]++
			continue
		}
		out = append(out, p)
	}
	return out, skipped, nil
}

func controllerRef(refs []metav1.OwnerReference, kind string) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller && refs[i].Kind == kind {
			return &refs[i]
		}
	}
	return nil
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

func (m *EgressMonitor) fetch(ctx context.Context, p *corev1.Pod, since time.Time) (*egressproxy.Summaries, error) {
	port := m.Port
	if port == 0 {
		port = 9188
	}
	c := m.HTTP
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	u := "http://" + net.JoinHostPort(p.Status.PodIP, strconv.Itoa(port)) + "/summaries?since=" + since.UTC().Format(time.RFC3339)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the gateway answered %d", resp.StatusCode)
	}
	var s egressproxy.Summaries
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSummaryBody)).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

// firstSince is where pulling a gateway starts when it has not been seen by this run: after the newest minute of its group that the log
// already holds (a restarted operator takes up the minutes it missed; the gateways keep an hour), else the last hour.
func (m *EgressMonitor) firstSince(ns string) time.Time {
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
		return t
	}
	return m.now().Add(-time.Hour)
}

// Pull takes the new minutes of every gateway, writes the log, and re-evaluates the signals.
func (m *EgressMonitor) Pull(ctx context.Context) error {
	pods, err := m.gatewayPods(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.last == nil {
		m.last = map[string]time.Time{}
	}
	m.mu.Unlock()
	var rows []EgressLogRow
	for i := range pods {
		p := &pods[i]
		key := string(p.UID)
		m.mu.Lock()
		since, ok := m.last[key]
		m.mu.Unlock()
		if !ok {
			since = m.firstSince(p.Namespace)
		}
		s, err := m.fetch(ctx, p, since)
		if err != nil {
			egressPullFailures.Inc()
			m.Log.Info("cannot pull the egress summaries of a gateway", "pod", client.ObjectKeyFromObject(p), "error", err.Error())
			continue
		}
		for _, min := range accept(s, since, m.now()) {
			egressMinutesPulled.Inc()
			mm := monitorMinute{start: min.Start, hosts: map[string]map[labKey]bool{}, caps: map[labKey]bool{}}
			for _, r := range min.Rows {
				rows = append(rows, EgressLogRow{Minute: min.Start, Namespace: p.Namespace, Lab: r.Lab, Host: r.Host, Conns: r.Conns, Up: r.Up, Down: r.Down, Denied: r.Denied})
				if r.Conns > 0 {
					if mm.hosts[r.Host] == nil {
						mm.hosts[r.Host] = map[labKey]bool{}
					}
					mm.hosts[r.Host][labKey{p.Namespace, r.Lab}] = true
				}
			}
			for _, c := range min.Caps {
				mm.caps[labKey{p.Namespace, c.Lab}] = true
			}
			m.mu.Lock()
			m.minutes = append(m.minutes, mm)
			if len(m.minutes) > maxKeptMinutes {
				m.minutes = m.minutes[len(m.minutes)-maxKeptMinutes:]
			}
			if min.Start.After(m.last[key]) {
				m.last[key] = min.Start
			}
			m.mu.Unlock()
		}
	}
	// forget the gateways that are gone
	m.mu.Lock()
	alive := map[string]bool{}
	for i := range pods {
		alive[string(pods[i].UID)] = true
	}
	for k := range m.last {
		if !alive[k] {
			delete(m.last, k)
		}
	}
	m.mu.Unlock()
	if err := m.writeLog(rows); err != nil {
		m.Log.Error(err, "write the egress connection log")
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
