package grpc

import (
	"context"
	"encoding/json"
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
		LabMaxDevices: 3, LabMaxCPU: 1000, LabMaxMemory: 1 << 30, TenantMaxLabs: 2,
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
	h.SetFeatures(limitedFeatures()) // 2 labs
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
	if l.GetDeviceMaxCpuMillicores() != 500 || l.GetDeviceMaxMemoryBytes() != 512<<20 || l.GetDeviceDefaultCpuMillicores() != 100 ||
		l.GetDeviceDefaultMemoryBytes() != 256<<20 || l.GetLabMaxDevices() != 3 || l.GetLabMaxCpuMillicores() != 1000 ||
		l.GetLabMaxMemoryBytes() != 1<<30 || l.GetTenantMaxLabs() != 2 {
		t.Fatalf("limits: %+v", l)
	}
}
