package grpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
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

// A policy naming a Lab the group no longer has (the destroy of a test lab sends the rest of the author's labs) is refused
// with LAB_NOT_IN_GROUP naming every such lab; the policy the caller sends without them applies.
func TestAccessPolicyWithMissingLabsHasReason(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "g", "g", nil)
	res, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON("web")}}, Items: []*protobuf.LabItem{{Name: "alive", LabGroup: "g", VariantId: "v"}}})
	wantStates(t, res, err, stCreated)
	policy := func(labsIn ...string) *protobuf.SetLabGroupAccessRequest {
		return &protobuf.SetLabGroupAccessRequest{Policies: []*protobuf.LabGroupAccessPolicy{{LabGroupName: "g", Rules: []*protobuf.LabGroupAccessRule{{Action: protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW, ClientNames: []string{"c1"}, LabNames: labsIn}}}}}
	}
	res, err = h.SetLabGroupAccess(ctx, policy("alive", "gone1", "gone2"))
	wantStates(t, res, err, stFailed)
	if r := res.Results[0]; r.Reason != client.ReasonLabNotInGroup || r.Retryable || !strings.Contains(r.Error, `"gone1", "gone2"`) {
		t.Fatalf("got %+v", r)
	}
	res, err = h.SetLabGroupAccess(ctx, policy("alive"))
	wantStates(t, res, err, stCreated)
}

// A pending child admission nobody repeats must not block the group for good: a newer create of the same Lab supersedes
// the older attempt that wrote nothing, and a leftover whose Lab write is durable is cleared by the next claim.
func TestPendingAdmissionLeftoverDoesNotBlockGroup(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "g", "g", nil)
	group, _ := h.getGroup(ctx, "g")
	mk := func(name string, devs ...string) *protobuf.CreateLabsRequest {
		return &protobuf.CreateLabsRequest{Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON(devs...)}}, Items: []*protobuf.LabItem{{Name: name, LabGroup: "g", VariantId: "v"}}}
	}
	setPending := func(a *lab.GroupChildAdmission) {
		t.Helper()
		g, err := h.getGroup(ctx, "g")
		if err != nil {
			t.Fatal(err)
		}
		a.Token = admissionToken(a)
		g.Spec.Admission = a
		if _, err := h.cs.LaboratoryV1alpha1().LabGroups().Update(ctx, g, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// Never written, for the same Lab name: superseded.
	setPending(&lab.GroupChildAdmission{GroupUID: string(group.UID), LabName: "a", DesiredState: lifecycleRunning, SpecHash: "old-hash"})
	res, err := h.CreateLabs(ctx, mk("a", "web"))
	wantStates(t, res, err, stCreated)

	// Never written, for another Lab: it keeps waiting for its own retry.
	setPending(&lab.GroupChildAdmission{GroupUID: string(group.UID), LabName: "ghost", DesiredState: lifecycleRunning, SpecHash: "ghost-hash"})
	res, err = h.CreateLabs(ctx, mk("b", "web"))
	wantStates(t, res, err, stFailed)
	if !res.Results[0].Retryable {
		t.Fatalf("pending admission is retryable: %+v", res.Results[0])
	}

	// Written (the Lab "a" exists with that hash) but never cleared: cleared by the next claim.
	cur, err := h.cs.LaboratoryV1alpha1().Labs("g").Get(ctx, "a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	setPending(&lab.GroupChildAdmission{GroupUID: string(group.UID), LabName: "a", DesiredState: lifecycleRunning, SpecHash: cur.Annotations[names.AnnotationSpecHash]})
	res, err = h.CreateLabs(ctx, mk("b", "web"))
	wantStates(t, res, err, stCreated)
}

// A Lab whose birth receipt was never committed (the creator died after the Create) can be deleted once the grace has
// passed; until then the delete is refused, because the commit may still come.
func TestDeleteLabWithUncommittedBirthAfterGrace(t *testing.T) {
	h, k := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k, "g", "g", nil)
	group, _ := h.getGroup(ctx, "g")
	res, err := h.CreateLabs(ctx, birthRequest(t, string(group.UID), "0198c0a4-7a41-7000-8000-000000000001", "web"))
	wantStates(t, res, err, stCreated)
	labs := h.cs.LaboratoryV1alpha1().Labs("g")
	cur, _ := labs.Get(ctx, "a", metav1.GetOptions{})
	var receipt lab.LabCreationReceipt
	if err := json.Unmarshal([]byte(cur.Annotations[names.AnnotationLabCreation]), &receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Committed = false
	raw, _ := json.Marshal(receipt)
	cur.Annotations[names.AnnotationLabCreation] = string(raw)
	if _, err := labs.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	del := &protobuf.DeleteRequest{Items: []*protobuf.ItemRef{{LabGroup: "g", Name: "a"}}}
	res, err = h.DeleteLabs(ctx, del)
	wantStates(t, res, err, stFailed)

	old := birthCommitGrace
	birthCommitGrace = 0
	t.Cleanup(func() { birthCommitGrace = old })
	res, err = h.DeleteLabs(ctx, del)
	wantStates(t, res, err, stDeleted)
}
