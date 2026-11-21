package laboratory

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/activity"
	"github.com/cybericebox/laboratory/internal/egressproxy"
	"github.com/cybericebox/laboratory/internal/names"
)

type monitorFixture struct {
	m      *EgressMonitor
	c      client.Client
	clock  *time.Time
	events *record.FakeRecorder
}

func newMonitorFixture(t *testing.T, labs ...string) *monitorFixture {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = laboratoryv1alpha1.AddToScheme(s)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lg-a", Labels: map[string]string{names.LabelGroup: "g"}}}
	objs := []client.Object{ns}
	for _, l := range labs {
		objs = append(objs, &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: l, Namespace: "lg-a"}})
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&laboratoryv1alpha1.Lab{}, &laboratoryv1alpha1.LabEgressReport{}).Build()
	now := time.Date(2026, 11, 1, 12, 0, 30, 0, time.UTC)
	rec := record.NewFakeRecorder(100)
	m := &EgressMonitor{Client: c, Reader: c, Recorder: rec, Log: logr.Discard(), HostLabsThreshold: 3, CapMinutes: 2, Window: 10 * time.Minute, Now: func() time.Time { return now }}
	return &monitorFixture{m: m, c: c, clock: &now, events: rec}
}

// setMinutes is what the gateway of namespace ns has in its report: exactly these minutes (the status is replaced).
func (f *monitorFixture) setMinutes(t *testing.T, ns, name string, minutes ...egressproxy.Minute) {
	t.Helper()
	r := &laboratoryv1alpha1.LabEgressReport{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	err := f.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, r)
	if apierrors.IsNotFound(err) {
		if err := f.c.Create(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	r.Status.Instance = "gateway-x"
	r.Status.Minutes = nil
	for _, m := range minutes {
		r.Status.Minutes = append(r.Status.Minutes, egressproxy.ReportMinute(m))
	}
	if err := f.c.Status().Update(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func (f *monitorFixture) report(t *testing.T, ns string) *laboratoryv1alpha1.LabEgressReport {
	t.Helper()
	var r laboratoryv1alpha1.LabEgressReport
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "gateway"}, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func (f *monitorFixture) flagged(t *testing.T, lab string) *metav1.Condition {
	t.Helper()
	var l laboratoryv1alpha1.Lab
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "lg-a", Name: lab}, &l); err != nil {
		t.Fatal(err)
	}
	return meta.FindStatusCondition(l.Status.Conditions, ConditionEgressAttention)
}

// Many labs hitting one host name are flagged, with an Event; the connection log is written and pruned; the flag goes when the labs stop.
func TestEgressMonitorFlagsManyLabsOnOneHostAndKeepsTheLog(t *testing.T) {
	f := newMonitorFixture(t, "l1", "l2", "l3", "quiet")
	dir := t.TempDir()
	f.m.LogDir = dir
	// an old file past the retention
	old := filepath.Join(dir, "egress-2026-09-01.jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	min := time.Date(2026, 11, 1, 11, 59, 0, 0, time.UTC)
	f.setMinutes(t, "lg-a", "gateway", egressproxy.Minute{Start: min, Rows: []egressproxy.Row{
		{Lab: "l1", Host: "victim.example", Conns: 4, Up: 10, Down: 20}, {Lab: "l2", Host: "victim.example", Conns: 9},
		{Lab: "l3", Host: "victim.example", Conns: 1}, {Lab: "quiet", Host: "pypi.org", Conns: 2},
	}})
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"l1", "l2", "l3"} {
		c := f.flagged(t, l)
		if c == nil || c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "victim.example") || !strings.Contains(c.Message, "3 labs") {
			t.Errorf("%s must be flagged for the host: %+v", l, c)
		}
	}
	if f.flagged(t, "quiet") != nil {
		t.Error("a lab that hits an ordinary host is not flagged")
	}
	if len(f.events.Events) != 3 {
		t.Errorf("one Event per flagged lab, got %d", len(f.events.Events))
	}
	b, err := os.ReadFile(filepath.Join(dir, "egress-2026-11-01.jsonl"))
	if err != nil || strings.Count(string(b), "\n") != 4 || !strings.Contains(string(b), `"host":"victim.example"`) {
		t.Fatalf("connection log: %q %v", b, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("a file older than 30 days must be removed")
	}
	// the same minute pulled again adds nothing; the labs stay flagged
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(filepath.Join(dir, "egress-2026-11-01.jsonl")); string(b2) != string(b) {
		t.Error("a minute already pulled was logged twice")
	}
	// ten minutes later the window has moved past it: the flags go
	*f.clock = f.clock.Add(11 * time.Minute)
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.flagged(t, "l1") != nil {
		t.Error("the flag must go when the labs stop")
	}
}

func TestEgressMonitorFlagsALabAtItsCaps(t *testing.T) {
	f := newMonitorFixture(t, "busy")
	base := time.Date(2026, 11, 1, 11, 57, 0, 0, time.UTC)
	f.setMinutes(t, "lg-a", "gateway",
		egressproxy.Minute{Start: base, Caps: []egressproxy.CapHit{{Lab: "busy", Reason: "lab-rate", Hits: 5}}},
		egressproxy.Minute{Start: base.Add(time.Minute), Caps: []egressproxy.CapHit{{Lab: "busy", Reason: "dns-rate", Hits: 2}}},
	)
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := f.flagged(t, "busy"); c == nil || !strings.Contains(c.Message, "caps") {
		t.Fatalf("a lab at its caps in 2 minutes must be flagged: %+v", c)
	}
}

// An operator that restarted takes the flag off a lab that carried it.
func TestEgressMonitorClearsAFlagItDidNotSetInThisRun(t *testing.T) {
	f := newMonitorFixture(t, "stale")
	var l laboratoryv1alpha1.Lab
	_ = f.c.Get(context.Background(), types.NamespacedName{Namespace: "lg-a", Name: "stale"}, &l)
	meta.SetStatusCondition(&l.Status.Conditions, metav1.Condition{Type: ConditionEgressAttention, Status: metav1.ConditionTrue, Reason: "EgressSignal", Message: "old"})
	if err := f.c.Status().Update(context.Background(), &l); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.flagged(t, "stale") != nil {
		t.Error("a flag with nothing behind it must be taken off")
	}
}

// Only the report named "gateway" in a namespace that carries the group label is read; the others are passed over and counted.
func TestEgressMonitorReadsOnlyTheGatewayReportOfAGroupNamespace(t *testing.T) {
	f := newMonitorFixture(t, "l1")
	min := time.Date(2026, 11, 1, 11, 59, 0, 0, time.UTC)
	mine := egressproxy.Minute{Start: min, Rows: []egressproxy.Row{{Lab: "l1", Host: "mine.test", Conns: 1}}}
	foreign := egressproxy.Minute{Start: min, Rows: []egressproxy.Row{{Lab: "l1", Host: "forged.test", Conns: 1}}}
	for _, ns := range []string{"not-a-group", "lg-b-unlabelled"} {
		if err := f.c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	f.setMinutes(t, "lg-a", "gateway", mine)
	f.setMinutes(t, "lg-a", "somebody-else", foreign)
	f.setMinutes(t, "not-a-group", "gateway", foreign)
	f.setMinutes(t, "lg-b-unlabelled", "gateway", foreign)
	f.setMinutes(t, "lg-nonexistent", "gateway", foreign)
	reports, skipped, err := f.m.groupReports(context.Background())
	if err != nil || len(reports) != 1 || reports[0].Namespace != "lg-a" || reports[0].Name != "gateway" {
		t.Fatalf("only lg-a/gateway is read: %+v %v", reports, err)
	}
	if skipped["not-a-group-namespace"] != 3 || skipped["not-named-gateway"] != 1 {
		t.Fatalf("the monitor must say why it passed reports over: %v", skipped)
	}
	dir := t.TempDir()
	f.m.LogDir = dir
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "egress-2026-11-01.jsonl"))
	if !strings.Contains(string(b), "mine.test") || strings.Contains(string(b), "forged.test") {
		t.Fatalf("what a foreign report says is not believed: %q", b)
	}
	for _, other := range []struct{ ns, name string }{{"lg-a", "somebody-else"}, {"not-a-group", "gateway"}} {
		var r laboratoryv1alpha1.LabEgressReport
		if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: other.ns, Name: other.name}, &r); err != nil {
			t.Fatal(err)
		}
		if r.Spec.AckedThrough != nil {
			t.Errorf("%s/%s is not acknowledged: the monitor does not touch a report it does not believe", other.ns, other.name)
		}
	}
}

