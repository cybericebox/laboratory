package egressproxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

type reportFixture struct {
	c     client.Client
	clock *time.Time
	col   *Collector
	w     *ReportWriter
}

func newReportFixture(t *testing.T, fns ...interceptor.Funcs) *reportFixture {
	t.Helper()
	s := runtime.NewScheme()
	_ = laboratoryv1alpha1.AddToScheme(s)
	b := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&laboratoryv1alpha1.LabEgressReport{})
	if len(fns) > 0 {
		b = b.WithInterceptorFuncs(fns[0])
	}
	c := b.Build()
	now := time.Date(2026, 11, 1, 12, 0, 10, 0, time.UTC)
	f := &reportFixture{c: c, clock: &now}
	f.col = NewCollector("gateway-x")
	f.col.Now = func() time.Time { return *f.clock }
	f.w = &ReportWriter{Reader: c, Writer: c, Namespace: "lg-a", Instance: "gateway-x", BootID: "boot-1", Collector: f.col, Now: func() time.Time { return *f.clock }, Log: logr.Discard()}
	return f
}

// minute records one connection in the current minute and moves the clock to the start of the next one, which closes it.
func (f *reportFixture) minute(host string) time.Time {
	start := f.clock.Truncate(time.Minute)
	f.col.Start("l1", host)
	f.col.End("l1", host, 10, 20)
	*f.clock = start.Add(time.Minute + 10*time.Second)
	return start
}

