package grpc

import (
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func retirementToProto(object metav1.Object, status *lab.LifecycleRetirementStatus) *protobuf.RetirementStatus {
	in, valid := lab.ParseLifecycleRetirement(object.GetAnnotations()[names.AnnotationLifecycleRetirement])
	if !valid || in.ExpectedUID != string(object.GetUID()) {
		return nil
	}
	out := &protobuf.RetirementStatus{ExpectedUid: in.ExpectedUID, StopOperationId: in.StopOperationID, StopRevision: in.StopRevision, OperationId: in.OperationID, Revision: in.Revision, State: "Unknown", StorageState: "Unknown", RequestedUnixMs: in.RequestedAt.UnixMilli()}
	if status == nil || status.ExpectedUID != in.ExpectedUID || status.StopOperationID != in.StopOperationID || status.StopRevision != in.StopRevision || status.OperationID != in.OperationID || status.Revision != in.Revision || status.ObservedGeneration != object.GetGeneration() {
		return out
	}
	out.ObservedGeneration = status.ObservedGeneration
	out.State = status.State
	out.ObservedUnixMs = ms(status.ObservedAt)
	out.RuntimeAbsent = status.RuntimeAbsent
	out.StorageState = status.StorageState
	out.CleanupComplete = status.CleanupComplete
	out.Error = status.Error
	// There is no actual physical registry-GC producer in this feature.
	out.PhysicalStorageBytesAvailable = false
	out.PhysicalStorageBytes = 0
	return out
}