func TestEgressMonitorBoundsWhatAGatewayTells(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 30, 0, time.UTC)
	since := now.Add(-5 * time.Minute)
	long := strings.Repeat("a", 300)
	var rows []egressproxy.Row
	for i := 0; i < maxRowsPerMinute+50; i++ {
		rows = append(rows, egressproxy.Row{Lab: "l", Host: "h" + strconv.Itoa(i), Conns: 1})
	}
	s := &egressproxy.Summaries{Minutes: []egressproxy.Minute{
		{Start: now.Add(-time.Minute), Rows: rows}, // fine, trimmed to the cap
		{Start: now.Add(10 * time.Minute)},         // from the future
		{Start: now.Add(-2 * time.Hour)},           // older than an hour
		{Start: since},                             // not after what was asked
		{Start: now.Add(-2 * time.Minute), Rows: []egressproxy.Row{{Lab: "l", Host: long, Conns: 1}, {Lab: "l", Host: "ok.test", Conns: -4}, {Lab: "l", Host: "fine.test", Conns: 2}}},
	}}
	got := accept(s, since, now)
	if len(got) != 2 || len(got[0].Rows) != maxRowsPerMinute {
		t.Fatalf("minutes kept: %d (first has %d rows)", len(got), len(got[0].Rows))
	}
	if len(got[1].Rows) != 1 || got[1].Rows[0].Host != "fine.test" {
		t.Fatalf("a long name and a negative count must be dropped: %+v", got[1].Rows)
	}
	many := &egressproxy.Summaries{}
	for i := 0; i < 500; i++ {
		many.Minutes = append(many.Minutes, egressproxy.Minute{Start: now.Add(-time.Duration(i+1) * 5 * time.Second)})
	}
	if got := accept(many, since, now); len(got) > maxMinutesPerPull {
		t.Fatalf("%d minutes in one pull", len(got))
	}
}

