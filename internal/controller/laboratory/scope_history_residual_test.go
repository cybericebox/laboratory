package laboratory

import (
	"context"
	"reflect"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These test certificate consumption only. Native producer provenance is proved
// separately by the Linux historical-scope adapter test, not by these fixtures.
func residualHistoryCertificate() (lab.OwnedRuntimeIdentity, lab.OwnedRuntimeReport) {
	id := lab.OwnedRuntimeIdentity{ScopeKind: "GroupScope", ScopeUID: "group", OwnerUID: "group", Namespace: "ns", Generation: 2, OperationID: "stop1", Revision: 1, NodeName: "node", NodeBootID: "boot", ContainerIDs: []string{"old-container"}, CgroupPaths: []string{"/old-cgroup"}, PortKeys: []string{"p12345678"}, PortRows: []lab.OwnedFabricPort{{Key: "p12345678", OwnerUID: "pod", RowUUID: "row"}}, FabricPorts: []lab.OwnedFabricPort{{Key: "pt1234567890", OwnerUID: "connection", RowUUID: "fabric-row"}}, VNIs: []uint{901}, VNIBindings: []lab.OwnedVNI{{Kind: "Connection", UID: "connection", VNI: 901, PoolUID: "pool", LeaseGeneration: 1}}}
	complete := *id.DeepCopy()
	complete.AttachmentsComplete = true
	complete.ContainerIDs = append(complete.ContainerIDs, "newly-observed-original-container")
	complete.CgroupPaths = append(complete.CgroupPaths, "/newly-observed-original-cgroup")
	now := metav1.Now()
	return id, lab.OwnedRuntimeReport{Identity: complete, RuntimeState: "Released", ObservedAt: &now, RuntimeAbsentAt: &now, CgroupAbsentAt: &now, AttachmentsAbsentAt: &now}
}
func TestResidualHistoricalScopeAdoptionRejectsChangedIdentityDroppedDebtAndIncompleteProof(t *testing.T) {
	id, good := residualHistoryCertificate()
	changes := map[string]func(*lab.OwnedRuntimeReport){
		"namespace":       func(r *lab.OwnedRuntimeReport) { r.Identity.Namespace = "foreign" },
		"lab-name":        func(r *lab.OwnedRuntimeReport) { r.Identity.LabName = "foreign" },
		"scope-kind":      func(r *lab.OwnedRuntimeReport) { r.Identity.ScopeKind = "LabFabric" },
		"scope-uid":       func(r *lab.OwnedRuntimeReport) { r.Identity.ScopeUID = "foreign" },
		"owner":           func(r *lab.OwnedRuntimeReport) { r.Identity.OwnerUID = "foreign" },
		"operation":       func(r *lab.OwnedRuntimeReport) { r.Identity.OperationID = "different" },
		"revision":        func(r *lab.OwnedRuntimeReport) { r.Identity.Revision++ },
		"generation":      func(r *lab.OwnedRuntimeReport) { r.Identity.Generation++ },
		"node":            func(r *lab.OwnedRuntimeReport) { r.Identity.NodeName = "different" },
		"boot":            func(r *lab.OwnedRuntimeReport) { r.Identity.NodeBootID = "different" },
		"pod":             func(r *lab.OwnedRuntimeReport) { r.Identity.PodUID = "foreign" },
		"deployment":      func(r *lab.OwnedRuntimeReport) { r.Identity.DeploymentUID = "foreign" },
		"component":       func(r *lab.OwnedRuntimeReport) { r.Identity.Component = "foreign" },
		"epoch":           func(r *lab.OwnedRuntimeReport) { r.Identity.Epoch++ },
		"incarnation":     func(r *lab.OwnedRuntimeReport) { r.Identity.Incarnation++ },
		"requests":        func(r *lab.OwnedRuntimeReport) { r.Identity.Requests.MemoryBytes++ },
		"limits":          func(r *lab.OwnedRuntimeReport) { r.Identity.Limits.MemoryBytes++ },
		"containers":      func(r *lab.OwnedRuntimeReport) { r.Identity.ContainerIDs = nil },
		"cgroups":         func(r *lab.OwnedRuntimeReport) { r.Identity.CgroupPaths = nil },
		"port-keys":       func(r *lab.OwnedRuntimeReport) { r.Identity.PortKeys = nil },
		"port-rows":       func(r *lab.OwnedRuntimeReport) { r.Identity.PortRows = nil },
		"fabric-rows":     func(r *lab.OwnedRuntimeReport) { r.Identity.FabricPorts = nil },
		"vnis":            func(r *lab.OwnedRuntimeReport) { r.Identity.VNIs = nil },
		"leases":          func(r *lab.OwnedRuntimeReport) { r.Identity.VNIBindings = nil },
		"observed-time":   func(r *lab.OwnedRuntimeReport) { r.ObservedAt = nil },
		"runtime-time":    func(r *lab.OwnedRuntimeReport) { r.RuntimeAbsentAt = nil },
		"cgroup-time":     func(r *lab.OwnedRuntimeReport) { r.CgroupAbsentAt = nil },
		"attachment-time": func(r *lab.OwnedRuntimeReport) { r.AttachmentsAbsentAt = nil },
		"error":           func(r *lab.OwnedRuntimeReport) { r.Error = "unknown" },
		"state":           func(r *lab.OwnedRuntimeReport) { r.RuntimeState = "Allocated" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			candidate := *good.DeepCopy()
			change(&candidate)
			adopted := adoptReleasedScopeHistory([]lab.OwnedRuntimeIdentity{id}, []lab.OwnedRuntimeReport{candidate})
			if !reflect.DeepEqual(adopted[0], id) {
				t.Fatal("invalid historical certificate adopted")
			}
		})
	}
	adopted := adoptReleasedScopeHistory([]lab.OwnedRuntimeIdentity{id}, []lab.OwnedRuntimeReport{good})
	if !reflect.DeepEqual(adopted[0], good.Identity) || !runtimeRowsReleased(adopted, []lab.OwnedRuntimeReport{good}, id.OwnerUID, id.OperationID, id.Revision) {
		t.Fatal("valid native scope extension did not reach exact release matcher")
	}
}
func TestResidualGroupStartAndLabPreparationAdoptOnlyCompletedHistoricalScope(t *testing.T) {
	ctx := context.Background()
	r, g, c := residualGroupStartFixture(t, false)
	original, report := residualHistoryCertificate()
	if err := c.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
		t.Fatal(err)
	}
	g.Status.ServiceRuntime = []lab.OwnedRuntimeIdentity{original}
	g.Status.ServiceReports = []lab.OwnedRuntimeReport{report}
	if err := c.Status().Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	handled, _, err := r.reconcileGroupLifecycle(ctx, g)
	if err != nil || handled {
		t.Fatalf("exact old release remained permanently waiting: handled=%v err=%v", handled, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(g), g); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g.Status.ServiceRuntime[0], report.Identity) {
		t.Fatal("Group Running branch did not persist native extension")
	}
	if g.Status.Lifecycle.ObservedState == "Running" {
		t.Fatal("historical release alone advertised current Running")
	}
	scheme := retentionScheme(t)
	_ = corev1.AddToScheme(scheme)
	old := *original.DeepCopy()
	old.ScopeKind = "LabFabric"
	old.OwnerUID = "lab"
	old.ScopeUID = "lab"
	old.LabName = "l"
	oldReport := *report.DeepCopy()
	oldReport.Identity = *old.DeepCopy()
	oldReport.Identity.AttachmentsComplete = true
	oldReport.Identity.CgroupPaths = append(oldReport.Identity.CgroupPaths, "/newly-observed-original-cgroup")
	parent := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab", Generation: 3}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Running", OperationID: "new-start", Revision: 2}}, Status: lab.LabStatus{ScopeInventory: []lab.OwnedRuntimeIdentity{old}, ScopeReports: []lab.OwnedRuntimeReport{oldReport}}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: map[string]string{names.LabelNodeAgentReady: "true", corev1.LabelOSStable: "linux"}}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	lc := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(parent).WithObjects(parent, node).Build()
	lr := &LabReconciler{Client: lc, Reader: lc, RuntimeObservation: true}
	if err := lr.prepareLabScopes(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parent.Status.ScopeInventory[0], oldReport.Identity) {
		t.Fatal("Lab preparation did not preserve completed old native extension")
	}
}