func (f *reportFixture) report(t *testing.T) *laboratoryv1alpha1.LabEgressReport {
	t.Helper()
	var r laboratoryv1alpha1.LabEgressReport
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "lg-a", Name: "gateway"}, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func (f *reportFixture) ack(t *testing.T, through time.Time) {
	t.Helper()
	r := f.report(t)
	at := metav1.NewTime(through)
	r.Spec.AckedThrough = &at
	if err := f.c.Update(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func starts(r *laboratoryv1alpha1.LabEgressReport) []string {
	var out []string
	for _, m := range r.Status.Minutes {
		out = append(out, m.Start.UTC().Format("15:04"))
	}
	return out
}

func TestReportWriterCreatesTheReportAndWritesFinishedMinutes(t *testing.T) {
	f := newReportFixture(t)
	ctx := context.Background()
	if err := f.w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var none laboratoryv1alpha1.LabEgressReportList
	if err := f.c.List(ctx, &none); err != nil || len(none.Items) != 0 {
		t.Fatalf("nothing to report: no object is made (%d, %v)", len(none.Items), err)
	}
	m1 := f.minute("a.test")
	if err := f.w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	r := f.report(t)
	if r.Name != "gateway" || r.Status.Instance != "gateway-x" || r.Status.BootID != "boot-1" || r.Status.UpdatedAt == nil {
		t.Fatalf("report identity: %+v", r.Status)
	}
	if len(r.Status.Minutes) != 1 || !r.Status.Minutes[0].Start.Time.Equal(m1) || len(r.Status.Minutes[0].Rows) != 1 {
		t.Fatalf("minutes = %+v", r.Status.Minutes)
	}
	row := r.Status.Minutes[0].Rows[0]
	if row.Lab != "l1" || row.Host != "a.test" || row.Conns != 1 || row.Up != 10 || row.Down != 20 {
		t.Fatalf("row = %+v", row)
	}
	if r.Spec.AckedThrough != nil {
		t.Fatal("the gateway never writes the spec")
	}
}

// A write happens only when the minutes changed: a sync with nothing new leaves the object alone.
func TestReportWriterWritesOnlyWhenSomethingChanged(t *testing.T) {
	f := newReportFixture(t)
	ctx := context.Background()
	f.minute("a.test")
	if err := f.w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	rv := f.report(t).ResourceVersion
	for i := 0; i < 3; i++ {
		*f.clock = f.clock.Add(20 * time.Second) // same minute still open
		if err := f.w.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.report(t).ResourceVersion; got != rv {
		t.Fatalf("the report was written again with nothing new (%s -> %s)", rv, got)
	}
	if ReportEvery != 20*time.Second {
		t.Fatalf("the least time between writes is 20 s, got %s", ReportEvery)
	}
}

// The minutes the operator acknowledged are removed; the ones after them stay; the removal is one write.
func TestReportWriterTrimsWhatTheOperatorAcknowledged(t *testing.T) {
	f := newReportFixture(t)
	ctx := context.Background()
	var ms []time.Time
	for _, h := range []string{"a", "b", "c", "d"} {
		ms = append(ms, f.minute(h+".test"))
	}
	if err := f.w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := starts(f.report(t)); len(got) != 4 {
		t.Fatalf("four unacknowledged minutes: %v", got)
	}
	f.ack(t, ms[1]) // the first two are logged
	if err := f.w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	got := starts(f.report(t))
	if len(got) != 2 || got[0] != ms[2].Format("15:04") || got[1] != ms[3].Format("15:04") {
		t.Fatalf("after the acknowledgement of %s: %v", ms[1].Format("15:04"), got)
	}
	f.ack(t, ms[3])
	if err := f.w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if r := f.report(t); len(r.Status.Minutes) != 0 {
		t.Fatalf("everything is acknowledged: %v", starts(r))
	}
	// an acknowledged minute does not come back from the Collector, which still holds it
	if err := f.w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if r := f.report(t); len(r.Status.Minutes) != 0 {
		t.Fatalf("an acknowledged minute came back: %v", starts(r))
	}
}

// Over 30 minutes the oldest minute is dropped and counted.
func TestReportWriterDropsTheOldestMinuteOverTheBoundAndCountsIt(t *testing.T) {
	f := newReportFixture(t)
	ctx := context.Background()
	var ms []time.Time
	for i := 0; i < MaxReportMinutes+5; i++ {
		ms = append(ms, f.minute("h"+strconv.Itoa(i)+".test"))
	}
	if err := f.w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	r := f.report(t)
	if len(r.Status.Minutes) != MaxReportMinutes || r.Status.DroppedMinutes != 5 {
		t.Fatalf("%d minutes kept, %d dropped; want %d and 5", len(r.Status.Minutes), r.Status.DroppedMinutes, MaxReportMinutes)
	}
	if !r.Status.Minutes[0].Start.Time.Equal(ms[5]) || !r.Status.Minutes[MaxReportMinutes-1].Start.Time.Equal(ms[len(ms)-1]) {
		t.Fatalf("the oldest five went, the newest stayed: first %v last %v", r.Status.Minutes[0].Start, r.Status.Minutes[MaxReportMinutes-1].Start)
	}
	// the operator takes the rest: the dropped count stays (it is a count of what was lost)
	f.ack(t, ms[len(ms)-1])
	if err := f.w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if r := f.report(t); len(r.Status.Minutes) != 0 || r.Status.DroppedMinutes != 5 {
		t.Fatalf("after the acknowledgement: %d minutes, dropped %d", len(r.Status.Minutes), r.Status.DroppedMinutes)
	}
}

// All the rows of all the minutes together stay at 4000: past that the oldest minutes go.
func TestReportWriterKeepsAllRowsUnderTheTotalBound(t *testing.T) {
	f := newReportFixture(t)
	ctx := context.Background()
	full := func(start time.Time) {
		for i := 0; i < MaxReportRows; i++ {
			f.col.Start("l"+strconv.Itoa(i), "h.test")
		}
		for i := 0; i < MaxReportCaps; i++ {
			f.col.Cap("c"+strconv.Itoa(i), "lab-rate")
		}
		for i := 0; i < MaxReportDenials; i++ {
			f.col.Denied("d"+strconv.Itoa(i), "", "deny-list")
		}
		*f.clock = start.Add(time.Minute + 10*time.Second)
	}
	var ms []time.Time
	for i := 0; i < 4; i++ {
		start := f.clock.Truncate(time.Minute)
		ms = append(ms, start)
		full(start)
	}
	if err := f.w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	r := f.report(t)
	total := 0
	for _, m := range r.Status.Minutes {
		total += len(m.Rows) + len(m.Caps) + len(m.Denials) + len(m.Flows)
	}
	if total > MaxReportRowsAllMinutes {
		t.Fatalf("%d rows of all kinds, bound %d", total, MaxReportRowsAllMinutes)
	}
	// 500 rows + 500 caps + 500 denials = 1500 a minute (the Denied rows with no host add no Row): at most two fit under 4000, three do not
	if len(r.Status.Minutes) != 2 || r.Status.DroppedMinutes != 2 || !r.Status.Minutes[0].Start.Time.Equal(ms[2]) {
		t.Fatalf("minutes %v dropped %d", starts(r), r.Status.DroppedMinutes)
	}
}

// A minute with more than the per-minute bound keeps the busiest rows.
func TestReportMinuteKeepsTheBusiestRowsOverTheBound(t *testing.T) {
	var rows []Row
	for i := 0; i < MaxReportRows+100; i++ {
		rows = append(rows, Row{Lab: "l", Host: fmt.Sprintf("h%03d.test", i), Conns: int64(i + 1)})
	}
	var flows []FlowRow
	for i := 0; i < MaxReportFlows+50; i++ {
		flows = append(flows, FlowRow{Lab: "l", Source: "10.0.0.1", Dest: "1.1.1.1", Port: i + 1, Up: int64(i)})
	}
	out := ReportMinute(Minute{Start: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), Rows: rows, Flows: flows})
	if len(out.Rows) != MaxReportRows || len(out.Flows) != MaxReportFlows {
		t.Fatalf("rows %d flows %d", len(out.Rows), len(out.Flows))
	}
	if out.Rows[0].Conns != int64(MaxReportRows+100) {
		t.Fatalf("the busiest row comes first: %+v", out.Rows[0])
	}
	for _, r := range out.Rows {
		if r.Conns <= 100 {
			t.Fatalf("a quiet row (%d connections) was kept while a busy one was cut", r.Conns)
		}
	}
}

// A gateway that restarts keeps the unacknowledged minutes of its predecessor and adds its own; the report names the new pod and boot.
func TestReportWriterRestartKeepsThePredecessorsMinutes(t *testing.T) {
	f := newReportFixture(t)
	ctx := context.Background()
	m1 := f.minute("before1.test")
	m2 := f.minute("before2.test")
	if err := f.w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	f.ack(t, m1)
	if err := f.w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// the pod is replaced: a new Collector with nothing in it, a new writer with a new identity
	col := NewCollector("gateway-y")
	col.Now = func() time.Time { return *f.clock }
	w := &ReportWriter{Reader: f.c, Writer: f.c, Namespace: "lg-a", Instance: "gateway-y", BootID: "boot-2", Collector: col, Now: func() time.Time { return *f.clock }, Log: logr.Discard()}
	if err := w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := starts(f.report(t)); len(got) != 1 || got[0] != m2.Format("15:04") {
		t.Fatalf("the minute of the predecessor that is not acknowledged stays: %v", got)
	}
	col.Start("l1", "after.test")
	m3 := f.clock.Truncate(time.Minute)
	*f.clock = m3.Add(time.Minute + 10*time.Second)
	if err := w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	r := f.report(t)
	if got := starts(r); len(got) != 2 || got[0] != m2.Format("15:04") || got[1] != m3.Format("15:04") {
		t.Fatalf("minutes = %v", got)
	}
	if r.Status.Instance != "gateway-y" || r.Status.BootID != "boot-2" {
		t.Fatalf("the report names the pod that wrote it last: %q %q", r.Status.Instance, r.Status.BootID)
	}
}

// A failed read is an error, not "there is no report": the writer does not create over an object it could not see.
func TestReportWriterReadErrorIsAnError(t *testing.T) {
	boom := errors.New("api unavailable")
	f := newReportFixture(t, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return boom
	}})
	f.minute("a.test")
	if err := f.w.Sync(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	var all laboratoryv1alpha1.LabEgressReportList
	if err := f.c.List(context.Background(), &all); err != nil || len(all.Items) != 0 {
		t.Fatalf("nothing was created: %d %v", len(all.Items), err)
	}
}

// The numbers in the CRD (maxItems) and in the writer are the same, and the two copies of the CRD are the same file.
func TestReportBoundsMatchTheCRD(t *testing.T) {
	read := func(path string) []byte {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	base := read("../../config/crd/bases/laboratory.cybericebox.com_labegressreports.yaml")
	chart := read("../../charts/laboratory/crds/laboratory.cybericebox.com_labegressreports.yaml")
	if string(base) != string(chart) {
		t.Fatal("charts/laboratory/crds and config/crd/bases differ: run make manifests")
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(base, &crd); err != nil {
		t.Fatal(err)
	}
	status := crd.Spec.Versions[0].Schema.OpenAPIV3Schema["properties"].(map[string]any)["status"].(map[string]any)["properties"].(map[string]any)
	minutes := status["minutes"].(map[string]any)
	item := minutes["items"].(map[string]any)["properties"].(map[string]any)
	max := func(m any) int { return int(m.(map[string]any)["maxItems"].(float64)) }
	for name, want := range map[string]int{"minutes": MaxReportMinutes} {
		if got := max(status[name]); got != want {
			t.Errorf("%s maxItems = %d, writer bound %d", name, got, want)
		}
	}
	for name, want := range map[string]int{"rows": MaxReportRows, "caps": MaxReportCaps, "denials": MaxReportDenials, "flows": MaxReportFlows} {
		if got := max(item[name]); got != want {
			t.Errorf("%s maxItems = %d, writer bound %d", name, got, want)
		}
	}
}
