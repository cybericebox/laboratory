package grpc

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func TestCreateLabGroupClient(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	mustNamespace(t, k8s, "team-cli")

	_, err := h.CreateLabGroupClient(ctx, &protobuf.LabGroupClient{
		Namespace: "team-cli", Name: "alice", PublicKey: "pk",
	})
	if err != nil {
		t.Fatalf("CreateLabGroupClient: %v", err)
	}
	got, err := h.GetLabGroupClient(ctx, &protobuf.NamespacedIDRequest{Namespace: "team-cli", Name: "alice"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PublicKey != "pk" {
		t.Errorf("pk lost: %+v", got)
	}
}

// The VPN server writes WireGuard peer stats onto the CR status subresource;
// the agent must surface them through GetLabGroupClient. Envtest catches CRD
// status-schema drift that a pure converter test cannot.
func TestGetLabGroupClientStatistics(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	mustNamespace(t, k8s, "team-stats")

	if _, err := h.CreateLabGroupClient(ctx, &protobuf.LabGroupClient{
		Namespace: "team-stats", Name: "carol", PublicKey: "pk",
	}); err != nil {
		t.Fatalf("CreateLabGroupClient: %v", err)
	}

	cur, err := h.cs.LaboratoryV1alpha1().LabGroupClients("team-stats").Get(ctx, "carol", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cr: %v", err)
	}
	cur.Status.Statistics = laboratoryv1alpha1.LabGroupClientStatistics{
		LastHandshake: metav1.Unix(1700000000, 0),
		RxBytes:       1234,
		TxBytes:       5678,
	}
	if _, err := h.cs.LaboratoryV1alpha1().LabGroupClients("team-stats").UpdateStatus(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update status: %v", err)
	}

	got, err := h.GetLabGroupClient(ctx, &protobuf.NamespacedIDRequest{Namespace: "team-stats", Name: "carol"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Statistics == nil {
		t.Fatalf("statistics not surfaced through agent: %+v", got.Status)
	}
	if got.Status.Statistics.RxBytes != 1234 || got.Status.Statistics.TxBytes != 5678 {
		t.Errorf("stats bytes wrong: %+v", got.Status.Statistics)
	}
	if got.Status.Statistics.LastHandshakeUnix != 1700000000 {
		t.Errorf("last handshake wrong: %+v", got.Status.Statistics)
	}
}

func TestListAndDeleteLabGroupClient(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	mustNamespace(t, k8s, "team-cli-list")

	_, err := h.CreateLabGroupClient(ctx, &protobuf.LabGroupClient{
		Namespace: "team-cli-list", Name: "bob", PublicKey: "pk2",
	})
	if err != nil {
		t.Fatalf("CreateLabGroupClient: %v", err)
	}

	list, err := h.ListLabGroupClients(ctx, &protobuf.NamespaceRequest{Namespace: "team-cli-list"})
	if err != nil {
		t.Fatalf("ListLabGroupClients: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Name != "bob" {
		t.Fatalf("unexpected list: %+v", list.Items)
	}

	if _, err := h.DeleteLabGroupClient(ctx, &protobuf.NamespacedIDRequest{Namespace: "team-cli-list", Name: "bob"}); err != nil {
		t.Fatalf("DeleteLabGroupClient: %v", err)
	}

	if _, err := h.GetLabGroupClient(ctx, &protobuf.NamespacedIDRequest{Namespace: "team-cli-list", Name: "bob"}); err == nil {
		t.Fatalf("expected error getting deleted client, got nil")
	}
}
