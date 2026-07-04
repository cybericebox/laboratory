package grpc

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func TestCreateAndGetLabGroup(t *testing.T) {
	h := newTestHandler(t) // sets up envtest + versioned clientset; see handler_test_setup.go
	ctx := context.Background()

	_, err := h.CreateLabGroup(ctx, &protobuf.LabGroup{Name: "team-x"})
	if err != nil {
		t.Fatalf("CreateLabGroup: %v", err)
	}
	got, err := h.GetLabGroup(ctx, &protobuf.IDRequest{Name: "team-x"})
	if err != nil {
		t.Fatalf("GetLabGroup: %v", err)
	}
	if got.Name != "team-x" {
		t.Errorf("got %q", got.Name)
	}
	_ = metav1.Now
}
