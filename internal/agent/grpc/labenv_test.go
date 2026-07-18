package grpc

import (
	"context"
	"encoding/json"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func TestReconcileEnvSecrets(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	mustNamespace(t, k8s, "team-env")

	spec := laboratoryv1alpha1.LabSpec{
		Devices: []laboratoryv1alpha1.DeviceTemplate{
			{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer},
		},
	}
	specJSON, _ := json.Marshal(spec)

	// Create with a per-device env var (a flag).
	if _, err := h.CreateLab(ctx, &protobuf.Lab{
		Namespace: "team-env", Name: "ctf1", SpecJson: specJSON,
		Env: []*protobuf.DeviceEnv{{Device: "web", Vars: map[string]string{"FLAG": "CTF{x}"}}},
	}); err != nil {
		t.Fatalf("CreateLab: %v", err)
	}

	s, err := k8s.CoreV1().Secrets("team-env").Get(ctx, "ctf1-web-env", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("env secret not created: %v", err)
	}
	if string(s.Data["FLAG"]) != "CTF{x}" {
		t.Errorf("flag wrong: %q", s.Data["FLAG"])
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Kind != "Lab" {
		t.Errorf("expected Lab owner reference, got %+v", s.OwnerReferences)
	}

	// Overwrite (delete+create, no read).
	if _, err := h.UpdateLab(ctx, &protobuf.Lab{
		Namespace: "team-env", Name: "ctf1", SpecJson: specJSON,
		Env: []*protobuf.DeviceEnv{{Device: "web", Vars: map[string]string{"FLAG": "CTF{y}"}}},
	}); err != nil {
		t.Fatalf("UpdateLab: %v", err)
	}
	s, _ = k8s.CoreV1().Secrets("team-env").Get(ctx, "ctf1-web-env", metav1.GetOptions{})
	if string(s.Data["FLAG"]) != "CTF{y}" {
		t.Errorf("overwrite failed: %q", s.Data["FLAG"])
	}

	// Dropping the env clears the secret.
	if _, err := h.UpdateLab(ctx, &protobuf.Lab{
		Namespace: "team-env", Name: "ctf1", SpecJson: specJSON,
	}); err != nil {
		t.Fatalf("UpdateLab(no env): %v", err)
	}
	if _, err := k8s.CoreV1().Secrets("team-env").Get(ctx, "ctf1-web-env", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected env secret deleted, err=%v", err)
	}
}
