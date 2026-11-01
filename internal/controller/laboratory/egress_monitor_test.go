package laboratory

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/egressproxy"
	"github.com/cybericebox/laboratory/internal/names"
)

type monitorFixture struct {
	m       *EgressMonitor
	c       client.Client
	gateway *httptest.Server
	clock   *time.Time
	minutes *[]egressproxy.Minute
	events  *record.FakeRecorder
}

func newMonitorFixture(t *testing.T, labs ...string) *monitorFixture {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = laboratoryv1alpha1.AddToScheme(s)
	var minutes []egressproxy.Minute
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		since, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
		out := egressproxy.Summaries{Gateway: "gw"}
		for _, m := range minutes {
			if m.Start.After(since) {
				out.Minutes = append(out.Minutes, m)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(gw.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(gw.URL, "http://"))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gateway-x", Namespace: "lg-a", UID: "gw-uid", Labels: map[string]string{names.LabelComponent: names.ComponentGateway}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: host}}
	objs := []client.Object{pod}
	for _, l := range labs {
		objs = append(objs, &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: l, Namespace: "lg-a"}})
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&laboratoryv1alpha1.Lab{}).Build()
	now := time.Date(2026, 11, 1, 12, 0, 30, 0, time.UTC)
	p, _ := strconv.Atoi(port)
	rec := record.NewFakeRecorder(100)
	m := &EgressMonitor{Client: c, Reader: c, Recorder: rec, Log: logr.Discard(), Port: p, HostLabsThreshold: 3, CapMinutes: 2, Window: 10 * time.Minute, Now: func() time.Time { return now }}
	return &monitorFixture{m: m, c: c, gateway: gw, clock: &now, minutes: &minutes, events: rec}
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
	*f.minutes = []egressproxy.Minute{{Start: min, Rows: []egressproxy.Row{
		{Lab: "l1", Host: "victim.example", Conns: 4, Up: 10, Down: 20}, {Lab: "l2", Host: "victim.example", Conns: 9},
		{Lab: "l3", Host: "victim.example", Conns: 1}, {Lab: "quiet", Host: "pypi.org", Conns: 2},
	}}}
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
	*f.minutes = []egressproxy.Minute{
		{Start: base, Caps: []egressproxy.CapHit{{Lab: "busy", Reason: "lab-rate", Hits: 5}}},
		{Start: base.Add(time.Minute), Caps: []egressproxy.CapHit{{Lab: "busy", Reason: "dns-rate", Hits: 2}}},
	}
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
