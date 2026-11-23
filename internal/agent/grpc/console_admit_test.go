package grpc

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func consoleLab(mod func(*laboratoryv1alpha1.LabSpec)) func(*laboratoryv1alpha1.LabSpec) {
	return func(s *laboratoryv1alpha1.LabSpec) {
		s.Devices[0].ConsolePorts = []laboratoryv1alpha1.ConsolePortSpec{{Name: "console0"}}
		s.Devices[1].SerialPorts = []laboratoryv1alpha1.SerialPortSpec{{Name: "serial0"}}
		s.ConsoleCables = []laboratoryv1alpha1.ConsoleCableTemplate{{
			Serial: laboratoryv1alpha1.PortRef{Device: "pc", Port: "serial0"}, Console: laboratoryv1alpha1.PortRef{Device: "r1", Port: "console0"}}}
		if mod != nil {
			mod(s)
		}
	}
}

// A lab with valid ports and a cable is created; one whose cable breaks a rule of the design (14.1) is refused as a whole call with
// InvalidArgument and the ErrorInfo reason CONSOLE_CABLE_INVALID, the device and the port named.
func TestCreateLabsChecksConsoleCables(t *testing.T) {
	h, _ := contractHandler(t, testFeatures)
	wantStates(t, createContractLab(t, h, &protobuf.LabVariant{SpecJson: contractSpecJSON(t, consoleLab(nil), "r1", "pc")}, &protobuf.LabItem{}), nil, stCreated)

	for name, c := range map[string]struct {
		mod  func(*laboratoryv1alpha1.LabSpec)
		want string
	}{
		"unresolved end": {func(s *laboratoryv1alpha1.LabSpec) { s.ConsoleCables[0].Console.Port = "aux0" }, `"aux0"`},
		"port named like a NIC": {func(s *laboratoryv1alpha1.LabSpec) {
			s.Devices[0].Interfaces = append(s.Devices[0].Interfaces, laboratoryv1alpha1.InterfaceSpec{Name: "console0"})
		}, "name of an interface"},
		"two cables on one port": {func(s *laboratoryv1alpha1.LabSpec) {
			s.Devices[1].SerialPorts = append(s.Devices[1].SerialPorts, laboratoryv1alpha1.SerialPortSpec{Name: "serial1"})
			s.ConsoleCables = append(s.ConsoleCables, laboratoryv1alpha1.ConsoleCableTemplate{
				Serial: laboratoryv1alpha1.PortRef{Device: "pc", Port: "serial1"}, Console: laboratoryv1alpha1.PortRef{Device: "r1", Port: "console0"}})
		}, "more than one console cable"},
		// the kernel log of a console port (contract 3.2) is a microvm device's only: on a container it would be the node's kernel
		"kernel log on a container device": {func(s *laboratoryv1alpha1.LabSpec) { s.Devices[0].ConsolePorts[0].KernelLog = true },
			`device "r1": the port "console0" has kernelLog, which only a microvm device has`},
		"two stdio": {func(s *laboratoryv1alpha1.LabSpec) {
			stdio := &laboratoryv1alpha1.NativeConsoleSpec{Kind: "stdio"}
			s.Devices[0].ConsolePorts = []laboratoryv1alpha1.ConsolePortSpec{{Name: "console0", Native: stdio}, {Name: "aux0", Native: stdio}}
		}, "stdio"},
	} {
		h, _ := contractHandler(t, testFeatures)
		_, err := h.CreateLabs(context.Background(), &protobuf.CreateLabsRequest{
			Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: contractSpecJSON(t, consoleLab(c.mod), "r1", "pc")}},
			Items:    []*protobuf.LabItem{{LabGroup: "team-lab", Name: "c1", VariantId: "v"}},
		})
		st := status.Convert(err)
		if st.Code() != codes.InvalidArgument || !strings.Contains(st.Message(), c.want) {
			t.Errorf("%s: got %v, want InvalidArgument with %q", name, err, c.want)
			continue
		}
		reason := ""
		for _, d := range st.Details() {
			if info, ok := d.(*errdetails.ErrorInfo); ok {
				reason = info.GetReason()
			}
		}
		if reason != client.ReasonConsoleCableInvalid {
			t.Errorf("%s: ErrorInfo reason %q, want %s", name, reason, client.ReasonConsoleCableInvalid)
		}
	}
}
