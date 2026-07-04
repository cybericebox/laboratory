package grpc

import (
	"context"
	"encoding/json"
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func TestCreateAndGetLab(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	mustNamespace(t, k8s, "team-lab") // helper: create a namespace via corev1 client on the same rest.Config

	spec := laboratoryv1alpha1.LabSpec{}
	spec.VPN.Enabled = true
	specJSON, _ := json.Marshal(spec)

	_, err := h.CreateLab(ctx, &protobuf.Lab{Namespace: "team-lab", Name: "ctf1", SpecJson: specJSON})
	if err != nil {
		t.Fatalf("CreateLab: %v", err)
	}
	got, err := h.GetLab(ctx, &protobuf.NamespacedIDRequest{Namespace: "team-lab", Name: "ctf1"})
	if err != nil {
		t.Fatalf("GetLab: %v", err)
	}
	if got.Name != "ctf1" {
		t.Errorf("got %q", got.Name)
	}
}
