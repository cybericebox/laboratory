package grpc

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func TestSnapshotRelaysTrafficReportsByNamespaceGroup(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	const namespace = "team-traffic"
	mustNamespace(t, k8s, namespace)

	group, err := h.cs.LaboratoryV1alpha1().LabGroups().Create(ctx, &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "e-1-t-2"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	group.Status.Namespace = namespace
	if _, err := h.cs.LaboratoryV1alpha1().LabGroups().UpdateStatus(ctx, group, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	report, err := h.cs.LaboratoryV1alpha1().LabTrafficReports(namespace).Create(ctx, &laboratoryv1alpha1.LabTrafficReport{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: namespace},
		Spec:       laboratoryv1alpha1.LabTrafficReportSpec{Kind: laboratoryv1alpha1.LabTrafficSurfaceVPN, Instance: "vpn-abc"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	report.Status = laboratoryv1alpha1.LabTrafficReportStatus{
		BootID: "boot-1", CoveredFromMs: 1000, CoveredToMs: 2000,
		Ledger: []laboratoryv1alpha1.LabTrafficTouch{{
			Subject: "p-u1", LabName: "c-9", DstIP: "10.9.1.5", Proto: "tcp", DstPort: 22, Attempts: 3,
			BytesIn: 10, FirstSeenMs: 1100, LastSeenMs: 1900, FirstRespondedMs: 1200,
		}},
	}
	if _, err := h.cs.LaboratoryV1alpha1().LabTrafficReports(namespace).UpdateStatus(ctx, report, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	update, err := h.snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got *protobuf.TrafficReport
	for _, traffic := range update.GetTraffic() {
		if traffic.GetNamespace() == namespace {
			got = traffic
		}
	}
	if got == nil {
		t.Fatalf("no traffic report in snapshot: %+v", update.GetTraffic())
	}
	if got.GetLabGroupName() != "e-1-t-2" || got.GetKind() != "vpn" || got.GetBootId() != "boot-1" || got.GetCoveredToUnixMs() != 2000 {
		t.Fatalf("report header = %+v", got)
	}
	touch := got.GetLedger()[0]
	if touch.GetSubject() != "p-u1" || touch.GetLabName() != "c-9" || touch.GetDstPort() != 22 || touch.GetAttempts() != 3 ||
		touch.GetFirstSeenUnixMs() != 1100 || touch.GetLastSeenUnixMs() != 1900 || touch.GetFirstRespondedUnixMs() != 1200 {
		t.Fatalf("touch = %+v", touch)
	}
}

func TestMonitoringDeltaSendsChangedTrafficOnly(t *testing.T) {
	report := func(coveredTo int64) *protobuf.TrafficReport {
		return &protobuf.TrafficReport{LabGroupName: "g", Namespace: "ns", Source: "vpn", CoveredToUnixMs: coveredTo}
	}
	previous := &protobuf.MonitoringUpdate{Traffic: []*protobuf.TrafficReport{report(1)}}

	if _, changed := monitoringDelta(previous, &protobuf.MonitoringUpdate{Traffic: []*protobuf.TrafficReport{report(1)}}); changed {
		t.Fatal("an identical report must not produce a delta")
	}
	delta, changed := monitoringDelta(previous, &protobuf.MonitoringUpdate{Traffic: []*protobuf.TrafficReport{report(2)}})
	if !changed || len(delta.GetTraffic()) != 1 || delta.GetTraffic()[0].GetCoveredToUnixMs() != 2 {
		t.Fatalf("delta = %+v", delta)
	}
	gone, changed := monitoringDelta(previous, &protobuf.MonitoringUpdate{})
	if !changed || len(gone.GetDeletedKeys()) != 1 || gone.GetDeletedKeys()[0].GetKind() != "traffic_report" {
		t.Fatalf("deleted = %+v", gone)
	}
}
