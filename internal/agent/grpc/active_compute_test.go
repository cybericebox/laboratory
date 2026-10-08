package grpc

import (
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/limits"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestActivePendingComputeRequiresExactNativeRelease(t *testing.T) {
	now := metav1.Now()
	h := &Handler{}
	h.features.Limits = limits.Limits{DeviceDefaultCPU: 100, DeviceDefaultMemory: 256 << 20}
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{UID: "u", Generation: 2}, Spec: lab.LabSpec{Devices: []lab.DeviceTemplate{{Name: "device", Type: lab.DeviceTypeContainer}}, Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 1, SnapshotMode: "Skip"}}, Status: lab.LabStatus{Lifecycle: &lab.LabLifecycleStatus{LabUID: "u", OperationID: "stop", Revision: 1, ObservedGeneration: 2, ObservedState: "Stopped"}, Resources: &lab.RuntimeAllocation{OperationID: "stop", Revision: 1, RuntimeState: "Released", ObservedAt: &now, ReleasedAt: &now}}}
	if cpu, mem := h.activeLabCompute(l); cpu != 0 || mem != 0 {
		t.Fatal("retained released config consumed active compute")
	}
	l.Spec.Lifecycle.OperationID = "start"
	l.Spec.Lifecycle.Revision = 2
	l.Spec.Lifecycle.DesiredState = "Running"
	if cpu, mem := h.activeLabCompute(l); cpu != 100 || mem != 256<<20 {
		t.Fatal("old release credited pending new start")
	}
}
