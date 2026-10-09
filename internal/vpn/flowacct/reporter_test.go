package flowacct

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestPublishCreatesThenUpdatesTheReportAndAdvancesCoverage(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&laboratoryv1alpha1.LabTrafficReport{}).Build()

	src := &fakeSource{flows: []Flow{flow(1, clientA, labIP, 22)}}
	collector := newCollector(src)
	src.flows[0].Replied = true
	src.flows[0].Mark = 0x80000100
	reporter := &Reporter{Reader: c, Writer: c, Namespace: "labgroup-x", Instance: "vpn-1", Collector: collector}

	observe(t, collector, sample(1), t0)
	_ = collector.Poll(t0)
	if err := reporter.Publish(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	// An idle minute: nothing new, but the covered span moves on.
	for d := 5 * time.Second; d <= time.Minute; d += 5 * time.Second {
		observe(t, collector, sample(1), t0.Add(d))
	}
	_ = collector.Poll(t0.Add(time.Minute))
	if err := reporter.Publish(context.Background(), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	var got laboratoryv1alpha1.LabTrafficReport
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "labgroup-x", Name: ReportName}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Kind != laboratoryv1alpha1.LabTrafficSurfaceVPN || got.Spec.Instance != "vpn-1" {
		t.Fatalf("spec = %+v", got.Spec)
	}
	if got.Status.BootID != "boot-1" || got.Status.CoveredFromMs != t0.UnixMilli() || got.Status.CoveredToMs != t0.Add(time.Minute).UnixMilli() {
		t.Fatalf("status = %+v", got.Status)
	}
	if len(got.Status.Ledger) != 1 {
		t.Fatalf("ledger = %+v", got.Status.Ledger)
	}
	row := got.Status.Ledger[0]
	if row.Subject != "p-a" || row.LabName != "c-1" || row.Attempts != 1 || row.FirstRespondedMs != t0.UnixMilli() {
		t.Fatalf("row = %+v", row)
	}
}

func TestReporterRestoresPrivateCheckpointWithoutRecount(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = laboratoryv1alpha1.AddToScheme(scheme)
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&laboratoryv1alpha1.LabTrafficReport{}).Build()
	first := newCollector(&fakeSource{})
	observe(t, first, sample(7), t0)
	writer := &Reporter{Reader: store, Writer: store, Namespace: "g", Collector: first}
	if err := writer.Publish(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	second := newCollector(&fakeSource{})
	restored := []PairCounters{}
	next := &Reporter{Reader: store, Writer: store, Namespace: "g", Collector: second, OnResume: func(rows []PairCounters) { restored = rows }}
	if err := next.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	observe(t, second, sample(7), t0.Add(time.Second))
	if len(restored) != 1 || second.Snapshot(t0).Ledger[0].PacketsOut != 7 {
		t.Fatal("private checkpoint not resumed")
	}
}

func TestTelemetryPreservesIndependentCurrentVPNBootAndRejectsOldWriter(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = laboratoryv1alpha1.AddToScheme(scheme)
	current := &laboratoryv1alpha1.VPNBootRecord{VPNRuntimeIdentity: laboratoryv1alpha1.VPNRuntimeIdentity{BootID: "current", PodName: "vpn", PodUID: "uid", ContainerID: "containerd://1"}, GroupUID: "group", PublishedAt: metav1.Now()}
	obj := &laboratoryv1alpha1.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: ReportName, Namespace: "ns"}, Spec: laboratoryv1alpha1.LabTrafficReportSpec{Kind: laboratoryv1alpha1.LabTrafficSurfaceVPN, Instance: "vpn"}, Status: laboratoryv1alpha1.LabTrafficReportStatus{CurrentVPNRuntime: current}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(obj).WithObjects(obj).Build()
	ctx := context.Background()
	key := types.NamespacedName{Name: ReportName, Namespace: "ns"}
	if err := PublishReport(ctx, c, c, key, obj.Spec, laboratoryv1alpha1.LabTrafficReportStatus{BootID: "current", CoveredToMs: 123}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, obj); err != nil {
		t.Fatal(err)
	}
	if obj.Status.CurrentVPNRuntime == nil || obj.Status.CurrentVPNRuntime.BootID != "current" || obj.Status.CoveredToMs != 123 {
		t.Fatal("telemetry replaced startup binding", obj.Status)
	}
	if err := PublishReport(ctx, c, c, key, obj.Spec, laboratoryv1alpha1.LabTrafficReportStatus{BootID: "superseded", CoveredToMs: 124}); err == nil {
		t.Fatal("old process telemetry overwrote new boot witness")
	}
}
