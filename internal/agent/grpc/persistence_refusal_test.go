package grpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodeagent"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func persistentRequest(persistence bool) *protobuf.CreateLabsRequest {
	spec := laboratoryv1alpha1.LabSpec{}
	spec.VPN.Enabled = true
	d := laboratoryv1alpha1.DeviceTemplate{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx"}
	if persistence {
		d.Persistence = &laboratoryv1alpha1.DevicePersistence{Enabled: true}
	}
	spec.Devices = []laboratoryv1alpha1.DeviceTemplate{d}
	raw, _ := json.Marshal(spec)
	return &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: raw}},
		Items:    []*protobuf.LabItem{{Name: "a", LabGroup: "g", VariantId: "v"}},
	}
}

func setNodeAnnotations(t *testing.T, h *Handler, name string, ann map[string]string) {
	t.Helper()
	ctx := context.Background()
	n, err := h.k8s.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n.Annotations = ann
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	n.Labels[names.LabelNodeAgentReady] = "true"
	if _, err := h.k8s.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A lab that asks for persistence is refused (PERSISTENCE_UNAVAILABLE, nothing created) when the lab nodes cannot keep state; a lab that
// does not ask is untouched; once the nodes confirm, the lab is created. The refusal carries the fixed reason only.
func TestCreateLabWithPersistenceNeedsTheNodesToKeepIt(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "g", "g", nil)
	h.features.StatePersistence = true
	h.statePersistence = true
	addNode(t, k, "secret-node-7", map[string]string{})
	setNodeAnnotations(t, h, "secret-node-7", map[string]string{
		names.AnnotationStatePersistence:   nodeagent.Unavailable(nodeagent.CodeContainerdRootMissing, "the containerd root /var/lib/k0s/containerd does not exist"),
		names.AnnotationRuntimeObservation: nodeagent.PreflightReady,
	})

	res, err := h.CreateLabs(ctx, persistentRequest(true))
	wantStates(t, res, err, stFailed)
	wantProfileReason(t, res, client.ReasonPersistenceUnavailable)
	if msg := res.GetResults()[0].GetError(); !strings.Contains(msg, "CONTAINERD_ROOT_MISSING:1/1") || strings.Contains(msg, "secret-node-7") || strings.Contains(msg, "/var/lib") {
		t.Errorf("the refusal carries the fixed reason only: %q", msg)
	}
	if _, err := h.cs.LaboratoryV1alpha1().Labs("g").Get(ctx, "a", metav1.GetOptions{}); err == nil {
		t.Fatal("a refused lab must not be created")
	}

	// a lab with no persistence is not affected
	res, err = h.CreateLabs(ctx, persistentRequest(false))
	wantStates(t, res, err, stCreated)
	if err := h.cs.LaboratoryV1alpha1().Labs("g").Delete(ctx, "a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	// the nodes confirm: created
	setNodeAnnotations(t, h, "secret-node-7", map[string]string{names.AnnotationStatePersistence: nodeagent.PreflightReady, names.AnnotationRuntimeObservation: nodeagent.PreflightReady})
	res, err = h.CreateLabs(ctx, persistentRequest(true))
	wantStates(t, res, err, stCreated)
}

// The start of a lab with persistence is refused the same way.
func TestStartOfALabWithPersistenceNeedsTheNodesToKeepIt(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	h.features.StatePersistence = true
	addNode(t, k, "n1", map[string]string{})
	setNodeAnnotations(t, h, "n1", map[string]string{names.AnnotationStatePersistence: nodeagent.Unavailable(nodeagent.CodeSnapshotRegistry, "x"), names.AnnotationRuntimeObservation: nodeagent.PreflightReady})
	withP := &laboratoryv1alpha1.Lab{Spec: laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{{Name: "w", Persistence: &laboratoryv1alpha1.DevicePersistence{Enabled: true}}}}}
	without := &laboratoryv1alpha1.Lab{Spec: laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{{Name: "w"}}}}
	err := h.refuseStartWithoutPersistence(ctx, withP)
	if err == nil || reasonOf(err) != client.ReasonPersistenceUnavailable || !strings.Contains(err.Error(), "SNAPSHOT_REGISTRY_UNREACHABLE:1/1") {
		t.Fatalf("start with persistence: %v", err)
	}
	if err := h.refuseStartWithoutPersistence(ctx, without); err != nil {
		t.Fatalf("a lab with no persistence starts: %v", err)
	}
	h.features.StatePersistence = false // the cluster is not configured for it: this check is not the one that refuses
	if err := h.refuseStartWithoutPersistence(ctx, withP); err != nil {
		t.Fatalf("not configured: %v", err)
	}
}
