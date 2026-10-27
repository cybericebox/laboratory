package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/grouppods"
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
	if a.Labels[names.LabelDeployGroup] != deployGroup || a.Annotations[names.AnnotationDeployGroup] != deployGroup || a.Annotations[names.AnnotationDeployAfter] != "first,"+deployGroup {
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

// CreateLabGroups takes explicit sizes of the group's VPN and gateway pods, checks them against the cluster's maxima and keeps them in the
// spec; the same request again is EXISTS, another size is a different spec.
func TestCreateLabGroupsPodSizes(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetFeatures(testFeatures)
	ctx := context.Background()
	groups := h.cs.LaboratoryV1alpha1().LabGroups()
	size := func(cpu, mem int64) *protobuf.PodSize { return &protobuf.PodSize{CpuMillicores: cpu, MemoryBytes: mem} }
	// the maxima: VPN 20m+6m*20 = 140m and 64Mi+20Mi*20 = 464Mi; gateway 105m and 216Mi
	req := &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{
		{Name: "sz-a", VpnSize: size(140, 464<<20), GatewaySize: size(15, 24<<20)},
		{Name: "sz-b"},
	}}
	res, err := h.CreateLabGroups(ctx, req)
	wantStates(t, res, err, stCreated, stCreated)
	a, _ := groups.Get(ctx, "sz-a", metav1.GetOptions{})
	b, _ := groups.Get(ctx, "sz-b", metav1.GetOptions{})
	if v := a.Spec.VPN.Size; v == nil || v.CPUMillicores != 140 || v.MemoryBytes != 464<<20 {
		t.Fatalf("vpn size: %+v", v)
	}
	if g := a.Spec.Gateway.Size; g == nil || g.CPUMillicores != 15 || g.MemoryBytes != 24<<20 {
		t.Fatalf("gateway size: %+v", g)
	}
	// neither a size nor a count: the group is made for the default counts (1 user, 5 labs)
	wv, wg := grouppods.VPNSize(1, 5), grouppods.GatewaySize(5)
	if v, g := b.Spec.VPN.Size, b.Spec.Gateway.Size; v == nil || g == nil || v.CPUMillicores != wv.CPU || v.MemoryBytes != wv.Memory || g.CPUMillicores != wg.CPU || g.MemoryBytes != wg.Memory {
		t.Fatalf("default counts: %+v %+v", v, g)
	}
	res, err = h.CreateLabGroups(ctx, req)
	wantStates(t, res, err, stExists, stExists)
	req.Items[0].VpnSize = size(100, 100<<20)
	res, err = h.CreateLabGroups(ctx, req)
	wantStates(t, res, err, stFailed, stExists)

	for name, it := range map[string]*protobuf.LabGroupItem{
		"vpn cpu over the maximum":     {Name: "x1", VpnSize: size(141, 64<<20)},
		"vpn memory over the maximum":  {Name: "x2", VpnSize: size(20, 465<<20)},
		"gateway cpu over the maximum": {Name: "x3", GatewaySize: size(106, 16<<20)},
		"gateway memory over":          {Name: "x4", GatewaySize: size(5, 217<<20)},
		"a size without memory":        {Name: "x5", VpnSize: size(20, 0)},
		"a negative size":              {Name: "x6", GatewaySize: size(-1, 16<<20)},
		"an empty size is not a size":  {Name: "x7", VpnSize: &protobuf.PodSize{}},
	} {
		_, err := h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{it}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
		if _, err := groups.Get(ctx, it.Name, metav1.GetOptions{}); err == nil {
			t.Errorf("%s: the group must not be created", name)
		}
	}
}

// A group is sized from the counts it is made for: the defaults (1 user, 5 labs) when it says nothing, the asked counts, a refusal
// over the maximum (10 users, 50 labs), and the old sizes when only sizes are sent.
func TestCreateLabGroupSizedFromExpectedCounts(t *testing.T) {
	h, _ := newTestHandler(t)
	h.reservationsOptional = true
	h.SetFeatures(testFeatures)
	ctx := context.Background()
	create := func(name string, it *protobuf.LabGroupItem) (*protobuf.BatchResult, error) {
		it.Name = name
		return h.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{it}})
	}
	sizeOf := func(name string) (vpn, gw laboratoryv1alpha1.GroupPodSize) {
		t.Helper()
		g, err := h.cs.LaboratoryV1alpha1().LabGroups().Get(ctx, name, metav1.GetOptions{})
		if err != nil || g.Spec.VPN.Size == nil || g.Spec.Gateway.Size == nil {
			t.Fatalf("%s: %+v %v", name, g, err)
		}
		return *g.Spec.VPN.Size, *g.Spec.Gateway.Size
	}
	for _, c := range []struct {
		name        string
		item        *protobuf.LabGroupItem
		users, labs int
	}{
		{"defaults", &protobuf.LabGroupItem{}, 1, 5},
		{"explicit", &protobuf.LabGroupItem{ExpectedUsers: 5, ExpectedLabs: 40}, 5, 40},
		{"only-users", &protobuf.LabGroupItem{ExpectedUsers: 10}, 10, 5},
		{"max", &protobuf.LabGroupItem{ExpectedUsers: 10, ExpectedLabs: 50}, 10, 50},
	} {
		res, err := create(c.name, c.item)
		wantStates(t, res, err, stCreated)
		vpn, gw := sizeOf(c.name)
		wantVPN, wantGW := grouppods.VPNSize(c.users, c.labs), grouppods.GatewaySize(c.labs)
		if r := res.Results[0]; r.GetVpnSize().GetCpuMillicores() != wantVPN.CPU || r.GetGatewaySize().GetMemoryBytes() != wantGW.Memory {
			t.Errorf("%s: the response must carry the assigned sizes: %+v", c.name, r)
		}
		if vpn.CPUMillicores != wantVPN.CPU || vpn.MemoryBytes != wantVPN.Memory || gw.CPUMillicores != wantGW.CPU || gw.MemoryBytes != wantGW.Memory {
			t.Errorf("%s: vpn %+v gateway %+v, want %+v %+v", c.name, vpn, gw, wantVPN, wantGW)
		}
	}
	for name, it := range map[string]*protobuf.LabGroupItem{
		"too-many-users": {ExpectedUsers: 11},
		"too-many-labs":  {ExpectedLabs: 51},
		"negative":       {ExpectedUsers: -1},
	} {
		if _, err := create(name, it); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s must be refused with InvalidArgument: %v", name, err)
		}
	}
	// the old way: only sizes, kept as sent
	res, err := create("legacy", &protobuf.LabGroupItem{VpnSize: &protobuf.PodSize{CpuMillicores: 33, MemoryBytes: 70 << 20}, GatewaySize: &protobuf.PodSize{CpuMillicores: 9, MemoryBytes: 20 << 20}})
	wantStates(t, res, err, stCreated)
	if vpn, gw := sizeOf("legacy"); vpn.CPUMillicores != 33 || gw.MemoryBytes != 20<<20 {
		t.Errorf("legacy sizes changed: %+v %+v", vpn, gw)
	}
	// repeated create with the same counts is the same group
	res, err = create("explicit", &protobuf.LabGroupItem{ExpectedUsers: 5, ExpectedLabs: 40})
	wantStates(t, res, err, stExists)
}
