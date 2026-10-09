package laboratory

import (
	"testing"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func retainedRetirementFixture() ([]lab.OwnedRuntimeIdentity, []lab.OwnedRuntimeReport, lab.LifecycleRetirementIntent) {
	in := lab.LifecycleRetirementIntent{ExpectedUID: "lab", StopOperationID: "stop", StopRevision: 2, Generation: 2, OperationID: "retire", Revision: 3, RequestedAt: metav1.NewTime(time.Unix(200, 0))}
	old := lab.OwnedRuntimeIdentity{ScopeKind: "LabFabric", ScopeUID: "lab", OwnerUID: "lab", Namespace: "ns", LabName: "l", Generation: 1, OperationID: "initial", Revision: 1, NodeName: "node", NodeBootID: "boot", ContainerIDs: []string{"original-container", "original-sandbox"}, CgroupPaths: []string{"original-container-cgroup", "original-sandbox-cgroup", "original-pod-cgroup"}, PortKeys: []string{}, AttachmentsComplete: true}
	never := *old.DeepCopy()
	never.ScopeKind, never.ScopeUID = "NeverMaterialized", "device"
	never.CgroupPaths = never.CgroupPaths[:2]
	current := *old.DeepCopy()
	current.OperationID, current.Revision, current.Generation = "stop", 2, 2
	pod := *never.DeepCopy()
	pod.ScopeKind, pod.PodUID, pod.OperationID, pod.Revision, pod.Generation = "", "original-pod", "stop", 2, 2
	rows := []lab.OwnedRuntimeIdentity{old, never, current, pod}
	when := metav1.NewTime(time.Unix(100, 0)) // independent native clock
	var reports []lab.OwnedRuntimeReport
	for _, id := range rows {
		reports = append(reports, lab.OwnedRuntimeReport{Identity: *id.DeepCopy(), RuntimeState: "Released", ObservedAt: &when, RuntimeAbsentAt: &when, CgroupAbsentAt: &when, AttachmentsAbsentAt: &when, RetirementOperationID: in.OperationID, RetirementRevision: in.Revision})
	}
	return rows, reports, in
}

func TestRetirementRetainedPositiveHistoricalRowsRequireFreshChallenge(t *testing.T) {
	rows, reports, in := retainedRetirementFixture()
	when := metav1.Now()
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{UID: "lab", Generation: 2}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", SnapshotMode: "Skip", OperationID: "stop", Revision: 2}}, Status: lab.LabStatus{Lifecycle: &lab.LabLifecycleStatus{LabUID: "lab", OperationID: "stop", Revision: 2, ObservedGeneration: 2, ObservedState: "Stopped", StoppedAt: &when}, Resources: &lab.RuntimeAllocation{OperationID: "stop", Revision: 2, RuntimeState: "Released", ObservedAt: &when, ReleasedAt: &when}}}
	if !RetirementReady(l) {
		t.Fatal("fixture lacks exact current stopped certificate")
	}
	if runtimeRowsReleased(rows, reports, in.ExpectedUID, in.StopOperationID, in.StopRevision) {
		t.Fatal("generic current-operation matcher accepted historical tuples")
	}
	if !freshRetirementRows(rows, reports, in) {
		t.Fatal("fresh exact native challenge rejected retained original positive historical debt")
	}
}

func TestRetirementRetainedRowsRejectIncompleteOrForeignChallenge(t *testing.T) {
	changes := map[string]func(*lab.OwnedRuntimeIdentity, *lab.OwnedRuntimeReport){
		"owner":             func(id *lab.OwnedRuntimeIdentity, _ *lab.OwnedRuntimeReport) { id.OwnerUID = "foreign" },
		"missing-operation": func(id *lab.OwnedRuntimeIdentity, _ *lab.OwnedRuntimeReport) { id.OperationID = "" },
		"future-revision":   func(id *lab.OwnedRuntimeIdentity, _ *lab.OwnedRuntimeReport) { id.Revision = 9 },
		"future-generation": func(id *lab.OwnedRuntimeIdentity, _ *lab.OwnedRuntimeReport) { id.Generation = 9 },
		"node":              func(id *lab.OwnedRuntimeIdentity, _ *lab.OwnedRuntimeReport) { id.NodeName = "foreign" },
		"boot":              func(id *lab.OwnedRuntimeIdentity, _ *lab.OwnedRuntimeReport) { id.NodeBootID = "foreign" },
		"scope-owner":       func(id *lab.OwnedRuntimeIdentity, _ *lab.OwnedRuntimeReport) { id.ScopeUID = "foreign" },
		"physical-debt": func(id *lab.OwnedRuntimeIdentity, _ *lab.OwnedRuntimeReport) {
			id.ContainerIDs = []string{"replacement"}
		},
		"unknown":          func(_ *lab.OwnedRuntimeIdentity, r *lab.OwnedRuntimeReport) { r.RuntimeState = "Unknown" },
		"error":            func(_ *lab.OwnedRuntimeIdentity, r *lab.OwnedRuntimeReport) { r.Error = "unresolved" },
		"nonce":            func(_ *lab.OwnedRuntimeIdentity, r *lab.OwnedRuntimeReport) { r.RetirementOperationID = "foreign" },
		"nonce-revision":   func(_ *lab.OwnedRuntimeIdentity, r *lab.OwnedRuntimeReport) { r.RetirementRevision = 9 },
		"observed-stamp":   func(_ *lab.OwnedRuntimeIdentity, r *lab.OwnedRuntimeReport) { r.ObservedAt = nil },
		"runtime-stamp":    func(_ *lab.OwnedRuntimeIdentity, r *lab.OwnedRuntimeReport) { r.RuntimeAbsentAt = nil },
		"cgroup-stamp":     func(_ *lab.OwnedRuntimeIdentity, r *lab.OwnedRuntimeReport) { r.CgroupAbsentAt = nil },
		"attachment-stamp": func(_ *lab.OwnedRuntimeIdentity, r *lab.OwnedRuntimeReport) { r.AttachmentsAbsentAt = nil },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			rows, reports, in := retainedRetirementFixture()
			change(&rows[0], &reports[0])
			if freshRetirementRows(rows, reports, in) {
				t.Fatal("retirement accepted foreign or incomplete original debt proof")
			}
		})
	}
	rows, reports, in := retainedRetirementFixture()
	reports = reports[1:]
	if freshRetirementRows(rows, reports, in) {
		t.Fatal("missing historical scope proof accepted")
	}
}
