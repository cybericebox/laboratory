package grpc

import (
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestImmutableGroupSizeProjectionAndFrozenTags(t *testing.T) {
	desc := (&protobuf.LabGroup{}).ProtoReflect().Descriptor().Fields()
	for name, tag := range map[string]int{"vpn_size": 16, "gateway_size": 17} {
		if f := desc.ByNumber(protoreflect.FieldNumber(tag)); f == nil || string(f.Name()) != name {
			t.Fatalf("frozen tag %d differs", tag)
		}
	}
	g := &lab.LabGroup{}
	if p := labGroupToProto(g); p.GetVpnSize() != nil || p.GetGatewaySize() != nil {
		t.Fatal("legacy chart default inferred")
	}
	g.Spec.VPN.Size = &lab.GroupPodSize{CPUMillicores: 100, MemoryBytes: 64 << 20}
	g.Spec.Gateway.Size = &lab.GroupPodSize{CPUMillicores: 250, MemoryBytes: 128 << 20}
	p := labGroupToProto(g)
	if p.GetVpnSize().GetCpuMillicores() != 100 || p.GetGatewaySize().GetMemoryBytes() != 128<<20 {
		t.Fatal("immutable sizing projection lost")
	}
	g.Spec.Gateway.Size.MemoryBytes = 0
	if labGroupToProto(g).GetGatewaySize() != nil {
		t.Fatal("invalid size projected")
	}
}
