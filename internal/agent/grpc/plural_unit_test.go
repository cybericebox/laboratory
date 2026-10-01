package grpc

import (
	"context"
	"strings"
	"testing"

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

func TestDeploySpec(t *testing.T) {
	id := "0198c0a4-7a41-7000-8000-000000000001"
	d, err := newDeploySpec(id, []string{"first", id, "first", ""})
	if err != nil || d.group != id || len(d.after) != 2 {
		t.Fatalf("deploy spec %+v %v", d, err)
	}
	labels, ann := d.stamp(nil, nil)
	if labels[names.LabelDeployGroup] != id || ann[names.AnnotationDeployGroup] != id || ann[names.AnnotationDeployAfter] != "first,"+id || !d.matches(labels, ann) {
		t.Fatalf("stamp %v %v", labels, ann)
	}
	if g, a := deployOf(ann); g != id || len(a) != 2 || a[0] != "first" {
		t.Fatalf("read back %q %v", g, a)
	}
	// An invalid label value is hashed in the label, the original is kept.
	d, _ = newDeploySpec("has space", nil)
	labels, ann = d.stamp(nil, nil)
	if labels[names.LabelDeployGroup] == "has space" || ann[names.AnnotationDeployGroup] != "has space" {
		t.Fatalf("hashed %v %v", labels, ann)
	}
	for _, bad := range []func() error{
		func() error { _, err := newDeploySpec(strings.Repeat("x", 65), nil); return err },
		func() error { _, err := newDeploySpec("a,b", nil); return err },
		func() error { _, err := newDeploySpec("", []string{"a,b"}); return err },
		func() error { _, err := newDeploySpec("", []string{strings.Repeat("x", 65)}); return err },
		func() error {
			more := make([]string, names.MaxDeployAfter+1)
			for i := range more {
				more[i] = string(rune('a' + i))
			}
			_, err := newDeploySpec("", more)
			return err
		},
	} {
		if bad() == nil {
			t.Error("must be rejected")
		}
	}
}

func TestParseLabSpecRejectsDeviceVariables(t *testing.T) {
	for _, key := range []string{"env", "flags", "Flag", "envFrom", "environment"} {
		raw := `{"devices":[{"name":"web","type":"container","` + key + `":[{"name":"X","value":"1"}]}]}`
		_, err := parseLabSpec([]byte(raw), false)
		if err == nil || !strings.Contains(err.Error(), "secrets") {
			t.Errorf("%s: %v", key, err)
		}
	}
	if _, err := parseLabSpec([]byte(`{"devices":[{"name":"web","type":"container","bogus":1}]}`), false); err == nil {
		t.Error("unknown fields are refused")
	}
	if _, err := parseLabSpec(nil, false); err == nil {
		t.Error("an empty spec is refused")
	}
	if _, err := parseLabSpec(specJSON("web"), false); err != nil {
		t.Errorf("a plain spec: %v", err)
	}
}

func TestParseVariants(t *testing.T) {
	v, err := parseVariants([]*protobuf.LabVariant{{VariantId: "a", SpecJson: specJSON("web"), Env: []*protobuf.DeviceEnv{envOf("web", "K", "v")}}}, false)
	if err != nil || v["a"].env["web"]["K"] != "v" {
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
		if _, err := parseVariants(b, false); status.Code(err) != codes.InvalidArgument {
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

func TestPersistenceInTheSpec(t *testing.T) {
	spec := func(p string) []byte {
		return []byte(`{"devices":[{"name":"web","type":"container","image":"x","persistence":` + p + `}]}`)
	}
	if _, err := parseLabSpec(spec(`{"enabled":true}`), false); err == nil || !strings.Contains(err.Error(), "does not allow") {
		t.Fatalf("persistence off in the cluster: %v", err)
	}
	got, err := parseLabSpec(spec(`{"enabled":true,"debounce":"5s","excludePaths":["/tmp"],"maxSnapshotSize":"512Mi"}`), true)
	if err != nil || !got.Devices[0].Persistence.Enabled || got.Devices[0].Persistence.MaxSnapshotSize != "512Mi" {
		t.Fatalf("%+v %v", got, err)
	}
	// Disabled persistence is fine anywhere.
	if _, err := parseLabSpec(spec(`{"enabled":false}`), false); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"enabled":true,"debounce":"0s"}`, `{"enabled":true,"excludePaths":["tmp"]}`, `{"enabled":true,"maxSnapshotSize":"lots"}`, `{"enabled":true,"nope":1}`} {
		if _, err := parseLabSpec(spec(bad), true); err == nil {
			t.Errorf("%s must be rejected", bad)
		}
	}
}

func TestIDsAreEncodedAndReturned(t *testing.T) {
	h, _ := newFinalizerHandler(t)
	ctx := context.Background()
	uuid := "0198c0a4-7a41-7000-8000-000000000001"
	res, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: groupItems(uuid, "Team Alpha!", strings.Repeat("Z", 64))})
	wantStates(t, res, err, stCreated, stCreated, stCreated)
	if _, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, uuid, metav1.GetOptions{}); err != nil {
		t.Fatalf("a valid id is the CR name: %v", err)
	}
	enc := names.EncodeName("Team Alpha!")
	g, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, enc, metav1.GetOptions{})
	if err != nil || g.Annotations[names.AnnotationID] != "Team Alpha!" {
		t.Fatalf("encoded group: %v %v", g, err)
	}
	// Lookups go through the encoding; answers carry the original id.
	list, err := h.ListLabGroups(ctx, &protobuf.ListRequest{Items: []*protobuf.ItemRef{{Name: "Team Alpha!"}}})
	if err != nil || len(list.Items) != 1 || list.Items[0].Name != "Team Alpha!" {
		t.Fatalf("list by id: %v %v", list, err)
	}
	if res, err = h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: groupItems("Team Alpha!")}); err != nil || res.Results[0].State != stExists || res.Results[0].Ref.Name != "Team Alpha!" {
		t.Fatalf("resend: %v %v", res, err)
	}
	del, err := h.DeleteLabGroups(ctx, &protobuf.DeleteRequest{Items: []*protobuf.ItemRef{{Name: "Team Alpha!"}}})
	wantStates(t, del, err, stDeleted)

	for _, bad := range []string{"", strings.Repeat("a", 65)} {
		if _, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: groupItems(bad)}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("id %q: %v", bad, err)
		}
	}
}

// Labs, clients and policies with encoded ids: the monitoring feed and the lists show the originals.
func TestEncodedIDsInMonitoring(t *testing.T) {
	grp := &laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{Name: names.EncodeName("Team X"), Annotations: map[string]string{names.AnnotationID: "Team X"}},
		Status:     laboratoryv1alpha1.LabGroupStatus{Namespace: "ns-x"},
	}
	h, _ := newFinalizerHandler(t, grp)
	ctx := context.Background()
	res, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON("web")}},
		Items:    []*protobuf.LabItem{{LabGroup: "Team X", Name: "Lab One", VariantId: "v"}},
	})
	wantStates(t, res, err, stCreated)
	h.cs.LaboratoryV1alpha1().LabGroupClients("ns-x").Create(ctx, &laboratoryv1alpha1.LabGroupClient{ObjectMeta: metav1.ObjectMeta{
		Name: names.EncodeName("Alice A"), Namespace: "ns-x", Annotations: map[string]string{names.AnnotationID: "Alice A"}}}, metav1.CreateOptions{})
	acc, err := h.SetLabGroupAccess(ctx, &protobuf.SetLabGroupAccessRequest{Policies: []*protobuf.LabGroupAccessPolicy{{
		LabGroupName: "Team X",
		Rules:        []*protobuf.LabGroupAccessRule{{Action: protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW, ClientNames: []string{"Alice A", "bob"}, LabNames: []string{"Lab One"}}},
	}}})
	wantStates(t, acc, err, stCreated)
	stored, _ := h.cs.LaboratoryV1alpha1().LabGroupAccessPolicies("ns-x").Get(ctx, names.LabGroupAccessPolicyName, metav1.GetOptions{})
	if stored.Spec.Rules[0].LabNames[0] != names.EncodeName("Lab One") {
		t.Fatalf("the reconciler matches CR names: %v", stored.Spec.Rules)
	}
	if acc, err = h.SetLabGroupAccess(ctx, &protobuf.SetLabGroupAccessRequest{Policies: []*protobuf.LabGroupAccessPolicy{{
		LabGroupName: "Team X",
		Rules:        []*protobuf.LabGroupAccessRule{{Action: protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW, ClientNames: []string{"Alice A", "bob"}, LabNames: []string{"Lab One"}}},
	}}}); err != nil || acc.Results[0].State != stExists {
		t.Fatalf("resend: %v %v", acc, err)
	}

	st, err := h.collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u := st.update
	if u.Groups[0].Name != "Team X" || u.Labs[0].Name != "Lab One" || u.Labs[0].LabGroupName != "Team X" || u.Clients[0].Name != "Alice A" || u.Clients[0].LabGroupName != "Team X" {
		t.Fatalf("ids in monitoring: %v %v %v", u.Groups, u.Labs, u.Clients)
	}
	rule := u.Policies[0].Rules[0]
	if rule.LabNames[0] != "Lab One" || rule.ClientNames[0] != "Alice A" || rule.ClientNames[1] != "bob" {
		t.Fatalf("policy ids: %v", rule)
	}
	// The filter keys use ids too.
	if _, ok := st.labels[recordKey("lab", "Team X", "ns-x", "Lab One")]; !ok {
		t.Fatal("record key with the id")
	}
	list, err := h.ListLabs(ctx, &protobuf.ListRequest{LabGroup: "Team X"})
	if err != nil || len(list.Items) != 1 || list.Items[0].Name != "Lab One" {
		t.Fatalf("list: %v %v", list, err)
	}
	del, err := h.DeleteLabs(ctx, &protobuf.DeleteRequest{Items: []*protobuf.ItemRef{{LabGroup: "Team X", Name: "Lab One"}}})
	wantStates(t, del, err, stDeleted)
}

func TestReservedLabelKeys(t *testing.T) {
	for _, k := range []string{names.LabelLab, names.LabelDeployGroup, "app", "pod-template-hash", "kubernetes.io/hostname", "node.kubernetes.io/x", "k8s.io/a", "foo.k8s.io/b"} {
		if !names.IsReservedLabel(k) {
			t.Errorf("%q must be reserved", k)
		}
		if validateLabels(map[string]string{k: "v"}) == nil {
			t.Errorf("setting %q must be refused", k)
		}
	}
	for _, k := range []string{"event", "example.com/app", "notkubernetes.io/x", "team"} {
		if names.IsReservedLabel(k) {
			t.Errorf("%q is a user label", k)
		}
	}
}

// Internal labels never leave the agent: not in answers, not searchable.
func TestInternalLabelsAreHidden(t *testing.T) {
	grp := &laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "g1", Labels: map[string]string{"team": "a", names.LabelDeployGroup: "x", "app": "y"}},
		Status:     laboratoryv1alpha1.LabGroupStatus{Namespace: "ns1"},
	}
	h, _ := newFinalizerHandler(t, grp)
	ctx := context.Background()

	list, err := h.ListLabGroups(ctx, &protobuf.ListRequest{})
	if err != nil || len(list.Items) != 1 || len(list.Items[0].Labels) != 1 || list.Items[0].Labels["team"] != "a" {
		t.Fatalf("list labels: %v %v", list, err)
	}
	res, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
		Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON("web")}},
		Items:    []*protobuf.LabItem{{LabGroup: "g1", Name: "c1", VariantId: "v", DeployGroup: "dg", Labels: map[string]string{"round": "1"}}},
	})
	wantStates(t, res, err, stCreated)
	labs, _ := h.ListLabs(ctx, &protobuf.ListRequest{})
	if len(labs.Items[0].Labels) != 1 || labs.Items[0].Labels["round"] != "1" || labs.Items[0].DeployGroup != "dg" {
		t.Fatalf("lab labels: %v", labs.Items[0])
	}
	st, err := h.collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range st.update.Groups {
		if len(g.Labels) != 1 {
			t.Fatalf("monitoring group labels: %v", g.Labels)
		}
	}
	for _, l := range st.update.Labs {
		if len(l.Labels) != 1 {
			t.Fatalf("monitoring lab labels: %v", l.Labels)
		}
	}

	sel := names.LabelDeployGroup + "=x"
	for name, call := range map[string]func() error{
		"list groups": func() error { _, err := h.ListLabGroups(ctx, &protobuf.ListRequest{Selector: sel}); return err },
		"list labs":   func() error { _, err := h.ListLabs(ctx, &protobuf.ListRequest{Selector: "app=y"}); return err },
		"list clients": func() error {
			_, err := h.ListLabGroupClients(ctx, &protobuf.ListRequest{Selector: "!kubernetes.io/hostname"})
			return err
		},
		"update": func() error {
			_, err := h.UpdateLabGroups(ctx, &protobuf.UpdateLabGroupsRequest{BySelector: &protobuf.Selector{Selector: "team=a," + sel}})
			return err
		},
		"delete": func() error {
			_, err := h.DeleteLabs(ctx, &protobuf.DeleteRequest{BySelector: &protobuf.Selector{Selector: "pod-template-hash in (a)"}})
			return err
		},
		"devices": func() error {
			_, err := h.ResetDevices(ctx, &protobuf.DevicesRequest{BySelector: &protobuf.DeviceSelector{Selector: sel, Device: "web"}})
			return err
		},
		"monitoring": func() error {
			return h.Monitoring(&protobuf.MonitoringRequest{Selector: sel}, &fakeMonStream{ctx: ctx, sent: make(chan *protobuf.MonitoringUpdate, 1)})
		},
	} {
		if err := call(); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A user selector still works.
	if list, err := h.ListLabGroups(ctx, &protobuf.ListRequest{Selector: "team=a"}); err != nil || len(list.Items) != 1 {
		t.Fatalf("user selector: %v %v", list, err)
	}
}
