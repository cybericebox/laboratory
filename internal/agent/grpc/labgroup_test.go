package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// The LabGroup RPC family end to end on a real API server.
func TestLabGroupsEndToEnd(t *testing.T) {
	h, _ := newTestHandler(t)
	ctx := context.Background()
	groups := h.cs.LaboratoryV1alpha1().LabGroups()

	// Create: request labels apply to every item, the item's own win; the scheduling
	// fields become the internal label and annotation.
	deployGroup := "0198c0a4-7a41-7000-8000-000000000001"
	req := &protobuf.CreateLabGroupsRequest{
		Labels: map[string]string{"event": "e1", "tier": "common"},
		Items: []*protobuf.LabGroupItem{
			{Name: "g-a", Labels: map[string]string{"tier": "own"}, DeployGroup: deployGroup, DeployAfter: []string{"first", deployGroup}},
			{Name: "g-b"},
		},
	}
	res, err := h.CreateLabGroups(ctx, req)
	wantStates(t, res, err, stCreated, stCreated)
	a, _ := groups.Get(ctx, "g-a", metav1.GetOptions{})
	b, _ := groups.Get(ctx, "g-b", metav1.GetOptions{})
	if a.Labels["tier"] != "own" || a.Labels["event"] != "e1" || b.Labels["tier"] != "common" {
		t.Fatalf("labels: %v %v", a.Labels, b.Labels)
	}
	if a.Labels[names.LabelDeployGroup] != deployKey(deployGroup) || a.Annotations[names.AnnotationDeployAfter] != deployKey("first")+","+deployKey(deployGroup) {
		t.Fatalf("scheduling metadata: %v %v", a.Labels, a.Annotations)
	}
	if _, ok := b.Labels[names.LabelDeployGroup]; ok {
		t.Fatal("no deploy group, no label")
	}

	// Idempotency: the same request is EXISTS; one more label is UPDATED; another spec FAILED.
	res, err = h.CreateLabGroups(ctx, req)
	wantStates(t, res, err, stExists, stExists)
	req.Items[1].Labels = map[string]string{"extra": "1"}
	res, err = h.CreateLabGroups(ctx, req)
	wantStates(t, res, err, stExists, stUpdated)
	req.Items[1].Suspended = true
	res, err = h.CreateLabGroups(ctx, req)
	wantStates(t, res, err, stExists, stFailed)
	if res.Results[1].Retryable {
		t.Fatal("a different spec is not worth repeating")
	}

	// Update by explicit items: request-level and per-item changes.
	res, err = h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{
		Changes: &protobuf.LabGroupChanges{Labels: &protobuf.LabelChanges{Set: map[string]string{"phase": "one"}, Remove: []string{"event"}}},
		Items: []*protobuf.UpdateLabGroupItem{
			{Name: "g-a", Changes: &protobuf.LabGroupChanges{Suspended: proto.Bool(true), Labels: &protobuf.LabelChanges{Set: map[string]string{"phase": "two"}}}},
			{Name: "g-b", Changes: &protobuf.LabGroupChanges{VpnDisabled: proto.Bool(true)}},
			{Name: "missing"},
		},
	})
	wantStates(t, res, err, stUpdated, stUpdated, stNotFound)
	a, _ = groups.Get(ctx, "g-a", metav1.GetOptions{})
	b, _ = groups.Get(ctx, "g-b", metav1.GetOptions{})
	if !a.Spec.Suspended || a.Labels["phase"] != "two" || b.Labels["phase"] != "one" || b.Spec.Suspended || !b.Spec.VPN.Disabled {
		t.Fatalf("update: %+v %+v", a, b)
	}
	if _, ok := a.Labels["event"]; ok {
		t.Fatal("remove did not remove")
	}
	if a.Labels[names.LabelDeployGroup] == "" {
		t.Fatal("scheduling label must survive an update")
	}

	// Update by selector, with the expected_count guard: nothing changes on a mismatch.
	_, err = h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{
		BySelector: &protobuf.Selector{Selector: "phase in (one,two)", ExpectedCount: proto.Int64(1)},
		Changes:    &protobuf.LabGroupChanges{Suspended: proto.Bool(false)},
	})
	if got, ok := client.CountMismatch(apiErrorToStatus(err)); !ok || got != 2 || status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("guard: %v", err)
	}
	if a, _ = groups.Get(ctx, "g-a", metav1.GetOptions{}); !a.Spec.Suspended {
		t.Fatal("a refused call must do nothing")
	}
	res, err = h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{
		BySelector: &protobuf.Selector{Selector: "phase in (one,two)", ExpectedCount: proto.Int64(2)},
		Changes:    &protobuf.LabGroupChanges{Suspended: proto.Bool(false)},
	})
	wantStates(t, res, err, stUpdated, stUpdated)
	if res.Results[0].Ref.Name != "g-a" || res.Results[1].Ref.Name != "g-b" {
		t.Fatalf("selector results are sorted by name: %v", res.Results)
	}

	// List by names, by selector, everything.
	list, err := h.ListLabGroups(ctx, &protobuf.ListRequest{Items: []*protobuf.ItemRef{{Name: "g-a"}, {Name: "missing"}}})
	if err != nil || len(list.Items) != 1 || list.Items[0].Name != "g-a" {
		t.Fatalf("list names: %v %v", list, err)
	}
	list, _ = h.ListLabGroups(ctx, &protobuf.ListRequest{Selector: "extra=1"})
	if len(list.Items) != 1 || list.Items[0].Name != "g-b" {
		t.Fatalf("list selector: %v", list)
	}
	if list, _ = h.ListLabGroups(ctx, &protobuf.ListRequest{}); len(list.Items) != 2 {
		t.Fatalf("list all: %v", list)
	}

	// Delete: a selector needs to be non-empty; the guard holds; then it deletes.
	if _, err = h.DeleteLabGroups(ctx, &protobuf.DeleteRequest{BySelector: &protobuf.Selector{}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty selector: %v", err)
	}
	if _, err = h.DeleteLabGroups(ctx, &protobuf.DeleteRequest{BySelector: &protobuf.Selector{Selector: "tier", ExpectedCount: proto.Int64(1)}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("guard: %v", err)
	}
	if list, _ = h.ListLabGroups(ctx, &protobuf.ListRequest{}); len(list.Items) != 2 {
		t.Fatal("a refused delete deleted something")
	}
	res, err = h.DeleteLabGroups(ctx, &protobuf.DeleteRequest{BySelector: &protobuf.Selector{Selector: "tier", ExpectedCount: proto.Int64(2)}})
	wantStates(t, res, err, stDeleted, stDeleted)
	res, err = h.DeleteLabGroups(ctx, &protobuf.DeleteRequest{Items: []*protobuf.ItemRef{{Name: "g-a"}}})
	wantStates(t, res, err, stNotFound)
}
