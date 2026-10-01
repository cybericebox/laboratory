package grpc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func TestValidateLabels(t *testing.T) {
	good := []map[string]string{nil, {"a": "b"}, {"event": ""}, {"example.com/role": "x-1"}}
	for _, m := range good {
		if err := validateLabels(m); err != nil {
			t.Errorf("%v: %v", m, err)
		}
	}
	bad := []map[string]string{
		{"laboratory.cybericebox.com/deploy-group": "x"},
		{names.LabelLab: "x"},
		{"laboratory.cybericebox.com/anything": "x"},
		{"bad key": "x"},
		{"a": "has space"},
		{"a": strings.Repeat("x", 64)},
		{"": "x"},
	}
	for _, m := range bad {
		if err := validateLabels(m); err == nil {
			t.Errorf("%v must be rejected", m)
		}
	}
}

func TestMergeItemLabelsItemWins(t *testing.T) {
	got := mergeItemLabels(map[string]string{"a": "1", "b": "common"}, map[string]string{"b": "own", "c": "3"})
	if len(got) != 3 || got["a"] != "1" || got["b"] != "own" || got["c"] != "3" {
		t.Fatalf("got %v", got)
	}
	if mergeItemLabels(nil, nil) != nil {
		t.Fatal("nothing in, nil out")
	}
}

