package laboratory

import (
	"context"
	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func residualPlacementNodes() (*corev1.Node, *corev1.Node) {
	worker := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{corev1.LabelOSStable: "linux", names.LabelNodeAgentReady: "true"}}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", BootID: "worker-boot"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	cp := worker.DeepCopy()
	cp.Name = "control-plane"
	cp.Status.NodeInfo.BootID = "cp-boot"
	delete(cp.Labels, names.LabelNodeAgentReady)
	cp.Spec.Taints = []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}
	return worker, cp
}
func TestResidualConfiguredPlacementDefaultControlPlaneDoesNotBlockLabScopes(t *testing.T) {
	ctx := context.Background()
	s := pruneScheme(t)
	_ = corev1.AddToScheme(s)
	_ = allocation.AddToScheme(s)
	worker, cp := residualPlacementNodes()
	parent := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab", Generation: 3}}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(worker, cp, parent).Build()
	r := &LabReconciler{Client: c, Reader: c, RuntimeObservation: true}
	if err := r.prepareLabScopes(ctx, parent); err != nil {
		t.Fatalf("default tainted non-placement CP blocked actual worker scope: %v", err)
	}
	if len(parent.Status.ScopeInventory) != 1 || parent.Status.ScopeInventory[0].NodeName != "worker" || parent.Status.ScopeInventory[0].NodeBootID != "worker-boot" {
		t.Fatalf("scope did not match actual configured observer pool: %+v", parent.Status.ScopeInventory)
	}
}
func TestResidualConfiguredPlacementDefaultControlPlaneDoesNotBlockGroupScope(t *testing.T) {
	ctx := context.Background()
	s := pruneScheme(t)
	_ = corev1.AddToScheme(s)
	_ = allocation.AddToScheme(s)
	worker, cp := residualPlacementNodes()
	g := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g", UID: "group", Generation: 3}, Spec: lab.LabGroupSpec{Lifecycle: &lab.GroupLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 1, RequireAllLabsStopped: true}}, Status: lab.LabGroupStatus{Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(g).WithObjects(worker, cp, g).Build()
	r := &LabGroupReconciler{Client: c, Reader: c, ServiceReleaseObserver: NativeGroupServiceReleaseObserver{Client: c}}
	prepared, err := r.PrepareGroupRelease(ctx, g)
	if err != nil || !prepared {
		t.Fatalf("default CP blocked valid native group declaration: prepared=%v err=%v", prepared, err)
	}
	if len(g.Status.ServiceRuntime) != 1 || g.Status.ServiceRuntime[0].NodeName != "worker" {
		t.Fatalf("group scope selected excluded CP: %+v", g.Status.ServiceRuntime)
	}
}
func TestResidualConfiguredPlacementNeverFiltersUnavailableSelectedWorker(t *testing.T) {
	for _, scenario := range []string{"missing-ready", "not-ready", "unreachable", "cordon", "memory-pressure", "disk-pressure", "network-unavailable", "cni-bootstrap"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s := pruneScheme(t)
			_ = corev1.AddToScheme(s)
			_ = allocation.AddToScheme(s)
			healthy, _ := residualPlacementNodes()
			healthy.Labels["pool"] = "labs"
			unavailable := healthy.DeepCopy()
			unavailable.Name = "second-worker"
			unavailable.Status.NodeInfo.BootID = "second-boot"
			delete(unavailable.Labels, names.LabelNodeAgentReady)
			switch scenario {
			case "not-ready":
				unavailable.Status.Conditions[0].Status = corev1.ConditionFalse
				unavailable.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoSchedule}}
			case "unreachable":
				unavailable.Status.Conditions[0].Status = corev1.ConditionUnknown
				unavailable.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoExecute}}
			case "cordon":
				unavailable.Spec.Unschedulable = true
				unavailable.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule}}
			case "memory-pressure":
				unavailable.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeMemoryPressure, Effect: corev1.TaintEffectNoSchedule}}
			case "disk-pressure":
				unavailable.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeDiskPressure, Effect: corev1.TaintEffectNoSchedule}}
			case "network-unavailable":
				unavailable.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeNetworkUnavailable, Effect: corev1.TaintEffectNoSchedule}}
			case "cni-bootstrap":
				unavailable.Spec.Taints = []corev1.Taint{{Key: "node.cilium.io/agent-not-ready", Effect: corev1.TaintEffectNoSchedule}}
			}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(healthy, unavailable).Build()
			policy := nativePlacementPolicy{Selector: map[string]string{"pool": "labs", names.LabelNodeAgentReady: "true"}}
			if scopes, err := declaredScopes(ctx, c, "lab", "ns", "l", "stop", 1, 3, "LabFabric", policy); err == nil {
				t.Fatalf("unavailable selected node vanished beside healthy worker: %+v", scopes)
			}
		})
	}
}
func TestResidualConfiguredPlacementStaticExclusionsAndRegisteredObservers(t *testing.T) {
	for _, scenario := range []string{"selector-excluded", "operator-taint", "registered-excluded", "tolerated-cp-needs-observer", "tolerated-cp-with-observer"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s := pruneScheme(t)
			_ = corev1.AddToScheme(s)
			_ = allocation.AddToScheme(s)
			worker, cp := residualPlacementNodes()
			worker.Labels["pool"] = "labs"
			cp.Labels["pool"] = "other"
			p := nativePlacementPolicy{Selector: map[string]string{"pool": "labs", names.LabelNodeAgentReady: "true"}}
			want := 1
			switch scenario {
			case "selector-excluded":
				cp.Spec.Taints = nil
			case "operator-taint":
				cp.Labels["pool"] = "labs"
				cp.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "operator", Effect: corev1.TaintEffectNoSchedule}}
			case "registered-excluded":
				cp.Labels[names.LabelNodeAgentReady] = "true"
				want = 2
			case "tolerated-cp-needs-observer", "tolerated-cp-with-observer":
				cp.Labels["pool"] = "labs"
				p.Tolerations = []corev1.Toleration{{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
				want = 2
				if scenario == "tolerated-cp-with-observer" {
					cp.Labels[names.LabelNodeAgentReady] = "true"
				}
			}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(worker, cp).Build()
			scopes, err := declaredScopes(ctx, c, "lab", "ns", "l", "stop", 1, 3, "LabFabric", p)
			if scenario == "tolerated-cp-needs-observer" {
				if err == nil {
					t.Fatal("new tolerated member inferred empty without observer")
				}
				return
			}
			if err != nil || len(scopes) != want {
				t.Fatalf("configured/registered membership mismatch: %+v %v", scopes, err)
			}
		})
	}
}
func TestResidualConfiguredPlacementRetainsOriginalUIDBootHistory(t *testing.T) {
	for _, scenario := range []string{"excluded-prior-node", "ready-removed-and-taint-change", "node-deleted", "boot-replaced", "missing-history-boot", "foreign-owner", "released-excluded-history"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s := pruneScheme(t)
			_ = corev1.AddToScheme(s)
			_ = allocation.AddToScheme(s)
			worker, cp := residualPlacementNodes()
			worker.Labels["pool"] = "labs"
			cp.Labels["pool"] = "other"
			old := lab.OwnedRuntimeIdentity{ScopeKind: "LabFabric", ScopeUID: "lab", OwnerUID: "lab", Namespace: "ns", LabName: "l", OperationID: "old-stop", Revision: 1, Generation: 2, NodeName: cp.Name, NodeBootID: cp.Status.NodeInfo.BootID}
			parent := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab", Generation: 3}, Status: lab.LabStatus{ScopeInventory: []lab.OwnedRuntimeIdentity{old}}}
			switch scenario {
			case "ready-removed-and-taint-change":
				cp.Spec.Taints = append(cp.Spec.Taints, corev1.Taint{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoExecute})
				cp.Status.Conditions[0].Status = corev1.ConditionUnknown
			case "boot-replaced":
				cp.Status.NodeInfo.BootID = "replacement-boot"
				cp.Labels[names.LabelNodeAgentReady] = "true"
			case "missing-history-boot":
				parent.Status.ScopeInventory[0].NodeBootID = ""
			case "foreign-owner":
				parent.Status.ScopeInventory[0].OwnerUID = "other"
			case "released-excluded-history":
				parent.Status.ScopeInventory[0].AttachmentsComplete = true
				parent.Status.ScopeReports = []lab.OwnedRuntimeReport{waveReleased(parent.Status.ScopeInventory[0])}
			}
			c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(worker, cp, parent).Build()
			if scenario == "node-deleted" {
				if err := c.Delete(ctx, cp); err != nil {
					t.Fatal(err)
				}
			}
			before := parent.DeepCopy()
			r := &LabReconciler{Client: c, Reader: c, RuntimeObservation: true, LabNodeSelector: map[string]string{"pool": "labs", names.LabelNodeAgentReady: "true"}}
			err := r.prepareLabScopes(ctx, parent)
			if scenario == "foreign-owner" || scenario == "released-excluded-history" {
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(parent.Status.ScopeInventory[0], before.Status.ScopeInventory[0]) {
					t.Fatal("retained original UID/boot history was rewritten")
				}
				for _, row := range parent.Status.ScopeInventory[1:] {
					if row.NodeName == cp.Name {
						t.Fatal("excluded resolved/foreign history invented new CP debt")
					}
				}
			} else {
				if err == nil {
					t.Fatal("unresolved original node history was erased/rebound")
				}
				var stored lab.Lab
				if err := c.Get(ctx, client.ObjectKeyFromObject(parent), &stored); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(stored.Status.ScopeInventory, before.Status.ScopeInventory) {
					t.Fatal("failed scope preparation dropped old debt")
				}
			}
		})
	}
}
func TestResidualConfiguredPlacementKeepsUIDOwnedLegacyCPPlacementUnknown(t *testing.T) {
	for _, scenario := range []string{"device-status", "connection-status", "owned-pod", "foreign-device"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s := pruneScheme(t)
			_ = corev1.AddToScheme(s)
			_ = allocation.AddToScheme(s)
			worker, cp := residualPlacementNodes()
			parent := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab", Generation: 3}}
			d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns", UID: "device", OwnerReferences: []metav1.OwnerReference{{Kind: "Lab", Name: "l", UID: parent.UID}}}, Spec: lab.DeviceSpec{LabRef: "l", Name: "d", Type: lab.DeviceTypeContainer}}
			objects := []client.Object{worker, cp, parent, d}
			switch scenario {
			case "device-status", "foreign-device":
				d.Status.NodeName = cp.Name
				d.Status.PodName = "legacy"
				if scenario == "foreign-device" {
					d.OwnerReferences[0].UID = "other"
				}
			case "connection-status":
				objects = append(objects, &lab.Connection{ObjectMeta: metav1.ObjectMeta{Name: "conn", Namespace: "ns", UID: "connection", OwnerReferences: []metav1.OwnerReference{{Kind: "Lab", Name: "l", UID: parent.UID}}}, Spec: lab.ConnectionSpec{LabRef: "l"}, Status: lab.ConnectionStatus{Ports: []lab.ConnectionPortStatus{{NodeName: cp.Name, PodUID: "actual-old-pod", RowUUID: "old-row"}}}})
			case "owned-pod":
				objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "pod", Labels: map[string]string{names.LabelLab: "l"}, OwnerReferences: []metav1.OwnerReference{{Kind: "Device", Name: d.Name, UID: d.UID}}}, Spec: corev1.PodSpec{NodeName: cp.Name}})
			}
			c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(objects...).Build()
			r := &LabReconciler{Client: c, Reader: c, RuntimeObservation: true}
			err := r.prepareLabScopes(ctx, parent)
			if scenario == "foreign-device" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || len(parent.Status.ScopeInventory) != 0 {
				t.Fatalf("unreachable positive legacy CP placement inferred absent: %v %+v", err, parent.Status.ScopeInventory)
			}
		})
	}
}
func TestResidualConfiguredPlacementKeepsActualDeviceUIDHistoryAfterPlacementPointerClears(t *testing.T) {
	for _, scenario := range []string{"cleared-node", "moved-node", "deleted-node", "foreign-scope-report"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s := pruneScheme(t)
			_ = corev1.AddToScheme(s)
			_ = allocation.AddToScheme(s)
			worker, cp := residualPlacementNodes()
			parent := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab", Generation: 3}}
			old := lab.OwnedRuntimeIdentity{OwnerUID: "lab", ScopeUID: "device", Namespace: "ns", LabName: "l", PodUID: "original-cp-pod", NodeName: cp.Name, NodeBootID: cp.Status.NodeInfo.BootID, OperationID: "old-stop", Revision: 1, Generation: 2, ContainerIDs: []string{"actual-old-container"}, CgroupPaths: []string{"/actual-old-cgroup"}, PortKeys: []string{"p12345678"}}
			d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns", UID: "device", OwnerReferences: []metav1.OwnerReference{{Kind: "Lab", Name: "l", UID: parent.UID}}}, Spec: lab.DeviceSpec{LabRef: "l", Name: "d", Type: lab.DeviceTypeContainer}, Status: lab.DeviceStatus{RuntimeInventory: []lab.OwnedRuntimeIdentity{old}}}
			if scenario == "moved-node" {
				d.Status.NodeName = worker.Name
			}
			if scenario == "foreign-scope-report" {
				d.Status.RuntimeInventory = nil
				foreign := old
				foreign.ScopeUID = "unrelated-device"
				d.Status.RuntimeReports = []lab.OwnedRuntimeReport{{Identity: foreign, RuntimeState: "Allocated"}}
			}
			c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent, d).WithObjects(worker, cp, parent, d).Build()
			if scenario == "deleted-node" {
				if err := c.Delete(ctx, cp); err != nil {
					t.Fatal(err)
				}
			}
			r := &LabReconciler{Client: c, Reader: c, RuntimeObservation: true}
			err := r.prepareLabScopes(ctx, parent)
			if scenario == "foreign-scope-report" {
				if err != nil {
					t.Fatal(err)
				}
				if len(parent.Status.ScopeInventory) == 0 {
					t.Fatal("worker scope missing")
				}
				for _, scope := range parent.Status.ScopeInventory {
					if scope.NodeName != worker.Name {
						t.Fatal("unrelated report widened owned placement")
					}
				}
				return
			}
			if err == nil {
				t.Fatal("original UID/boot Device debt vanished when current placement cleared/moved")
			}
			var stored lab.Device
			if err := c.Get(ctx, client.ObjectKeyFromObject(d), &stored); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored.Status.RuntimeInventory, []lab.OwnedRuntimeIdentity{old}) {
				t.Fatal("old Device UID/boot obligation was rewritten")
			}
		})
	}
}
func TestResidualConfiguredPlacementGroupHistorySurvivesRemovedObserverAndNode(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(map[bool]string{false: "observer-removed", true: "node-deleted"}[gone], func(t *testing.T) {
			ctx := context.Background()
			s := pruneScheme(t)
			_ = corev1.AddToScheme(s)
			_ = allocation.AddToScheme(s)
			worker, cp := residualPlacementNodes()
			old := lab.OwnedRuntimeIdentity{ScopeKind: "GroupScope", ScopeUID: "group", OwnerUID: "group", Namespace: "ns", OperationID: "old-stop", Revision: 1, Generation: 2, NodeName: cp.Name, NodeBootID: cp.Status.NodeInfo.BootID}
			g := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g", UID: "group", Generation: 3}, Spec: lab.LabGroupSpec{Lifecycle: &lab.GroupLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 2}}, Status: lab.LabGroupStatus{Namespace: "ns", ServiceRuntime: []lab.OwnedRuntimeIdentity{old}}}
			c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(g).WithObjects(worker, cp, g).Build()
			if gone {
				if err := c.Delete(ctx, cp); err != nil {
					t.Fatal(err)
				}
			}
			r := &LabGroupReconciler{Client: c, Reader: c, ServiceReleaseObserver: NativeGroupServiceReleaseObserver{Client: c}}
			if prepared, err := r.PrepareGroupRelease(ctx, g); err == nil || prepared {
				t.Fatal("old service scope was erased by current exclusion/absence")
			}
			var stored lab.LabGroup
			if err := c.Get(ctx, client.ObjectKeyFromObject(g), &stored); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored.Status.ServiceRuntime, []lab.OwnedRuntimeIdentity{old}) {
				t.Fatal("old Group UID/boot inventory changed on failure")
			}
		})
	}
}
