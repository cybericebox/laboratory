package laboratory

import (
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// Consumption fixtures do not claim native producer provenance. The Linux
// historical NeverMaterialized producer tests exercise the actual observer.
func TestHistoricalNeverMaterializedCertificateJoinsDeploymentSkip(t *testing.T) {
	old := lab.OwnedRuntimeIdentity{ScopeKind: "NeverMaterialized", ScopeUID: "device", OwnerUID: "lab", Namespace: "ns", LabName: "l", Generation: 1, OperationID: "initial", Revision: 1, NodeName: "node", NodeBootID: "boot", ContainerIDs: []string{}, CgroupPaths: []string{}, PortKeys: []string{}}
	complete := *old.DeepCopy()
	complete.ContainerIDs, complete.CgroupPaths, complete.AttachmentsComplete = []string{"original-container"}, []string{"original-cgroup"}, true
	oldReport := waveReleased(complete)
	pod := lab.OwnedRuntimeIdentity{ScopeUID: "device", OwnerUID: "lab", Namespace: "ns", LabName: "l", Generation: 2, OperationID: "stop", Revision: 2, NodeName: "node", NodeBootID: "boot", PodUID: "original-pod", ContainerIDs: []string{"original-container"}, CgroupPaths: []string{"original-cgroup"}, AttachmentsComplete: true, Requests: lab.ResourceAmounts{CPUMillicores: 100, MemoryBytes: 268435456}, Limits: lab.ResourceAmounts{CPUMillicores: 100, MemoryBytes: 268435456}}
	fabric := lab.OwnedRuntimeIdentity{ScopeKind: "LabFabric", ScopeUID: "lab", OwnerUID: "lab", Namespace: "ns", LabName: "l", Generation: 2, OperationID: "stop", Revision: 2, NodeName: "node", NodeBootID: "boot", AttachmentsComplete: true}
	for _, scenario := range []string{"missing-old-proof", "replacement-device", "incomplete-native-proof", "complete-native-proof"} {
		t.Run(scenario, func(t *testing.T) {
			reports := []lab.OwnedRuntimeReport{waveReleased(pod), waveReleased(fabric)}
			candidate := *oldReport.DeepCopy()
			switch scenario {
			case "replacement-device":
				candidate.Identity.ScopeUID = "replacement"
			case "incomplete-native-proof":
				candidate.CgroupAbsentAt = nil
			}
			if scenario != "missing-old-proof" {
				reports = append(reports, candidate)
			}
			rows := adoptReleasedScopeHistory([]lab.OwnedRuntimeIdentity{old, fabric}, reports)
			rows = append(rows, pod)
			allocation := aggregateRuntime(rows, reports, "lab", "stop", 2)
			wantReleased := scenario == "complete-native-proof"
			if (allocation.RuntimeState == "Released") != wantReleased || wantReleased && allocation.AllocatedRequests != (lab.ResourceAmounts{}) {
				t.Fatalf("old scope proof qualification changed: %+v", allocation)
			}
			if wantReleased && (len(rows[0].ContainerIDs) != 1 || len(rows[0].CgroupPaths) != 1 || rows[0].Generation != 1 || rows[0].Revision != 1) {
				t.Fatal("old positive native inventory was dropped or rebound")
			}
		})
	}
}