// The table of minutes the signals are computed from does not grow past its cap.
func TestEgressMonitorTableIsBounded(t *testing.T) {
	f := newMonitorFixture(t, "l1")
	for i := 0; i < maxKeptMinutes+50; i++ {
		f.m.minutes = append(f.m.minutes, monitorMinute{start: f.m.now().Add(-time.Second)})
	}
	f.setMinutes(t, "lg-a", "gateway", egressproxy.Minute{Start: time.Date(2026, 11, 1, 11, 59, 0, 0, time.UTC), Rows: []egressproxy.Row{{Lab: "l1", Host: "x.test", Conns: 1}}})
	_ = f.m.Pull(context.Background())
	if len(f.m.minutes) > maxKeptMinutes+1 {
		t.Fatalf("the table of minutes grew to %d", len(f.m.minutes))
	}
}

// With activity reports on, the gateways' per-address rows become raw facts in the ring of the group namespace; the connection log of the
// administrator is the same as before (no address in it). With them off nothing is kept.
func TestEgressMonitorHandsFlowRowsToTheActivityRing(t *testing.T) {
	f := newMonitorFixture(t, "l1")
	f.m.Activity = activity.New(f.c, "egress")
	f.m.Activity.Now = f.m.now
	min := time.Date(2026, 11, 1, 11, 59, 0, 0, time.UTC)
	f.setMinutes(t, "lg-a", "gateway", egressproxy.Minute{Start: min,
		Rows:  []egressproxy.Row{{Lab: "l1", Host: "pypi.org", Conns: 2, Up: 10, Down: 20}},
		Flows: []egressproxy.FlowRow{{Lab: "l1", Source: "10.9.1.5", Dest: "151.101.0.223", Port: 443, Conns: 2, Up: 10, Down: 20, FirstMs: 1, LastMs: 2}, {Lab: "l1", Source: "x", Dest: "y", Port: 70000}}})
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.m.Activity.Events("lg-a")
	if len(got) != 1 {
		t.Fatalf("events = %+v (a row with an impossible port is dropped)", got)
	}
	e := got[0]
	if e.Kind != laboratoryv1alpha1.ActivityEgress || e.LabName != "l1" || e.SourceAddr != "10.9.1.5" || e.DstAddr != "151.101.0.223" || e.DstPort != 443 ||
		e.BytesUp != 10 || e.BytesDown != 20 || e.Connections != 2 || e.AtMs != min.Add(time.Minute).UnixMilli() {
		t.Fatalf("event = %+v", e)
	}
}

func TestEgressMonitorKeepsNoActivityWhenOff(t *testing.T) {
	f := newMonitorFixture(t, "l1")
	min := time.Date(2026, 11, 1, 11, 59, 0, 0, time.UTC)
	f.setMinutes(t, "lg-a", "gateway", egressproxy.Minute{Start: min, Flows: []egressproxy.FlowRow{{Lab: "l1", Source: "10.9.1.5", Dest: "1.2.3.4", Port: 443, Conns: 1}}})
	if err := f.m.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.m.Activity != nil {
		t.Fatal("the hub must be off by default")
	}
}
