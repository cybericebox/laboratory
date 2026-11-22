package grpc

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// The lab status carries one entry per console cable (contract 3.1), with both ends, the line state and the reason while it is down.
func TestLabStatusReportsConsoleCables(t *testing.T) {
	l := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns"}}
	l.Status.ConsoleCables = []laboratoryv1alpha1.ConsoleCableRef{
		{Name: "a", Serial: laboratoryv1alpha1.PortRef{Device: "pc1", Port: "serial0"}, Console: laboratoryv1alpha1.PortRef{Device: "r1", Port: "console0"}, LineUp: true},
		{Name: "b", Serial: laboratoryv1alpha1.PortRef{Device: "pc1", Port: "serial1"}, Console: laboratoryv1alpha1.PortRef{Device: "sw", Port: "console0"},
			Reason: laboratoryv1alpha1.ConsoleCableReasonWaitingForDevice},
	}
	got := labToProto(l).GetStatus().GetConsoleCables()
	if len(got) != 2 {
		t.Fatalf("cables: %+v", got)
	}
	if c := got[0]; c.GetSerialDevice() != "pc1" || c.GetSerialPort() != "serial0" || c.GetConsoleDevice() != "r1" || c.GetConsolePort() != "console0" ||
		!c.GetLineUp() || c.GetReason() != "" {
		t.Fatalf("first cable: %+v", c)
	}
	if c := got[1]; c.GetLineUp() || c.GetReason() != "WaitingForDevice" || c.GetConsoleDevice() != "sw" {
		t.Fatalf("second cable: %+v", c)
	}
}
