package grpc

import (
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func retiredStorageToProto(l *lab.Lab, out *protobuf.ResourceAllocation) *protobuf.ResourceAllocation {
	f, ok := lab.ParseSnapshotRetirement(l.Annotations[names.AnnotationSnapshotRetirement])
	i := l.Spec.Lifecycle
	if !ok || i == nil || f.LabUID != string(l.UID) || f.OperationID != i.OperationID || f.Revision != i.Revision {
		return out
	}
	if out == nil {
		out = &protobuf.ResourceAllocation{RuntimeState: "Unknown"}
	}
	out.StorageState = f.State
	// Manifest deletion never certifies physical reclamation or a measured zero.
	out.PhysicalStorageBytesAvailable = false
	out.PhysicalStorageBytes = 0
	if f.State == "Deleted" {
		out.SnapshotQuotaBytes = 0
	}
	return out
}
