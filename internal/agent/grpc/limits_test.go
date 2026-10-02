package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/limits"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func limitedFeatures() Features {
	f := testFeatures
	f.Limits = limits.Limits{
		DeviceMaxCPU: 500, DeviceMaxMemory: 512 << 20, DeviceDefaultCPU: 100, DeviceDefaultMemory: 256 << 20,
		LabMaxDevices: 3, GroupMaxLabs: 2, GroupMaxCPU: 250, GroupMaxMemory: 1 << 30, TenantMaxLabs: 3,
	}
	return f
}

func specWithCPU(cpu string) []byte {
	spec := laboratoryv1alpha1.LabSpec{}
	spec.Devices = []laboratoryv1alpha1.DeviceTemplate{{Name: "box", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx",
		Resources: &laboratoryv1alpha1.DeviceResources{CPULimit: cpu}}}
	raw, _ := json.Marshal(spec)
	return raw
}

func TestCreateLabsRefusesSpecsOverTheCaps(t *testing.T) {
	h, k8s := newTestHandler(t)
	h.SetFeatures(limitedFeatures())
	readyGroup(t, h, k8s, "g", "g", nil)
	ctx := context.Background()
	create := func(variant []byte) error {
		_, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
			Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: variant}},
			Items:    []*protobuf.LabItem{{LabGroup: "g", Name: "x", VariantId: "v"}},
		})
		return err
	}
	for name, tc := range map[string]struct {
		spec []byte
		want string
	}{
		"device over cpu": {specWithCPU("1"), "exceeds the limit of 500m"},
		"too many":        {specJSON("a", "b", "c", "d"), "4 container devices, the limit is 3"},
	} {
		err := create(tc.spec)
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `variant "v"`) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := create(specJSON("a", "b", "c")); err != nil {
		t.Fatalf("3 devices of the profile (300m, 768Mi) are fine: %v", err)
	}
}

func TestCreateLabsHonoursTheTenantLabLimit(t *testing.T) {
	h, k8s := newTestHandler(t)
	f := limitedFeatures()
	f.Limits.GroupMaxLabs, f.Limits.TenantMaxLabs = 0, 2
	h.SetFeatures(f)
	readyGroup(t, h, k8s, "g", "g", nil)
	ctx := context.Background()
	create := func(ids ...string) *protobuf.BatchResult {
		items := make([]*protobuf.LabItem, len(ids))
		for i, id := range ids {
			items[i] = &protobuf.LabItem{LabGroup: "g", Name: id, VariantId: "v"}
		}
		res, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: specJSON("a")}}, Items: items})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := create("l1", "l2", "l3")
	wantStates(t, res, nil, stCreated, stCreated, stFailed)
	if !strings.Contains(res.Results[2].Error, "limit of 2 labs") || res.Results[2].Retryable {
		t.Fatalf("over the cap: %+v", res.Results[2])
	}
	// an existing lab is not new: the resend is EXISTS, the next new one still fails
	wantStates(t, create("l1", "l4"), nil, stExists, stFailed)
}

