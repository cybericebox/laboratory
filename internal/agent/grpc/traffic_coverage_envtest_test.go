package grpc

import (
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"testing"
	"time"

	labv1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	grpcrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The real API server must preserve the collector/CR contract, and the actual
// gRPC decoder must see public facts only. This is not a live kernel WG test.
func TestCollectorCoverageSurvivesRealCRDAndRPC(t *testing.T) {
	h, k8s := newTestHandler(t)
	h.SetMonitoringConfig(MonitoringConfig{PollInterval: 20 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ns := "coverage-wire"
	mustNamespace(t, k8s, ns)
	groups := h.cs.LaboratoryV1alpha1().LabGroups()
	g, err := groups.Create(ctx, &labv1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g", Labels: map[string]string{names.LabelTenant: names.DefaultTenant}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	g.Status.Namespace = ns
	if _, err = groups.UpdateStatus(ctx, g, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = h.cs.LaboratoryV1alpha1().Labs(ns).Create(ctx, &labv1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: ns, Annotations: map[string]string{names.AnnotationID: "original lab"}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = h.cs.LaboratoryV1alpha1().LabGroupClients(ns).Create(ctx, &labv1.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns, Annotations: map[string]string{names.AnnotationID: "participant"}}, Spec: labv1.LabGroupClientSpec{PublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	status := flowacct.ToStatus(flowacct.Report{BootID: "new", CoveredFromMs: 4000, CoveredToMs: 5000, Truncated: true,
		Ledger:            []flowacct.Touch{{Key: flowacct.Key{Subject: "p", Lab: "lab"}, Attempts: 2, LabInitiatedAttempts: 3, PacketsOut: 5, PacketsIn: 7, BytesOut: 11, BytesIn: 13, FirstSeenMs: 4100, LastSeenMs: 4900, FirstRespondMs: 4200}},
		KernelCheckpoints: []flowacct.PairCounters{{Key: flowacct.Key{Subject: "p", Lab: "lab"}, BindingID: "private-binding", Epoch: "private-epoch", Attempts: 2, LabInitiatedAttempts: 3}},
	})
	status.CoverageSpans = []labv1.LabTrafficCoverageSpan{{FromMs: 1000, ToMs: 2000, Source: "vpn", Instance: "writer", BootID: "old"}, {FromMs: 4000, ToMs: 5000, Source: "vpn", Instance: "writer", BootID: "new"}}
	reports := h.cs.LaboratoryV1alpha1().LabTrafficReports(ns)
	r, err := reports.Create(ctx, &labv1.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: ns}, Spec: labv1.LabTrafficReportSpec{Kind: labv1.LabTrafficSurfaceVPN, Instance: "writer"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r.Status = status
	if _, err = reports.UpdateStatus(ctx, r, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	persisted, err := reports.Get(ctx, "vpn", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Status.CoverageSpans) != 2 || len(persisted.Status.KernelCheckpoints) != 1 {
		t.Fatal("CRD pruned public coverage or private checkpoint")
	}
	listener := bufconn.Listen(1 << 20)
	server := grpcrpc.NewServer(grpcrpc.WaitForHandlers(true))
	protobuf.RegisterLabManagerServer(server, h)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() { cancel(); server.Stop(); listener.Close(); <-serveDone; h.monitor().stopCache() })
	conn, err := grpcrpc.NewClient("passthrough:///coverage", grpcrpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpcrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := protobuf.NewLabManagerClient(conn).Monitoring(ctx, &protobuf.MonitoringRequest{})
	if err != nil {
		t.Fatal(err)
	}
	update, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if !update.Snapshot || len(update.Traffic) != 1 {
		t.Fatalf("snapshot traffic=%v", update.Traffic)
	}
	got := update.Traffic[0]
	if len(got.CoverageSpans) != 2 || got.CoverageSpans[0].FromUnixMs != 1000 || got.CoverageSpans[0].ToUnixMs != 2000 || got.CoverageSpans[1].FromUnixMs != 4000 || got.CoverageSpans[1].ToUnixMs != 5000 || !got.Partial {
		t.Fatalf("coverage gap/completeness lost: %v", got)
	}
	row := got.Ledger[0]
	if row.Subject != "participant" || row.LabName != "original lab" || row.Attempts != 2 || row.LabInitiatedAttempts != 3 || row.PacketsOut != 5 || row.PacketsIn != 7 || row.BytesOut != 11 || row.BytesIn != 13 || row.FirstSeenUnixMs != 4100 || row.LastSeenUnixMs != 4900 || row.FirstRespondedUnixMs != 4200 {
		t.Fatalf("counter/ID/time contract changed: %v", row)
	}
	raw, err := proto.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("private-binding")) || bytes.Contains(raw, []byte("private-epoch")) {
		t.Fatal("private checkpoints leaked on RPC")
	}
}
