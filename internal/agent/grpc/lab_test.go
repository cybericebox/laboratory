package grpc

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devnames "github.com/cybericebox/laboratory/internal/devices"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// The Lab RPC family end to end on a real API server.
func TestLabsEndToEnd(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k8s, "team-lab", "team-lab", nil)
	labs := h.cs.LaboratoryV1alpha1().Labs("team-lab")

	secret := func(lab, dev string) map[string]string {
		t.Helper()
		s, err := k8s.CoreV1().Secrets("team-lab").Get(ctx, devnames.Name(lab, dev)+"-env", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for k, v := range s.Data {
			out[k] = string(v)
		}
		if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Kind != "Lab" {
			t.Fatalf("expected the Lab as owner, got %+v", s.OwnerReferences)
		}
		return out
	}

	// Two variants, sent once; lab-level variables override the variant's per variable.
	deployGroup := "0198c0a4-7a41-7000-8000-000000000002"
	req := &protobuf.CreateLabsRequest{
		Labels: map[string]string{"event": "e1"},
		Variants: []*protobuf.LabVariant{
			{VariantId: "web", SpecJson: specJSON("web", "db"), Env: []*protobuf.DeviceEnv{envOf("web", "FLAG", "common", "MODE", "ctf")}},
			{VariantId: "plain", SpecJson: specJSON("box")},
		},
		Items: []*protobuf.LabItem{
			{LabGroup: "team-lab", Name: "c1", VariantId: "web", Env: []*protobuf.DeviceEnv{envOf("web", "FLAG", "own1")}, Labels: map[string]string{"tier": "gold"}, DeployGroup: deployGroup, DeployAfter: []string{"x"}},
			{LabGroup: "team-lab", Name: "c2", VariantId: "web"},
			{LabGroup: "team-lab", Name: "c3", VariantId: "plain"},
			{LabGroup: "no-group", Name: "c4", VariantId: "plain"},
		},
	}
	res, err := h.CreateLabs(ctx, req)
	wantStates(t, res, err, stCreated, stCreated, stCreated, stNotFound)
	if got := secret("c1", "web"); got["FLAG"] != "own1" || got["MODE"] != "ctf" {
		t.Fatalf("c1 env: %v", got)
	}
	if got := secret("c2", "web"); got["FLAG"] != "common" {
		t.Fatalf("c2 env: %v", got)
	}
	if secret("c1", "db") != nil || secret("c3", "box") != nil {
		t.Fatal("no variables, no Secret")
	}
	c1, _ := labs.Get(ctx, "c1", metav1.GetOptions{})
	if c1.Labels["event"] != "e1" || c1.Labels["tier"] != "gold" || c1.Labels[names.LabelDeployGroup] != deployGroup ||
		c1.Annotations[names.AnnotationDeployAfter] != "x" {
		t.Fatalf("c1 metadata: %v %v", c1.Labels, c1.Annotations)
	}
	if string(c1.Spec.Devices[0].Name) == "" || len(c1.Spec.Devices) != 2 {
		t.Fatalf("spec: %+v", c1.Spec)
	}

	// Resend: EXISTS, Secrets rewritten; a Secret deleted behind the agent's back returns.
	if err := k8s.CoreV1().Secrets("team-lab").Delete(ctx, devnames.Name("c1", "web")+"-env", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err = h.CreateLabs(ctx, req)
	wantStates(t, res, err, stExists, stExists, stExists, stNotFound)
	if got := secret("c1", "web"); got["FLAG"] != "own1" {
		t.Fatalf("resend must rewrite the Secret: %v", got)
	}
	// Different variables in the resend: the Secret takes the new values, the lab is EXISTS.
	req.Items[1].Env = []*protobuf.DeviceEnv{envOf("web", "FLAG", "changed")}
	res, err = h.CreateLabs(ctx, req)
	wantStates(t, res, err, stExists, stExists, stExists, stNotFound)
	if got := secret("c2", "web"); got["FLAG"] != "changed" {
		t.Fatalf("c2 env: %v", got)
	}
	// A different spec for an existing name: FAILED for that item only.
	req.Variants[1].SpecJson = specJSON("box", "other")
	res, err = h.CreateLabs(ctx, req)
	wantStates(t, res, err, stExists, stExists, stFailed, stNotFound)
	if !strings.Contains(res.Results[2].Error, "different spec") {
		t.Fatalf("got %v", res.Results[2])
	}

	// Update: labels and a device Secret rewrite, by items and by selector.
	upd, err := h.UpdateLabs(ctx, &protobuf.UpdateLabsRequest{
		Changes: &protobuf.LabChanges{Labels: &protobuf.LabelChanges{Set: map[string]string{"round": "2"}}},
		Items: []*protobuf.UpdateLabItem{
			{LabGroup: "team-lab", Name: "c1", Changes: &protobuf.LabChanges{Env: []*protobuf.DeviceEnv{envOf("web", "FLAG", "new")}}},
			{LabGroup: "team-lab", Name: "c2", Changes: &protobuf.LabChanges{Env: []*protobuf.DeviceEnv{envOf("web")}}},
			{LabGroup: "team-lab", Name: "c3", Changes: &protobuf.LabChanges{Env: []*protobuf.DeviceEnv{envOf("ghost", "A", "1")}}},
			{LabGroup: "team-lab", Name: "zzz"},
		},
	})
	wantStates(t, upd, err, stUpdated, stUpdated, stFailed, stNotFound)
	if got := secret("c1", "web"); len(got) != 1 || got["FLAG"] != "new" {
		t.Fatalf("c1 env after update: %v", got)
	}
	if secret("c2", "web") != nil {
		t.Fatal("an empty device env removes the Secret")
	}
	if c1, _ := labs.Get(ctx, "c1", metav1.GetOptions{}); c1.Labels["round"] != "2" {
		t.Fatalf("the common labels apply to every item: %v", c1.Labels)
	}
	sel, err := h.UpdateLabs(ctx, &protobuf.UpdateLabsRequest{
		BySelector: &protobuf.Selector{Selector: "event=e1,tier", ExpectedCount: proto.Int64(1)},
		Changes:    &protobuf.LabChanges{Labels: &protobuf.LabelChanges{Remove: []string{"tier"}}},
	})
	wantStates(t, sel, err, stUpdated)

	// List by names, by selector and per group; spec_json comes back.
	list, err := h.ListLabs(ctx, &protobuf.ListRequest{Selector: "event=e1"})
	if err != nil || len(list.Items) != 3 || list.Items[0].LabGroupName != "team-lab" || len(list.Items[0].SpecJson) == 0 {
		t.Fatalf("list selector: %v %v", list, err)
	}
	list, _ = h.ListLabs(ctx, &protobuf.ListRequest{Items: []*protobuf.ItemRef{{LabGroup: "team-lab", Name: "c1"}, {LabGroup: "team-lab", Name: "zzz"}}})
	if len(list.Items) != 1 {
		t.Fatalf("list names: %v", list)
	}
	if list, _ = h.ListLabs(ctx, &protobuf.ListRequest{LabGroup: "team-lab"}); len(list.Items) != 3 {
		t.Fatalf("list group: %v", list)
	}

	// Delete: the selector guard, then the selector delete and a missing one.
	if _, err = h.DeleteLabs(ctx, &protobuf.DeleteRequest{BySelector: &protobuf.Selector{Selector: "event=e1", ExpectedCount: proto.Int64(2)}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("guard: %v", err)
	}
	if list, _ = h.ListLabs(ctx, &protobuf.ListRequest{}); len(list.Items) != 3 {
		t.Fatal("a refused delete deleted something")
	}
	del, err := h.DeleteLabs(ctx, &protobuf.DeleteRequest{BySelector: &protobuf.Selector{Selector: "event=e1", ExpectedCount: proto.Int64(3)}})
	wantStates(t, del, err, stDeleted, stDeleted, stDeleted)
	del, err = h.DeleteLabs(ctx, &protobuf.DeleteRequest{Items: []*protobuf.ItemRef{{LabGroup: "team-lab", Name: "c1"}}})
	wantStates(t, del, err, stNotFound)
}

// The names the platform provides in every lab cannot be taken by a device.
func TestCreateLabsRefusesReservedDeviceNames(t *testing.T) {
	h, k8s := newTestHandler(t)
	readyGroup(t, h, k8s, "team-lab", "team-lab", nil)
	for _, name := range names.ReservedDeviceNames {
		_, err := h.CreateLabs(context.Background(), &protobuf.CreateLabsRequest{
			Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON(name)}},
			Items:    []*protobuf.LabItem{{LabGroup: "team-lab", Name: "l1", VariantId: "v"}},
		})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("device %q: %v", name, err)
		}
	}
}