func TestFeaturesReportTheLimits(t *testing.T) {
	h := featuresHandler(t, limitedFeatures(), newTenantTenant("a", true, nil))
	got, err := h.GetFeatures(asClient("a"), &protobuf.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	l := got.GetLimits()
	d, lab, g := l.GetDevice(), l.GetLab(), l.GetGroup()
	if d.GetMaxCpuMillicores() != 500 || d.GetMaxMemoryBytes() != 512<<20 || d.GetDefaultCpuMillicores() != 100 ||
		d.GetDefaultMemoryBytes() != 256<<20 || lab.GetMaxDevices() != 3 || g.GetMaxLabs() != 2 || g.GetMaxCpuMillicores() != 250 ||
		g.GetMaxMemoryBytes() != 1<<30 || l.GetTenant().GetMaxLabs() != 3 {
		t.Fatalf("limits: %+v", l)
	}
}

// Per group: its labs, and the sum of the planned resources of all of them (the profile is 100m / 256Mi per device).
func TestCreateLabsHonoursTheGroupLimits(t *testing.T) {
	h, k8s := newTestHandler(t)
	f := limitedFeatures()
	f.Limits.TenantMaxLabs = 0
	h.SetFeatures(f) // 2 labs, 250m, 1Gi per group
	readyGroup(t, h, k8s, "g1", "g1", nil)
	readyGroup(t, h, k8s, "g2", "g2", nil)
	ctx := context.Background()
	create := func(variant []byte, refs ...[2]string) *protobuf.BatchResult {
		items := make([]*protobuf.LabItem, len(refs))
		for i, r := range refs {
			items[i] = &protobuf.LabItem{LabGroup: r[0], Name: r[1], VariantId: "v"}
		}
		res, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: variant}}, Items: items})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// two devices per lab = 200m: the second lab of g1 passes 250m, g2 is counted apart
	res := create(specJSON("a", "b"), [2]string{"g1", "l1"}, [2]string{"g1", "l2"}, [2]string{"g2", "l3"})
	wantStates(t, res, nil, stCreated, stFailed, stCreated)
	if !strings.Contains(res.Results[1].Error, "limit per group is 250m") || res.Results[1].Retryable {
		t.Fatalf("cpu cap: %+v", res.Results[1])
	}
	// g1 has 200m planned; a resend of l1 is not new, another 200m lab would make 400m
	res = create(specJSON("a", "b"), [2]string{"g1", "l1"}, [2]string{"g1", "l4"})
	wantStates(t, res, nil, stExists, stFailed)
	// the labs cap: g2 holds one lab (200m), a second 50m-less lab fits, a third passes the cap of 2 labs
	f.Limits.GroupMaxCPU = 0
	h.SetFeatures(f)
	res = create(specJSON("a"), [2]string{"g2", "l5"}, [2]string{"g2", "l6"})
	wantStates(t, res, nil, stCreated, stFailed)
	if !strings.Contains(res.Results[1].Error, "limit of 2 labs") {
		t.Fatalf("labs cap: %+v", res.Results[1])
	}
}

func specWithProfile(preset string) []byte {
	spec := laboratoryv1alpha1.LabSpec{}
	spec.Devices = []laboratoryv1alpha1.DeviceTemplate{{Name: "box", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "nginx", SecurityPreset: laboratoryv1alpha1.SecurityPreset(preset)}}
	raw, _ := json.Marshal(spec)
	return raw
}

// The agent reports the enabled profiles and refuses a device that asks for one that is not enabled; the old names
// are aliases (basic and service mean standard, net and debug mean extended).
func TestProfilesAreReportedAndEnforced(t *testing.T) {
	h, k8s := newTestHandler(t)
	f := testFeatures
	f.DeviceProfiles = []string{"standard"}
	h.SetFeatures(f)
	readyGroup(t, h, k8s, "g", "g", nil)
	got, err := h.GetFeatures(asClient("default"), &protobuf.Empty{})
	if err != nil || len(got.GetDeviceProfiles()) != 1 || got.GetDeviceProfiles()[0] != "standard" {
		t.Fatalf("features: %+v %v", got.GetDeviceProfiles(), err)
	}
	n := 0
	create := func(spec []byte) error {
		n++
		res, err := h.CreateLabs(context.Background(), &protobuf.CreateLabsRequest{
			Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: spec}},
			Items:    []*protobuf.LabItem{{LabGroup: "g", Name: fmt.Sprintf("x%d", n), VariantId: "v"}},
		})
		if err == nil && res.Results[0].State != protobuf.ItemState_ITEM_STATE_CREATED {
			return fmt.Errorf("not created: %v", res.Results[0])
		}
		return err
	}
	for _, ok := range []string{"", "standard", "basic", "service"} {
		if err := create(specWithProfile(ok)); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
	for _, refused := range []string{"extended", "net", "debug"} {
		err := create(specWithProfile(refused))
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "is not enabled on this cluster") || !strings.Contains(err.Error(), `device "box"`) {
			t.Errorf("%q must be refused with the reason: %v", refused, err)
		}
	}
	f.DeviceProfiles = []string{"standard", "extended"}
	h.SetFeatures(f)
	for _, ok := range []string{"extended", "net", "debug"} {
		if err := create(specWithProfile(ok)); err != nil {
			t.Errorf("%q must be accepted when extended is enabled: %v", ok, err)
		}
	}
}
