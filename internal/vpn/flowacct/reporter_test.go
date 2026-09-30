package flowacct

import (
	"context"
	"testing"
	"time"

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
	reporter := &Reporter{Reader: c, Writer: c, Namespace: "labgroup-x", Instance: "vpn-1", Collector: collector}

	_ = collector.Poll(t0)
	if err := reporter.Publish(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	// An idle minute: nothing new, but the covered span moves on.
	_ = collector.Poll(t0.Add(5 * time.Second))
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
