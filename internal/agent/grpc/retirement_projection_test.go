package grpc

import (
	"encoding/json"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRetirementManifestDeletionCannotClaimPhysicalGC(t *testing.T) {
	f := lab.SnapshotRetirementFence{LabUID: "uid", OperationID: "op", Revision: 1, State: "Deleted"}
	raw, _ := json.Marshal(f)
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{UID: "uid", Annotations: map[string]string{names.AnnotationSnapshotRetirement: string(raw)}}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1}}}
	out := retiredStorageToProto(l, &protobuf.ResourceAllocation{RuntimeState: "Released", SnapshotQuotaBytes: 42, PhysicalStorageBytesAvailable: true, PhysicalStorageBytes: 100})
	if out.PhysicalStorageBytesAvailable || out.PhysicalStorageBytes != 0 || out.StorageState != "Deleted" || out.SnapshotQuotaBytes != 0 {
		t.Fatalf("fake GC result %v", out)
	}
}
