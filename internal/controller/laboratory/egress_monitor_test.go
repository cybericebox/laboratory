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
	appsv1 "k8s.io/api/apps/v1"
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
	yes := true
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lg-a", Labels: map[string]string{names.LabelGroup: "g"}}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "gateway-abc", Namespace: "lg-a", UID: "rs-uid", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: names.ComponentGateway, Controller: &yes}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gateway-x", Namespace: "lg-a", UID: "gw-uid", Labels: map[string]string{names.LabelComponent: names.ComponentGateway},
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "gateway-abc", UID: "rs-uid", Controller: &yes}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: host}}
	objs := []client.Object{ns, rs, pod}
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

// Only the pods of the gateway Deployments that the operator made, in the namespaces of its groups, are asked; and what they answer is bounded.
func TestEgressMonitorAsksOnlyItsOwnGateways(t *testing.T) {
	f := newMonitorFixture(t, "l1")
	yes := true
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(f.gateway.URL, "http://"))
	mk := func(name, ns string, labels map[string]string, owners []metav1.OwnerReference) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name), Labels: labels, OwnerReferences: owners},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: host}}
	}
	gw := map[string]string{names.LabelComponent: names.ComponentGateway}
	foreign := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "not-a-group"}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "other-rs", Namespace: "lg-a", UID: "other-uid", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "something-else", Controller: &yes}}}},
		mk("no-owner", "lg-a", gw, nil),
		mk("outside-groups", "not-a-group", gw, []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "gateway-abc", UID: "rs-uid", Controller: &yes}}),
		mk("other-deployment", "lg-a", gw, []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "other-rs", UID: "other-uid", Controller: &yes}}),
		mk("wrong-rs-uid", "lg-a", gw, []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "gateway-abc", UID: "forged", Controller: &yes}}),
	}
	for _, o := range foreign {
		if err := f.c.Create(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	pods, err := f.m.gatewayPods(context.Background())
	if err != nil || len(pods) != 1 || pods[0].Name != "gateway-x" {
		t.Fatalf("only the operator's own gateway is asked: %v %v", names2(pods), err)
	}
	_, skipped, err := f.m.selectGatewayPods(context.Background())
	if err != nil || skipped["no-replicaset-owner"] != 1 || skipped["not-a-group-namespace"] != 1 || skipped["not-the-gateway-deployment"] != 2 {
		t.Fatalf("the monitor must say why it passed pods over: %v %v", skipped, err)
	}
}

func names2(pods []corev1.Pod) []string {
	var out []string
	for _, p := range pods {
		out = append(out, p.Name)
	}
	return out
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

// A gateway that answers with more than the bound is not believed; the table of minutes does not grow past its cap.
func TestEgressMonitorBodyAndTableAreBounded(t *testing.T) {
	f := newMonitorFixture(t, "l1")
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"minutes":[{"start":"2026-11-01T11:59:00Z","rows":[{"lab":"l1","host":"`))
		chunk := []byte(strings.Repeat("a", 1<<20))
		for i := 0; i < 8; i++ { // 8 MB in one string
			_, _ = w.Write(chunk)
		}
		_, _ = w.Write([]byte(`","conns":1}]}]}`))
	}))
	defer big.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(big.URL, "http://"))
	p, _ := strconv.Atoi(port)
	goodPort := f.m.Port
	f.m.Port = p
	var pod corev1.Pod
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "lg-a", Name: "gateway-x"}, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.PodIP = host
	if err := f.c.Status().Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.fetch(context.Background(), &pod, time.Time{}); err == nil {
		t.Fatal("a body over the bound was accepted")
	}
	for i := 0; i < maxKeptMinutes+50; i++ {
		f.m.minutes = append(f.m.minutes, monitorMinute{start: f.m.now().Add(-time.Second)})
	}
	*f.minutes = []egressproxy.Minute{{Start: time.Date(2026, 11, 1, 11, 59, 0, 0, time.UTC), Rows: []egressproxy.Row{{Lab: "l1", Host: "x.test", Conns: 1}}}}
	pod.Status.PodIP, f.m.Port = strings.Split(strings.TrimPrefix(f.gateway.URL, "http://"), ":")[0], goodPort
	if err := f.c.Status().Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	_ = f.m.Pull(context.Background())
	if len(f.m.minutes) > maxKeptMinutes+1 {
		t.Fatalf("the table of minutes grew to %d", len(f.m.minutes))
	}
}
