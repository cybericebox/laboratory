package grpc

import (
	"context"
	"testing"

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
