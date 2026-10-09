package laboratory

import (
	"context"
	"testing"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/api/pool"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func waveReleased(id lab.OwnedRuntimeIdentity) lab.OwnedRuntimeReport {
	now := metav1.Now()
	return lab.OwnedRuntimeReport{Identity: id, RuntimeState: "Released", ObservedAt: &now, RuntimeAbsentAt: &now, CgroupAbsentAt: &now, AttachmentsAbsentAt: &now, ReleasedVNIs: append([]lab.OwnedVNI(nil), id.VNIBindings...)}
}
func TestLifecycleWaveLateACKThenThirdStopDeduplicatesPhysicalHold(t *testing.T) {
	id := lab.OwnedRuntimeIdentity{OwnerUID: "lab", OperationID: "stop1", Revision: 1, PodUID: "pod", NodeName: "node", NodeBootID: "boot", ContainerIDs: []string{"task"}, CgroupPaths: []string{"/owned"}, PortKeys: []string{"port"}, Requests: lab.ResourceAmounts{CPUMillicores: 100, MemoryBytes: 64 << 20}}
	next := *id.DeepCopy()
	next.OperationID = "stop3"
	next.Revision = 3
	held := aggregateRuntime([]lab.OwnedRuntimeIdentity{id, next}, nil, "lab", "stop3", 3)
	if held.RuntimeState != "Unknown" || held.AllocatedRequests != id.Requests {
		t.Fatalf("duplicate old/current physical reservation: %+v", held)
	}
	ack := waveReleased(id)
	held = aggregateRuntime([]lab.OwnedRuntimeIdentity{id, next}, []lab.OwnedRuntimeReport{ack}, "lab", "stop3", 3)
	if held.RuntimeState != "Unknown" || held.AllocatedRequests != id.Requests {
		t.Fatalf("old ACK released current obligation: %+v", held)
	}
	final := aggregateRuntime([]lab.OwnedRuntimeIdentity{id, next}, []lab.OwnedRuntimeReport{ack, waveReleased(next)}, "lab", "stop3", 3)
	if final.RuntimeState != "Released" || final.AllocatedRequests != (lab.ResourceAmounts{}) {
		t.Fatalf("late exact historical ACK poisoned stop3: %+v", final)
	}
}
func TestLifecycleWaveAuthoritativeEmptyScopesAndLostNode(t *testing.T) {
	for _, kind := range []string{"LabFabric", "GroupScope", "NeverMaterialized"} {
		t.Run(kind, func(t *testing.T) {
			id := lab.OwnedRuntimeIdentity{ScopeKind: kind, ScopeUID: "lab", OwnerUID: "lab", OperationID: "stop", Revision: 1, Generation: 3, NodeName: "actual-node", NodeBootID: "boot", AttachmentsComplete: true}
			if kind == "NeverMaterialized" {
				id.ScopeUID = "actual-device"
			}
			if !runtimeRowsReleased([]lab.OwnedRuntimeIdentity{id}, []lab.OwnedRuntimeReport{waveReleased(id)}, "lab", "stop", 1) {
				t.Fatal("actual empty native scope rejected")
			}
			unknown := waveReleased(id)
			unknown.Identity.NodeBootID = "lost-node-boot"
			if runtimeRowsReleased([]lab.OwnedRuntimeIdentity{id}, []lab.OwnedRuntimeReport{unknown}, "lab", "stop", 1) {
				t.Fatal("lost node credited release")
			}
		})
	}
	if runtimeRowsReleased(nil, nil, "lab", "stop", 1) {
		t.Fatal("absent API scope manufactured proof")
	}
	// Owned service processes may have zero platform legs after children stopped.
	id := lab.OwnedRuntimeIdentity{OwnerUID: "group", OperationID: "stop", Revision: 2, PodUID: "service", NodeName: "node", NodeBootID: "boot", ContainerIDs: []string{"sandbox", "service"}, CgroupPaths: []string{"/owned"}, AttachmentsComplete: true}
	if !runtimeRowsReleased([]lab.OwnedRuntimeIdentity{id}, []lab.OwnedRuntimeReport{waveReleased(id)}, "group", "stop", 2) {
		t.Fatal("zero-leg owned service rejected")
	}
}
func TestLifecycleWaveDeclaredScopeKeepsAllActualNodes(t *testing.T) {
	scheme := pruneScheme(t)
	_ = corev1.AddToScheme(scheme)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: map[string]string{names.LabelNodeAgentReady: "true"}}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot", OperatingSystem: "linux"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node).Build()
	scopes, err := declaredScopes(context.Background(), c, "lab", "ns", "l", "stop", 1, 3, "LabFabric")
	if err != nil || len(scopes) != 1 || scopes[0].PodUID != "" || len(scopes[0].ContainerIDs) != 0 {
		t.Fatalf("invalid actual scope: %+v %v", scopes, err)
	}
	delete(node.Labels, names.LabelNodeAgentReady)
	_ = c.Update(context.Background(), node)
	if _, err := declaredScopes(context.Background(), c, "lab", "ns", "l", "stop", 1, 3, "LabFabric"); err == nil {
		t.Fatal("lost ready label erased actual Linux placement node")
	}
	node.Labels = map[string]string{names.LabelNodeAgentReady: "true", corev1.LabelOSStable: "linux"}
	_ = c.Update(context.Background(), node)
	node.Status.Conditions[0].Status = corev1.ConditionFalse
	_ = c.Status().Update(context.Background(), node)
	if _, err := declaredScopes(context.Background(), c, "lab", "ns", "l", "stop", 1, 3, "LabFabric"); err == nil {
		t.Fatal("unavailable placement node erased obligation")
	}
}
func TestLifecycleWaveDeletionReturnsVNIOnlyAfterAllNativeNodeACKs(t *testing.T) {
	ctx := context.Background()
	scheme := pruneScheme(t)
	_ = allocation.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&allocation.Pool{}).Build()
	bitmap, free := pool.InitBitmap(names.VNIPoolSize, 0)
	seeded := &allocation.Pool{ObjectMeta: metav1.ObjectMeta{Name: names.VNIPoolPrefix + "-0", Namespace: names.SystemNamespace, UID: "pool-uid", Labels: map[string]string{pool.PoolGroupLabel: names.VNIPoolPrefix, pool.PoolStateLabel: pool.PoolStateEmpty, pool.LatestPoolLabel: "true"}}, Spec: allocation.PoolSpec{Size: names.VNIPoolSize}, Status: allocation.PoolStatus{BitMap: bitmap, Free: free}}
	if err := c.Create(ctx, seeded); err != nil {
		t.Fatal(err)
	}
	allocator := pool.NewRotatingAllocator(c, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize)
	vni, err := allocator.AllocateIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := pool.PinExistingIndex(ctx, c, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, vni, "original-connection")
	if err != nil {
		t.Fatal(err)
	}
	binding := lab.OwnedVNI{PoolUID: lease.PoolUID, LeaseGeneration: lease.Generation, Kind: "Connection", Namespace: "ns", Name: "gone", UID: "original-connection", VNI: vni, OwnerUID: "lab", OperationID: "stop", Revision: 1, Generation: 3}
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab"}}
	for _, node := range []string{"a", "b"} {
		l.Status.ScopeInventory = append(l.Status.ScopeInventory, lab.OwnedRuntimeIdentity{ScopeKind: "LabFabric", ScopeUID: "lab", OwnerUID: "lab", OperationID: "stop", Revision: 1, Generation: 3, NodeName: node, NodeBootID: "boot", VNIBindings: []lab.OwnedVNI{binding}})
	}
	r := &LabReconciler{Client: c, Reader: c, RuntimeObservation: true}
	var pools allocation.PoolList
	_ = c.List(ctx, &pools)
	before := pools.Items[0].Status.Free
	if err := r.releaseRecordedVNIs(ctx, l); err != nil {
		t.Fatal(err)
	}
	_ = c.List(ctx, &pools)
	if pools.Items[0].Status.Free != before {
		t.Fatal("API absence released lease before native ACK")
	}
	l.Status.ScopeReports = []lab.OwnedRuntimeReport{waveReleased(l.Status.ScopeInventory[0])}
	if err := r.releaseRecordedVNIs(ctx, l); err != nil {
		t.Fatal(err)
	}
	_ = c.List(ctx, &pools)
	if pools.Items[0].Status.Free != before {
		t.Fatal("one node ACK silently dropped lost-node obligation")
	}
	l.Status.ScopeReports = append(l.Status.ScopeReports, waveReleased(l.Status.ScopeInventory[1]))
	if err := r.releaseRecordedVNIs(ctx, l); err != nil {
		t.Fatal(err)
	}
	_ = c.List(ctx, &pools)
	if pools.Items[0].Status.Free != before+1 {
		t.Fatal("all native ACKs failed to retire original lease")
	}
	// Once recycled, the old ACK cannot free the new object's allocation.
	replacement := &lab.Connection{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: "other", UID: "replacement"}, Status: lab.ConnectionStatus{VNI: &vni}}
	if err := c.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err := r.releaseRecordedVNIs(ctx, l); err != nil {
		t.Fatal(err)
	}
	_ = c.List(ctx, &pools)
	if pools.Items[0].Status.Free != before+1 {
		t.Fatal("old ACK mutated replacement pool state")
	}
}
func TestLifecycleWaveStartPublishesOnlyCurrentNativeRunning(t *testing.T) {
	r, l, d, c := lifecycleFixture(t, "Skip")
	ctx := context.Background()
	r.Recorder = record.NewFakeRecorder(10)
	l.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Running", OperationID: "start2", Revision: 2}
	_ = c.Update(ctx, l)
	l.Status.Lifecycle = &lab.LabLifecycleStatus{LabUID: string(l.UID), OperationID: "start2", Revision: 2, ObservedGeneration: l.Generation, ObservedState: "Starting"}
	_ = c.Status().Update(ctx, l)
	d.Status.Ready = true
	d.Status.NodeName = "node"
	d.Status.RuntimeReports = []lab.OwnedRuntimeReport{{Identity: lab.OwnedRuntimeIdentity{OwnerUID: string(l.UID), OperationID: "stop1", Revision: 1, Generation: l.Generation, ScopeUID: string(d.UID), PodUID: "actual-pod", NodeName: "node", NodeBootID: "boot", ContainerIDs: []string{"task"}, CgroupPaths: []string{"/owned"}}, ObservedAt: ptrTime(metav1.Now()), RuntimeState: "Allocated"}}
	_ = c.Status().Update(ctx, d)
	if _, err := r.updateStatus(ctx, l); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(l), l)
	if l.Status.Lifecycle.ObservedState == "Running" {
		t.Fatal("old native operation granted new Running")
	}
	d.Status.RuntimeReports[0].Identity.OperationID = "start2"
	d.Status.RuntimeReports[0].Identity.Revision = 2
	_ = c.Status().Update(ctx, d)
	if _, err := r.updateStatus(ctx, l); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(l), l)
	if l.Status.Lifecycle.ObservedState != "Running" {
		t.Fatalf("current ready native operation failed Running: %+v", l.Status.Lifecycle)
	}
}