// Labs "a-b" (device "c") and "a" (device "b-c") used to spell one Secret name; each keeps its own now.
func TestEnvSecretsOfCollidingPairsStaySeparate(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	readyGroup(t, h, k8s, "team-lab", "team-lab", nil)
	envOf := func(dev, k, v string) *protobuf.DeviceEnv {
		return &protobuf.DeviceEnv{Device: dev, Vars: map[string]string{k: v}}
	}
	for _, c := range []struct{ lab, dev, flag string }{{"a-b", "c", "one"}, {"a", "b-c", "two"}} {
		res, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
			Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON(c.dev), Env: []*protobuf.DeviceEnv{envOf(c.dev, "FLAG", c.flag)}}},
			Items:    []*protobuf.LabItem{{LabGroup: "team-lab", Name: c.lab, VariantId: "v"}},
		})
		wantStates(t, res, err, stCreated)
	}
	for _, c := range []struct{ lab, dev, flag string }{{"a-b", "c", "one"}, {"a", "b-c", "two"}} {
		s, err := k8s.CoreV1().Secrets("team-lab").Get(ctx, devnames.Name(c.lab, c.dev)+"-env", metav1.GetOptions{})
		if err != nil || string(s.Data["FLAG"]) != c.flag {
			t.Errorf("%s/%s: %v %v", c.lab, c.dev, err, s)
		}
	}
}
