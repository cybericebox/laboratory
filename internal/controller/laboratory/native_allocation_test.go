package laboratory

import (
	"testing"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNativeAllocationRetainsEveryUnprovenOwner(t *testing.T) {
	now := metav1.Now()
	id := lab.OwnedRuntimeIdentity{OwnerUID: "lab", OperationID: "stop", Revision: 2, PodUID: "old-pod", NodeName: "node", NodeBootID: "boot", ContainerIDs: []string{"task"}, CgroupPaths: []string{"/owned/cgroup"}, PortKeys: []string{"owned-port"}, Requests: lab.ResourceAmounts{CPUMillicores: 80, MemoryBytes: 32 << 20}, Limits: lab.ResourceAmounts{CPUMillicores: 150, MemoryBytes: 64 << 20}}
	good := lab.OwnedRuntimeReport{Identity: id, RuntimeState: "Released", ObservedAt: &now, RuntimeAbsentAt: &now, CgroupAbsentAt: &now, AttachmentsAbsentAt: &now}
	for _, tc := range []struct {
		name   string
		change func(*lab.OwnedRuntimeReport)
	}{
		{"terminating process", func(r *lab.OwnedRuntimeReport) { r.RuntimeState = "Releasing"; r.RuntimeAbsentAt = nil }},
		{"force deleted API with live task", func(r *lab.OwnedRuntimeReport) { r.RuntimeAbsentAt = nil }},
		{"absent node", func(r *lab.OwnedRuntimeReport) { r.Identity.NodeName = "other" }},
		{"old pod UID", func(r *lab.OwnedRuntimeReport) { r.Identity.PodUID = "replacement" }},
		{"cleanup error", func(r *lab.OwnedRuntimeReport) { r.Error = "OVS unreachable" }},
		{"new start generation", func(r *lab.OwnedRuntimeReport) { r.Identity.OperationID = "previous"; r.Identity.Revision = 1 }},
		{"missing cgroup proof", func(r *lab.OwnedRuntimeReport) { r.CgroupAbsentAt = nil }},
		{"old node boot", func(r *lab.OwnedRuntimeReport) { r.Identity.NodeBootID = "old-boot" }},
		{"no physical ACK", func(r *lab.OwnedRuntimeReport) { r.AttachmentsAbsentAt = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := good
			tc.change(&report)
			a := aggregateRuntime([]lab.OwnedRuntimeIdentity{id}, []lab.OwnedRuntimeReport{report}, "lab", "stop", 2)
			if a.RuntimeState != "Unknown" || a.AllocatedRequests != id.Requests || a.ReleasedAt != nil {
				t.Fatalf("uncertainty credited capacity: %+v", a)
			}
		})
	}
	a := aggregateRuntime([]lab.OwnedRuntimeIdentity{id}, []lab.OwnedRuntimeReport{good}, "lab", "stop", 2)
	if a.RuntimeState != "Released" || a.AllocatedRequests != (lab.ResourceAmounts{}) || a.ConfiguredLimits != id.Limits {
		t.Fatalf("release/configured limits lost: %+v", a)
	}
	// Independent owners can report in any clock order. Missing proof is forbidden,
	// artificial ordering between valid acknowledgements is also forbidden.
	old := metav1.NewTime(time.Unix(10, 0))
	good.AttachmentsAbsentAt = &old
	if !runtimeRowsReleased([]lab.OwnedRuntimeIdentity{id}, []lab.OwnedRuntimeReport{good}, "lab", "stop", 2) {
		t.Fatal("cross-owner clock ordering required")
	}
}
