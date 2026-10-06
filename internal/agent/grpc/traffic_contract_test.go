package grpc

import (
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"google.golang.org/protobuf/proto"
)

func TestTrafficReportWireContractIncludesBothInitiators(t *testing.T) {
	input := &lab.LabTrafficReport{Status: lab.LabTrafficReportStatus{
		Partial: true, Ledger: []lab.LabTrafficTouch{{Subject: "p1", LabName: "a", Attempts: 2, LabInitiatedAttempts: 3, PacketsOut: 300, PacketsIn: 200, BytesOut: 3000, BytesIn: 2000}},
		KernelCheckpoints: []lab.LabTrafficKernelCheckpoint{{Subject: "p1", LabName: "a", BindingID: "private-binding", Epoch: "private-epoch"}},
	}}
	wire := trafficReportToProto(input, "group")
	raw, err := proto.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var result protobuf.TrafficReport
	if err := proto.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Partial || len(result.Ledger) != 1 {
		t.Fatalf("lost coverage/ledger: %+v", &result)
	}
	r := result.Ledger[0]
	if r.Attempts != 2 || r.LabInitiatedAttempts != 3 || r.PacketsOut != 300 || r.PacketsIn != 200 || r.BytesOut != 3000 || r.BytesIn != 2000 {
		t.Fatalf("counter/initiator semantics lost on wire: %+v", r)
	}
	if result.ProtoReflect().Descriptor().Fields().ByName("kernel_checkpoints") != nil {
		t.Fatal("private kernel checkpoints became a public protocol field")
	}
}