func TestMergeLabelChanges(t *testing.T) {
	plan, err := mergeLabelChanges(
		&protobuf.LabelChanges{Set: map[string]string{"a": "1", "b": "common"}, Remove: []string{"x"}},
		&protobuf.LabelChanges{Set: map[string]string{"b": "own"}, Remove: []string{"x", "y"}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.set["b"] != "own" || len(plan.remove) != 2 {
		t.Fatalf("plan %+v", plan)
	}
	out, changed := plan.apply(map[string]string{"x": "1", "y": "2", "keep": "3"})
	if !changed || len(out) != 3 || out["a"] != "1" || out["b"] != "own" || out["keep"] != "3" {
		t.Fatalf("apply: %v", out)
	}
	if _, changed := plan.apply(out); changed {
		t.Fatal("applying twice changes nothing")
	}
	for _, bad := range []*protobuf.LabelChanges{
		{Set: map[string]string{"a": "1"}, Remove: []string{"a"}},
		{Set: map[string]string{names.LabelDeployGroup: "x"}},
		{Remove: []string{names.LabelLab}},
	} {
		if _, err := mergeLabelChanges(bad, nil); err == nil {
			t.Errorf("%v must be rejected", bad)
		}
	}
}

func TestDeployKey(t *testing.T) {
	id := uuid.New().String()
	k := deployKey(id)
	if k == "" || len(k) > 25 || strings.ToLower(k) != k || k != deployKey(strings.ToUpper(id)) {
		t.Fatalf("uuid key %q", k)
	}
	h := deployKey("just some text")
	if len(h) > 63 || h[0] != 'h' || h != deployKey("just some text") || h == deployKey("other text") {
		t.Fatalf("hash key %q", h)
	}
	if deployKey("") != "" {
		t.Fatal("empty stays empty")
	}
	for _, key := range []string{k, h} {
		if err := validateLabels(map[string]string{"k": key}); err != nil {
			t.Fatalf("%q is not label safe: %v", key, err)
		}
	}
	d := newDeploySpec("g", []string{id, "g", id, ""})
	if d.group != deployKey("g") || d.after != deployKey(id)+","+deployKey("g") {
		t.Fatalf("deploy spec %+v", d)
	}
	labels, ann := d.stamp(nil, nil)
	if labels[names.LabelDeployGroup] != d.group || ann[names.AnnotationDeployAfter] != d.after || !d.matches(labels, ann) {
		t.Fatalf("stamp %v %v", labels, ann)
	}
}

func TestParseLabSpecRejectsDeviceVariables(t *testing.T) {
	for _, key := range []string{"env", "flags", "Flag", "envFrom", "environment"} {
		raw := `{"devices":[{"name":"web","type":"container","` + key + `":[{"name":"X","value":"1"}]}]}`
		_, err := parseLabSpec([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), "secrets") {
			t.Errorf("%s: %v", key, err)
		}
	}
	if _, err := parseLabSpec([]byte(`{"devices":[{"name":"web","type":"container","bogus":1}]}`)); err == nil {
		t.Error("unknown fields are refused")
	}
	if _, err := parseLabSpec(nil); err == nil {
		t.Error("an empty spec is refused")
	}
	if _, err := parseLabSpec(specJSON("web")); err != nil {
		t.Errorf("a plain spec: %v", err)
	}
}

func TestParseVariants(t *testing.T) {
	v, err := parseVariants([]*protobuf.LabVariant{{VariantId: "a", SpecJson: specJSON("web"), Env: []*protobuf.DeviceEnv{envOf("web", "K", "v")}}})
	if err != nil || v["a"].spec.LaunchClass != "a" || v["a"].env["web"]["K"] != "v" {
		t.Fatalf("%+v %v", v, err)
	}
	bad := [][]*protobuf.LabVariant{
		{{VariantId: "a", SpecJson: specJSON("web")}, {VariantId: "a", SpecJson: specJSON("web")}},
		{{SpecJson: specJSON("web")}},
		{{VariantId: "a", SpecJson: specJSON("web"), Env: []*protobuf.DeviceEnv{envOf("ghost", "K", "v")}}},
		{{VariantId: "a", SpecJson: specJSON("web"), Env: []*protobuf.DeviceEnv{envOf("web", "1BAD", "v")}}},
		{{VariantId: "a", SpecJson: []byte(`{"devices":[{"name":"web","type":"container","env":[]}]}`)}},
	}
	for i, b := range bad {
		if _, err := parseVariants(b); status.Code(err) != codes.InvalidArgument {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestMergeEnvListsOverridesByVariable(t *testing.T) {
	got := mergeEnvLists(
		[]*protobuf.DeviceEnv{envOf("web", "A", "1", "B", "2"), envOf("db", "P", "x")},
		[]*protobuf.DeviceEnv{envOf("web", "B", "own", "C", "3"), envOf("empty")})
	if got["web"]["A"] != "1" || got["web"]["B"] != "own" || got["web"]["C"] != "3" || got["db"]["P"] != "x" {
		t.Fatalf("got %v", got)
	}
	if v, ok := got["empty"]; !ok || len(v) != 0 {
		t.Fatal("a listed device without variables stays listed")
	}
}

func TestCheckSelection(t *testing.T) {
	cases := []struct {
		sel      string
		set      bool
		items    int
		wantCode codes.Code
	}{
		{"", true, 0, codes.InvalidArgument},    // an empty selector
		{"a=b", true, 1, codes.InvalidArgument}, // both
		{"", false, 0, codes.InvalidArgument},   // neither
		{"a in (", true, 0, codes.InvalidArgument},
		{"a=b", true, 0, codes.OK},
		{"", false, 1, codes.OK},
		{"", false, maxItems + 1, codes.InvalidArgument},
	}
	for _, c := range cases {
		if got := status.Code(checkSelection(c.sel, c.items, c.set)); got != c.wantCode {
			t.Errorf("%+v: %v", c, got)
		}
	}
}

func TestCheckMatched(t *testing.T) {
	if err := checkMatched(3, nil); err != nil {
		t.Fatal(err)
	}
	if err := checkMatched(3, proto.Int64(3)); err != nil {
		t.Fatal(err)
	}
	if err := checkMatched(1, proto.Int64(3)); err != nil {
		t.Fatal("fewer matches than expected go through")
	}
	err := checkMatched(4, proto.Int64(3))
	if n, ok := client.CountMismatch(err); !ok || n != 4 || status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("%v", err)
	}
	if err := checkMatched(maxItems+1, nil); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("too many matches: %v", err)
	}
}

func TestMaxItemsPerCall(t *testing.T) {
	h, _ := newFinalizerHandler(t)
	items := make([]*protobuf.LabGroupItem, maxItems+1)
	for i := range items {
		items[i] = &protobuf.LabGroupItem{Name: "g"}
	}
	if _, err := h.CreateLabGroups(context.Background(), &protobuf.CreateLabGroupsRequest{Items: items}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v", err)
	}
}

func TestRequestValidation(t *testing.T) {
	h, _ := newFinalizerHandler(t, &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1"}, Status: laboratoryv1alpha1.LabGroupStatus{Namespace: "ns1"}})
	ctx := context.Background()
	reserved := map[string]string{"laboratory.cybericebox.com/x": "y"}
	calls := map[string]func() error{
		"group request labels": func() error {
			_, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Labels: reserved, Items: groupItems("a")})
			return err
		},
		"group item labels": func() error {
			_, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: "a", Labels: reserved}}})
			return err
		},
		"duplicate group": func() error {
			_, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: groupItems("a", "a")})
			return err
		},
		"client labels": func() error {
			_, err := h.CreateLabGroupClients(ctx, &protobuf.CreateLabGroupClientsRequest{Items: []*protobuf.LabGroupClientItem{{LabGroup: "g1", Name: "p", Labels: reserved}}})
			return err
		},
		"lab labels": func() error {
			_, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
				Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON("web")}},
				Items:    []*protobuf.LabItem{{LabGroup: "g1", Name: "c", VariantId: "v", Labels: reserved}}})
			return err
		},
		"unknown variant": func() error {
			_, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{Items: []*protobuf.LabItem{{LabGroup: "g1", Name: "c", VariantId: "v"}}})
			return err
		},
		"item env of unknown device": func() error {
			_, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
				Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON("web")}},
				Items:    []*protobuf.LabItem{{LabGroup: "g1", Name: "c", VariantId: "v", Env: []*protobuf.DeviceEnv{envOf("ghost", "A", "1")}}}})
			return err
		},
		"update reserved": func() error {
			_, err := h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{
				Changes: &protobuf.LabGroupChanges{Labels: &protobuf.LabelChanges{Remove: []string{names.LabelDeployGroup}}},
				Items:   []*protobuf.UpdateLabGroupItem{{Name: "g1"}}})
			return err
		},
		"update selector and items": func() error {
			_, err := h.UpdateLabs(ctx, &protobuf.UpdateLabsRequest{BySelector: &protobuf.Selector{Selector: "a=b"}, Items: []*protobuf.UpdateLabItem{{LabGroup: "g1", Name: "c"}}})
			return err
		},
		"update client empty selector": func() error {
			_, err := h.UpdateLabGroupClients(ctx, &protobuf.UpdateLabGroupClientsRequest{BySelector: &protobuf.Selector{}})
			return err
		},
		"list bad selector": func() error {
			_, err := h.ListLabs(ctx, &protobuf.ListRequest{Selector: "a in ("})
			return err
		},
		"access labels": func() error {
			_, err := h.SetLabGroupAccess(ctx, &protobuf.SetLabGroupAccessRequest{Labels: reserved, Policies: []*protobuf.LabGroupAccessPolicy{{LabGroupName: "g1"}}})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A selector guard on the fake clientset: a refused call leaves every object untouched.
func TestSelectorGuardsLeaveObjectsUntouched(t *testing.T) {
	var objs []*laboratoryv1alpha1.LabGroup
	for _, n := range []string{"a", "b", "c"} {
		objs = append(objs, &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: n, Labels: map[string]string{"event": "e"}}})
	}
	h, _ := newFinalizerHandler(t, objs[0], objs[1], objs[2])
	ctx := context.Background()
	sel := &protobuf.Selector{Selector: "event=e", ExpectedCount: proto.Int64(2)}
	if _, err := h.DeleteLabGroups(ctx, &protobuf.DeleteRequest{BySelector: sel}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v", err)
	}
	if _, err := h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{BySelector: sel, Changes: &protobuf.LabGroupChanges{Suspended: proto.Bool(true)}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("got %v", err)
	}
	list, _ := h.ListLabGroups(ctx, &protobuf.ListRequest{})
	if len(list.Items) != 3 || list.Items[0].Status.Suspended {
		t.Fatalf("something changed: %v", list)
	}
	// Names that match nothing: an empty answer, not an error.
	res, err := h.DeleteLabGroups(ctx, &protobuf.DeleteRequest{BySelector: &protobuf.Selector{Selector: "event=none", ExpectedCount: proto.Int64(0)}})
	if err != nil || len(res.Results) != 0 {
		t.Fatalf("no match: %v %v", res, err)
	}
}

// A big batch goes through the bounded workers and keeps the order of the request.
func TestBatchKeepsRequestOrder(t *testing.T) {
	h, _ := newFinalizerHandler(t)
	ctx := context.Background()
	names := make([]string, 300)
	for i := range names {
		names[i] = "g-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	res, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: groupItems(names...)})
	if err != nil || len(res.Results) != len(names) {
		t.Fatalf("%v %v", res, err)
	}
	for i, r := range res.Results {
		if r.State != stCreated || r.Ref.Name != names[i] {
			t.Fatalf("result %d: %v", i, r)
		}
	}
	list, _ := h.ListLabGroups(ctx, &protobuf.ListRequest{})
	if len(list.Items) != len(names) {
		t.Fatalf("listed %d", len(list.Items))
	}
}
