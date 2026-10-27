package grpc

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func birthRequest(t *testing.T, groupUID, op string, devices ...string) *protobuf.CreateLabsRequest {
	t.Helper()
	spec := json.RawMessage(specJSON(devices...))
	var m map[string]any
	if err := json.Unmarshal(spec, &m); err != nil {
		t.Fatal(err)
	}
	m["lifecycle"] = map[string]any{"desiredState": "Running", "operationId": op, "revision": 1}
	raw, _ := json.Marshal(m)
	return &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: raw}},
		Items:    []*protobuf.LabItem{{Name: "a", LabGroup: "g", VariantId: "v", ExpectedGroupUid: groupUID}},
	}
}

// A stale Lab of an older operation holding the name is replaced: the first create answers with a retryable
// STALE_LAB_REPLACING, the repeated one creates the Lab of the new generation.
func TestCreateLabReplacesStaleLabOfOlderOperation(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "g", "g", nil)
	group, err := h.getGroup(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	oldOp, newOp := "0198c0a4-7a41-7000-8000-000000000001", "0198c0a4-7a41-7000-8000-000000000002"
	res, err := h.CreateLabs(ctx, birthRequest(t, string(group.UID), oldOp, "web"))
	wantStates(t, res, err, stCreated)

	// The same name, a newer operation and another spec (the stale CR of khctf).
	req := birthRequest(t, string(group.UID), newOp, "web", "db")
	res, err = h.CreateLabs(ctx, req)
	wantStates(t, res, err, stFailed)
	if r := res.Results[0]; !r.Retryable || r.Reason != client.ReasonStaleLabReplacing {
		t.Fatalf("want a retryable stale-replacing answer, got %+v", r)
	}
	labs := h.cs.LaboratoryV1alpha1().Labs("g")
	// envtest has no controllers: the Lab disappears when its finalizers are gone.
	if cur, err := labs.Get(ctx, "a", metav1.GetOptions{}); err == nil {
		cur.Finalizers = nil
		if _, err := labs.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	res, err = h.CreateLabs(ctx, req)
	wantStates(t, res, err, stCreated)
	cur, err := labs.Get(ctx, "a", metav1.GetOptions{})
	if err != nil || len(cur.Spec.Devices) != 2 || cur.Spec.Lifecycle.OperationID != newOp {
		t.Fatalf("the Lab of the new generation is expected: %+v %v", cur, err)
	}
}

// The Lab of a newer operation is never replaced by a late request of an older one.
func TestCreateLabKeepsNewerLab(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "g", "g", nil)
	group, _ := h.getGroup(ctx, "g")
	oldOp, newOp := "0198c0a4-7a41-7000-8000-000000000001", "0198c0a4-7a41-7000-8000-000000000002"
	res, err := h.CreateLabs(ctx, birthRequest(t, string(group.UID), newOp, "web"))
	wantStates(t, res, err, stCreated)
	res, err = h.CreateLabs(ctx, birthRequest(t, string(group.UID), oldOp, "web", "db"))
	wantStates(t, res, err, stFailed)
	if r := res.Results[0]; r.Retryable || r.Reason != client.ReasonLabNameConflict {
		t.Fatalf("want LAB_NAME_CONFLICT, got %+v", r)
	}
	cur, err := h.cs.LaboratoryV1alpha1().Labs("g").Get(ctx, "a", metav1.GetOptions{})
	if err != nil || cur.DeletionTimestamp != nil || len(cur.Spec.Devices) != 1 {
		t.Fatalf("the newer Lab must stay: %+v %v", cur, err)
	}
}

// A different spec without a comparable generation keeps failing, now with a distinct code.
func TestDifferentSpecCarriesNameConflictReason(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "g", "g", nil)
	mk := func(devs ...string) *protobuf.CreateLabsRequest {
		return &protobuf.CreateLabsRequest{Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON(devs...)}}, Items: []*protobuf.LabItem{{Name: "a", LabGroup: "g", VariantId: "v"}}}
	}
	res, err := h.CreateLabs(ctx, mk("web"))
	wantStates(t, res, err, stCreated)
	res, err = h.CreateLabs(ctx, mk("web", "db"))
	wantStates(t, res, err, stFailed)
	if res.Results[0].Reason != client.ReasonLabNameConflict || res.Results[0].Retryable {
		t.Fatalf("got %+v", res.Results[0])
	}
	_ = lab.Lab{}
}
