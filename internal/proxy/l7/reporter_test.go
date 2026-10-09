package l7

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestReportNameIsADNSName(t *testing.T) {
	if got := ReportName("Laboratory-Proxy_7d9f"); got != "proxy-laboratory-proxy-7d9f" {
		t.Fatalf("name = %q", got)
	}
}

func TestReportWriterPublishesLedgerAndHeartbeatForEveryNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&laboratoryv1alpha1.LabTrafficReport{}).Build()
	started := time.UnixMilli(1_000)
	meter := NewMeter("boot-1", started)
	meter.Record("ns-busy", "user-1", "c-1", time.UnixMilli(2_000), true, 7, 3)

	w := &ReportWriter{Reader: c, Writer: c, Meter: meter, Instance: "proxy-abc",
		Namespaces: func(context.Context) []string { return []string{"ns-busy", "ns-idle"} }}
	w.PublishAll(context.Background(), time.UnixMilli(61_000), func(err error) { t.Fatal(err) })

	get := func(ns string) laboratoryv1alpha1.LabTrafficReport {
		var got laboratoryv1alpha1.LabTrafficReport
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "proxy-proxy-abc"}, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	busy := get("ns-busy")
	if busy.Spec.Kind != laboratoryv1alpha1.LabTrafficSurfaceProxy || busy.Status.BootID != "boot-1" || busy.Status.CoveredFromMs != 1_000 || busy.Status.CoveredToMs != 61_000 {
		t.Fatalf("busy = %+v", busy)
	}
	if len(busy.Status.Ledger) != 1 || busy.Status.Ledger[0].Subject != "user-1" || busy.Status.Ledger[0].Attempts != 1 || busy.Status.Ledger[0].BytesIn != 7 || busy.Status.Ledger[0].FirstRespondedMs != 2_000 {
		t.Fatalf("ledger = %+v", busy.Status.Ledger)
	}
	idle := get("ns-idle")
	if len(idle.Status.Ledger) != 0 || idle.Status.CoveredToMs != 61_000 {
		t.Fatalf("an idle group still gets its coverage heartbeat: %+v", idle.Status)
	}
}

func TestSamePodRestartKeepsTotals(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = laboratoryv1alpha1.AddToScheme(scheme)
	old := &laboratoryv1alpha1.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: ReportName("pod"), Namespace: "ns"}, Spec: laboratoryv1alpha1.LabTrafficReportSpec{Kind: laboratoryv1alpha1.LabTrafficSurfaceProxy, Instance: "pod"}, Status: laboratoryv1alpha1.LabTrafficReportStatus{BootID: "old", CoveredFromMs: 1000, CoveredToMs: 2000, Ledger: []laboratoryv1alpha1.LabTrafficTouch{{Subject: "p", LabName: "lab", Attempts: 8, BytesIn: 80, FirstSeenMs: 1000, LastSeenMs: 2000}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(old).WithObjects(old).Build()
	m := NewMeter("new", time.UnixMilli(3000))
	m.Record("ns", "p", "lab", time.UnixMilli(3500), true, 1, 0)
	w := &ReportWriter{Reader: c, Writer: c, Meter: m, Instance: "pod"}
	for i := 0; i < 2; i++ {
		if err := w.Publish(context.Background(), "ns", time.UnixMilli(4000)); err != nil {
			t.Fatal(err)
		}
	}
	var got laboratoryv1alpha1.LabTrafficReport
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: ReportName("pod")}, &got)
	if got.Status.Ledger[0].Attempts != 9 || got.Status.Ledger[0].BytesIn != 81 || got.Status.CoveredFromMs != 3000 {
		t.Fatal(got.Status)
	}
	if len(got.Status.CoverageSpans) != 2 {
		t.Fatalf("restart history=%+v", got.Status.CoverageSpans)
	}
	a, b := got.Status.CoverageSpans[0], got.Status.CoverageSpans[1]
	if a.FromMs != 1000 || a.ToMs != 2000 || a.BootID != "old" || b.FromMs != 3000 || b.ToMs != 4000 || b.BootID != "new" || a.Source != ReportName("pod") {
		t.Fatal(got.Status.CoverageSpans)
	}

}

type failingReportReader struct {
	client.Reader
	fail bool
}

