package l7

import (
	"context"
	"testing"
	"time"

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
	meter.Record("ns-busy", "user-1", "c-1", "web", time.UnixMilli(2_000), true, 7, 3)

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