func (r *failingReportReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if r.fail {
		return errors.New("read unavailable")
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}
func TestRestoreFailureCannotOverwriteOldTotals(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = laboratoryv1alpha1.AddToScheme(scheme)
	old := &laboratoryv1alpha1.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: ReportName("pod"), Namespace: "ns"}, Spec: laboratoryv1alpha1.LabTrafficReportSpec{Kind: laboratoryv1alpha1.LabTrafficSurfaceProxy}, Status: laboratoryv1alpha1.LabTrafficReportStatus{Ledger: []laboratoryv1alpha1.LabTrafficTouch{{Subject: "p", LabName: "lab", Attempts: 8}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(old).WithObjects(old).Build()
	reader := &failingReportReader{Reader: c, fail: true}
	m := NewMeter("new", time.Now())
	m.Record("ns", "p", "lab", time.Now(), false, 0, 0)
	w := &ReportWriter{Reader: reader, Writer: c, Meter: m, Instance: "pod"}
	if err := w.Prepare(context.Background(), "ns"); err == nil {
		t.Fatal("restore error missing")
	}
	var got laboratoryv1alpha1.LabTrafficReport
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: ReportName("pod")}, &got)
	if got.Status.Ledger[0].Attempts != 8 {
		t.Fatal(got.Status)
	}
	reader.fail = false
	if err := w.Publish(context.Background(), "ns", time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: ReportName("pod")}, &got)
	if got.Status.Ledger[0].Attempts != 9 {
		t.Fatal(got.Status)
	}
}

func TestRestartCoverageHistoryIsBoundedAndMarkedIncomplete(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = laboratoryv1alpha1.AddToScheme(scheme)
	old := &laboratoryv1alpha1.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: ReportName("pod"), Namespace: "ns"}, Spec: laboratoryv1alpha1.LabTrafficReportSpec{Kind: laboratoryv1alpha1.LabTrafficSurfaceProxy}}
	for i := 0; i < MaxReportRows; i++ {
		old.Status.CoverageSpans = append(old.Status.CoverageSpans, laboratoryv1alpha1.LabTrafficCoverageSpan{FromMs: int64(1000 + i), ToMs: int64(2000 + i)})
	}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(old).WithObjects(old).Build()
	w := &ReportWriter{Reader: store, Writer: store, Meter: NewMeter("new", time.UnixMilli(3000)), Instance: "pod"}
	if err := w.Publish(context.Background(), "ns", time.UnixMilli(4000)); err != nil {
		t.Fatal(err)
	}
	var got laboratoryv1alpha1.LabTrafficReport
	_ = store.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: ReportName("pod")}, &got)
	if len(got.Status.CoverageSpans) != MaxReportRows || !got.Status.Partial || got.Status.CoverageSpans[MaxReportRows-1].BootID != "new" {
		t.Fatal(got.Status)
	}
}

func TestSaturatedRestorePreservesDurableBaselineAndRetries(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = laboratoryv1alpha1.AddToScheme(scheme)
	old := &laboratoryv1alpha1.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: ReportName("pod"), Namespace: "target"}, Spec: laboratoryv1alpha1.LabTrafficReportSpec{Kind: laboratoryv1alpha1.LabTrafficSurfaceProxy}, Status: laboratoryv1alpha1.LabTrafficReportStatus{CoveredFromMs: 1000, CoveredToMs: 2000, Ledger: []laboratoryv1alpha1.LabTrafficTouch{{Subject: "p", LabName: "lab", Attempts: 8, BytesIn: 80, FirstSeenMs: 1000, LastSeenMs: 2000}}}}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(old).WithObjects(old).Build()
	m := NewMeter("new", time.UnixMilli(2500))
	for i := 0; i < MaxMeterKeys; i++ {
		m.Record("other", strconv.Itoa(i), "lab", time.UnixMilli(2000), false, 0, 0)
	}
	w := &ReportWriter{Reader: store, Writer: store, Meter: m, Instance: "pod"}
	err := w.Publish(context.Background(), "target", time.UnixMilli(3000))
	var got laboratoryv1alpha1.LabTrafficReport
	_ = store.Get(context.Background(), types.NamespacedName{Namespace: "target", Name: ReportName("pod")}, &got)
	if err == nil || len(got.Status.Ledger) != 1 || got.Status.Ledger[0].Attempts != 8 || got.Status.Ledger[0].BytesIn != 80 || got.Status.CoveredToMs != 2000 {
		t.Fatal("saturated restore destroyed baseline", err, got.Status)
	}
	m.RetireNamespaces(map[string]bool{"target": true})
	m.Record("target", "p", "lab", time.UnixMilli(3000), true, 1, 0)
	for i := 0; i < 2; i++ {
		if err := w.Publish(context.Background(), "target", time.UnixMilli(3500)); err != nil {
			t.Fatal(err)
		}
	}
	_ = store.Get(context.Background(), types.NamespacedName{Namespace: "target", Name: ReportName("pod")}, &got)
	if len(got.Status.Ledger) != 1 || got.Status.Ledger[0].Attempts != 9 || got.Status.Ledger[0].BytesIn != 81 {
		t.Fatal("retry lost or doubled progress", got.Status)
	}
}
