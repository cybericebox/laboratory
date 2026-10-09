package laboratory

import (
	"context"
	"reflect"
	"testing"
	"time"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func runningAllocationFixture(t *testing.T) (*LabReconciler, *lab.Lab, *lab.Device, *corev1.Pod, *corev1.Node) {
	t.Helper()
	s := runtime.NewScheme()
	_ = lab.AddToScheme(s)
	_ = allocation.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	res := &lab.DeviceResources{CPULimit: "100m", MemoryLimit: "256Mi"}
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab", Generation: 2}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Running", OperationID: "initial", Revision: 1}, Devices: []lab.DeviceTemplate{{Name: "web", Type: lab.DeviceTypeContainer, Resources: res}}}, Status: lab.LabStatus{Lifecycle: &lab.LabLifecycleStatus{LabUID: "lab", OperationID: "initial", Revision: 1, ObservedGeneration: 2, ObservedState: "Starting"}}}
	d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "l-web", Namespace: "ns", UID: "device", Labels: map[string]string{names.LabelLab: "l"}, OwnerReferences: []metav1.OwnerReference{{Kind: "Lab", Name: "l", UID: l.UID}}}, Spec: lab.DeviceSpec{Name: "web", LabRef: "l", Type: lab.DeviceTypeContainer, Resources: res}, Status: lab.DeviceStatus{Ready: true, NodeName: "worker", PodName: "pod", State: &lab.DeviceStateStatus{Incarnation: 1}}}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", UID: "pod", OwnerReferences: []metav1.OwnerReference{{Kind: "Device", Name: d.Name, UID: d.UID}}}, Spec: corev1.PodSpec{NodeName: "worker", Containers: []corev1.Container{{Name: "web", Resources: deviceResources(d, DeviceDefaults{})}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "web", ContainerID: "containerd://main"}}}}
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{corev1.LabelOSStable: "linux", names.LabelNodeAgentReady: "true"}}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot", OperatingSystem: "linux"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	now := metav1.NewTime(time.Now().Add(-time.Second))
	id := lab.OwnedRuntimeIdentity{OwnerUID: "lab", ScopeUID: "device", Namespace: "ns", LabName: "l", OperationID: "initial", Revision: 1, Generation: 2, PodUID: "pod", NodeName: "worker", NodeBootID: "boot", ContainerIDs: []string{"main", "sandbox"}, CgroupPaths: []string{"/actual"}, AttachmentsComplete: true, Incarnation: 1, Requests: lab.ResourceAmounts{CPUMillicores: 100, MemoryBytes: 256 << 20}, Limits: lab.ResourceAmounts{CPUMillicores: 100, MemoryBytes: 256 << 20}}
	d.Status.RuntimeReports = []lab.OwnedRuntimeReport{{Identity: id, RuntimeState: "Allocated", ObservedAt: &now}}
	scope := lab.OwnedRuntimeIdentity{ScopeKind: "LabFabric", ScopeUID: "lab", OwnerUID: "lab", Namespace: "ns", LabName: "l", OperationID: "initial", Revision: 1, Generation: 2, NodeName: "worker", NodeBootID: "boot", AttachmentsComplete: true}
	l.Status.ScopeInventory = []lab.OwnedRuntimeIdentity{scope}
	l.Status.ScopeReports = []lab.OwnedRuntimeReport{{Identity: scope, RuntimeState: "Allocated", ObservedAt: &now}}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(l, d).WithObjects(l, d, p, n).Build()
	return &LabReconciler{Client: c, Reader: c, Recorder: record.NewFakeRecorder(20), RuntimeObservation: true}, l, d, p, n
}

func runningAllocation(t *testing.T, r *LabReconciler, l *lab.Lab, d *lab.Device) *lab.RuntimeAllocation {
	t.Helper()
	var ds []lab.Device
	if d != nil {
		ds = []lab.Device{*d}
	}
	a, err := r.runningLabAllocation(context.Background(), l, ds)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRunningAllocationIdentityAndFreshness(t *testing.T) {
	cases := map[string]func(*LabReconciler, *lab.Lab, *lab.Device, *corev1.Pod, *corev1.Node){
		"missing report": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			d.Status.RuntimeReports = nil
		},
		"stale": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			old := metav1.NewTime(time.Now().Add(-61 * time.Second))
			d.Status.RuntimeReports[0].ObservedAt = &old
		},
		"future": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			future := metav1.NewTime(time.Now().Add(time.Minute))
			d.Status.RuntimeReports[0].ObservedAt = &future
		},
		"owner": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			d.Status.RuntimeReports[0].Identity.OwnerUID = "foreign"
		},
		"scope": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			d.Status.RuntimeReports[0].Identity.ScopeUID = "foreign"
		},
		"pod uid": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			d.Status.RuntimeReports[0].Identity.PodUID = "old"
		},
		"boot": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			d.Status.RuntimeReports[0].Identity.NodeBootID = "old"
		},
		"epoch": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			d.Status.RuntimeReports[0].Identity.Epoch++
		},
		"incarnation": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			d.Status.RuntimeReports[0].Identity.Incarnation++
		},
		"generation": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			d.Status.RuntimeReports[0].Identity.Generation++
		},
		"operation": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			d.Status.RuntimeReports[0].Identity.OperationID = "old"
		},
		"revision": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			d.Status.RuntimeReports[0].Identity.Revision++
		},
		"containers": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			d.Status.RuntimeReports[0].Identity.ContainerIDs = []string{"old"}
		},
		"attachments": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			d.Status.RuntimeReports[0].Identity.AttachmentsComplete = false
		},
		"lost pod": func(r *LabReconciler, _ *lab.Lab, _ *lab.Device, p *corev1.Pod, _ *corev1.Node) {
			if err := r.Delete(context.Background(), p); err != nil {
				t.Fatal(err)
			}
		},
		"node replaced": func(r *LabReconciler, _ *lab.Lab, _ *lab.Device, _ *corev1.Pod, n *corev1.Node) {
			n.Status.NodeInfo.BootID = "replacement"
			if err := r.Status().Update(context.Background(), n); err != nil {
				t.Fatal(err)
			}
		},
		"missing whole scope": func(_ *LabReconciler, l *lab.Lab, _ *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			l.Status.ScopeReports = nil
		},
		"stale whole scope": func(_ *LabReconciler, l *lab.Lab, _ *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			old := metav1.NewTime(time.Now().Add(-61 * time.Second))
			l.Status.ScopeReports[0].ObservedAt = &old
		},
		"wrong scope boot": func(_ *LabReconciler, l *lab.Lab, _ *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			l.Status.ScopeReports[0].Identity.NodeBootID = "old"
		},
		"native off": func(r *LabReconciler, _ *lab.Lab, _ *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			r.RuntimeObservation = false
		},
		"device not ready": func(_ *LabReconciler, _ *lab.Lab, d *lab.Device, _ *corev1.Pod, _ *corev1.Node) {
			d.Status.Ready = false
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r, l, d, p, n := runningAllocationFixture(t)
			change(r, l, d, p, n)
			a := runningAllocation(t, r, l, d)
			if a.RuntimeState != "Unknown" || a.AllocatedRequests.CPUMillicores < 100 || a.AllocatedRequests.MemoryBytes < 256<<20 || a.ReleasedAt != nil {
				t.Fatalf("unbound evidence gave credit: %+v", a)
			}
		})
	}
}

func TestRunningAllocationInitialAndRestart(t *testing.T) {
	r, l, d, _, _ := runningAllocationFixture(t)
	a := runningAllocation(t, r, l, d)
	if a.RuntimeState != "Allocated" || a.AllocatedRequests.CPUMillicores != 100 || a.AllocatedRequests.MemoryBytes != 256<<20 || !reflect.DeepEqual(a.ObservedAt, d.Status.RuntimeReports[0].ObservedAt) {
		t.Fatalf("initial: %+v", a)
	}
	l.Status.Resources = &lab.RuntimeAllocation{RuntimeState: "Released", OperationID: "stop", Revision: 2, ReleasedAt: a.ObservedAt}
	l.Spec.Lifecycle.OperationID, l.Spec.Lifecycle.Revision = "restart", 3
	l.Status.Lifecycle.OperationID, l.Status.Lifecycle.Revision = "restart", 3
	if got := runningAllocation(t, r, l, d); got.RuntimeState != "Unknown" || got.ReleasedAt != nil || got.AllocatedRequests.CPUMillicores < 100 {
		t.Fatalf("old release carried into restart: %+v", got)
	}
	d.Status.RuntimeReports[0].Identity.OperationID, d.Status.RuntimeReports[0].Identity.Revision = "restart", 3
	l.Status.ScopeInventory[0].OperationID, l.Status.ScopeInventory[0].Revision = "restart", 3
	l.Status.ScopeReports[0].Identity = l.Status.ScopeInventory[0]
	if got := runningAllocation(t, r, l, d); got.RuntimeState != "Allocated" || got.ReleasedAt != nil || got.AllocatedRequests.CPUMillicores != 100 {
		t.Fatalf("fresh restart failed: %+v", got)
	}
}

func TestRunningAllocationHistoricalDebtAndNoDoubleCount(t *testing.T) {
	r, l, d, _, _ := runningAllocationFixture(t)
	id := d.Status.RuntimeReports[0].Identity
	d.Status.RuntimeInventory = []lab.OwnedRuntimeIdentity{id, id}
	if got := runningAllocation(t, r, l, d); got.AllocatedRequests.CPUMillicores != 100 || got.RuntimeState != "Allocated" {
		t.Fatalf("same physical inventory duplicated: %+v", got)
	}
	old := id
	old.PodUID, old.OperationID, old.Revision = "old-pod", "stop", 2
	old.Requests = lab.ResourceAmounts{CPUMillicores: 50, MemoryBytes: 128 << 20}
	d.Status.RuntimeInventory = append(d.Status.RuntimeInventory, old)
	if got := runningAllocation(t, r, l, d); got.RuntimeState != "Unknown" || got.AllocatedRequests.CPUMillicores != 150 || got.AllocatedRequests.MemoryBytes != 384<<20 {
		t.Fatalf("historical obligation lost: %+v", got)
	}
	release := waveReleased(old)
	d.Status.RuntimeReports = append(d.Status.RuntimeReports, release)
	if got := runningAllocation(t, r, l, d); got.RuntimeState != "Allocated" || got.AllocatedRequests.CPUMillicores != 100 {
		t.Fatalf("exact fresh release not retired: %+v", got)
	}
	stale := metav1.NewTime(time.Now().Add(-61 * time.Second))
	d.Status.RuntimeReports[1].ObservedAt = &stale
	if got := runningAllocation(t, r, l, d); got.RuntimeState != "Unknown" || got.AllocatedRequests.CPUMillicores != 150 {
		t.Fatalf("stale release credited: %+v", got)
	}
}

func TestRunningAllocationNeverMaterializedAndEmpty(t *testing.T) {
	r, l, d, p, _ := runningAllocationFixture(t)
	never := l.Status.ScopeInventory[0]
	never.ScopeKind, never.ScopeUID, never.AttachmentsComplete = "NeverMaterialized", string(d.UID), false
	l.Status.ScopeInventory = append(l.Status.ScopeInventory, never)
	if got := runningAllocation(t, r, l, d); got.RuntimeState != "Allocated" || got.AllocatedRequests.CPUMillicores != 100 {
		t.Fatalf("empty declaration blocks materialized device: %+v", got)
	}
	l.Status.ScopeInventory[1].Requests.CPUMillicores = 25
	if got := runningAllocation(t, r, l, d); got.RuntimeState != "Unknown" || got.AllocatedRequests.CPUMillicores != 125 {
		t.Fatalf("positive declaration erased: %+v", got)
	}
	l.Status.ScopeInventory = l.Status.ScopeInventory[:1]
	l.Spec.Devices, d.Status.RuntimeReports = nil, nil
	if err := r.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := runningAllocation(t, r, l, nil); got.RuntimeState != "Allocated" || got.AllocatedRequests != (lab.ResourceAmounts{}) {
		t.Fatalf("empty native fabric: %+v", got)
	}
	l.Spec.Devices = []lab.DeviceTemplate{{Name: "switch", Type: lab.DeviceTypeHub}}
	if got := runningAllocation(t, r, l, nil); got.RuntimeState != "Allocated" || got.AllocatedRequests != (lab.ResourceAmounts{}) {
		t.Fatalf("switch fabric: %+v", got)
	}
	l.Status.ScopeReports = nil
	if got := runningAllocation(t, r, l, nil); got.RuntimeState != "Unknown" {
		t.Fatalf("empty API absence minted allocation: %+v", got)
	}
}

func TestRunningAllocationDefaultsClampsAndStorage(t *testing.T) {
	r, l, d, p, _ := runningAllocationFixture(t)
	r.Defaults = DeviceDefaults{CPU: "250m", Memory: "512Mi", MaxCPU: "2", MaxMemory: "4Gi"}
	l.Spec.Devices[0].Resources = nil
	d.Status.RuntimeReports = nil
	if err := r.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	d.Status.State.SizeBytes = 73
	l.Status.Resources = &lab.RuntimeAllocation{SnapshotQuotaBytes: 90, AllocatedRequests: lab.ResourceAmounts{CPUMillicores: 300, MemoryBytes: 600 << 20}, StorageState: "Retained", PhysicalStorageBytesAvailable: true, PhysicalStorageBytes: 1}
	a := runningAllocation(t, r, l, d)
	if a.ConfiguredRequests.CPUMillicores != 250 || a.ConfiguredLimits.MemoryBytes != 512<<20 || a.AllocatedRequests.CPUMillicores != 300 || a.SnapshotQuotaBytes != 90 || a.StorageState != "Unknown" || a.PhysicalStorageBytesAvailable || a.Used != nil || a.UsageAvailable {
		t.Fatalf("defaults/storage/lost holds: %+v", a)
	}
	l.Spec.Devices[0].Resources = &lab.DeviceResources{CPULimit: "64", MemoryLimit: "100Gi"}
	a = runningAllocation(t, r, l, d)
	if a.ConfiguredRequests.CPUMillicores != 2000 || a.ConfiguredLimits.MemoryBytes != 4<<30 || a.AllocatedRequests.CPUMillicores != 2000 {
		t.Fatalf("clamps: %+v", a)
	}
}

func TestRunningAllocationActualPodAmountsAndDirtyNoop(t *testing.T) {
	r, l, d, p, _ := runningAllocationFixture(t)
	p.Spec.Containers[0].Resources = deviceResources(&lab.Device{Spec: lab.DeviceSpec{Resources: &lab.DeviceResources{CPULimit: "200m", MemoryLimit: "512Mi"}}}, DeviceDefaults{})
	if err := r.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	a := runningAllocation(t, r, l, d)
	if a.AllocatedRequests.CPUMillicores != 200 || a.ConfiguredRequests.MemoryBytes != 512<<20 {
		t.Fatalf("actual Pod quantities ignored: %+v", a)
	}
	if _, err := r.updateStatus(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(l), l); err != nil {
		t.Fatal(err)
	}
	before := l.DeepCopy()
	if _, err := r.updateStatus(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(l), l); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Status.Resources, l.Status.Resources) || before.ResourceVersion != l.ResourceVersion {
		t.Fatalf("unchanged native sample writes again: rv %s -> %s", before.ResourceVersion, l.ResourceVersion)
	}
}

func TestRunningAllocationEarlyRestartAndUnavailableNode(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable node", true: "early restart"}[restart], func(t *testing.T) {
			r, l, _, _, node := runningAllocationFixture(t)
			now := metav1.Now()
			l.Status.Resources = &lab.RuntimeAllocation{RuntimeState: "Released", OperationID: "stop", Revision: 2, ObservedAt: &now, ReleasedAt: &now}
			if restart {
				l.Spec.Lifecycle.OperationID, l.Spec.Lifecycle.Revision = "restart", 3
				if err := r.Update(context.Background(), l); err != nil {
					t.Fatal(err)
				}
			} else if err := r.Delete(context.Background(), node); err != nil {
				t.Fatal(err)
			}
			if err := r.Status().Update(context.Background(), l); err != nil {
				t.Fatal(err)
			}
			_, _, err := r.reconcileLifecycle(context.Background(), l)
			if restart && err != nil {
				t.Fatal(err)
			}
			if !restart && err == nil {
				t.Fatal("expected unavailable native placement error")
			}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(l), l); err != nil {
				t.Fatal(err)
			}
			if l.Status.Resources == nil || l.Status.Resources.RuntimeState != "Unknown" || l.Status.Resources.OperationID != l.Spec.Lifecycle.OperationID || l.Status.Resources.ReleasedAt != nil || l.Status.Resources.AllocatedRequests.CPUMillicores < 100 {
				t.Fatalf("early return carried release credit: %+v", l.Status.Resources)
			}
		})
	}
}

func TestRunningAllocationQueuedMissingDeviceAndCurrentHistoricalPod(t *testing.T) {
	r, l, d, p, _ := runningAllocationFixture(t)
	if err := r.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	d.Status.RuntimeReports = nil
	d.Status.PodName, d.Status.NodeName, d.Status.State = "", "", nil
	d.Status.Scheduling = &lab.PodSchedule{State: lab.PodQueued}
	if got := runningAllocation(t, r, l, d); got.RuntimeState != "Unknown" || got.AllocatedRequests.CPUMillicores != 100 {
		t.Fatalf("queued credit: %+v", got)
	}
	if got := runningAllocation(t, r, l, nil); got.RuntimeState != "Unknown" || got.AllocatedRequests.CPUMillicores != 100 {
		t.Fatalf("missing device credit: %+v", got)
	}
	r, l, d, _, _ = runningAllocationFixture(t)
	old := d.Status.RuntimeReports[0].Identity
	old.PodUID, old.ContainerIDs = "old-pod", []string{"old-container"}
	d.Status.RuntimeInventory = []lab.OwnedRuntimeIdentity{old}
	if got := runningAllocation(t, r, l, d); got.RuntimeState != "Unknown" || got.AllocatedRequests.CPUMillicores != 200 {
		t.Fatalf("second physical Pod treated as current: %+v", got)
	}
}

func TestRunningAllocationNonpersistentDeployment(t *testing.T) {
	r, l, d, p, _ := runningAllocationFixture(t)
	d.Status.State = nil
	d.Status.RuntimeReports[0].Identity.Incarnation = 0
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "deployment", Namespace: "ns", UID: "deployment", OwnerReferences: []metav1.OwnerReference{{Kind: "Device", Name: d.Name, UID: d.UID}}}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "replicaset", Namespace: "ns", UID: "replicaset", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: dep.Name, UID: dep.UID}}}}
	p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID}}
	for _, object := range []client.Object{dep, rs} {
		if err := r.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := runningAllocation(t, r, l, d); got.RuntimeState != "Allocated" || got.AllocatedRequests.CPUMillicores != 100 {
		t.Fatalf("ordinary Deployment permanently Unknown: %+v", got)
	}
	if err := r.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := r.updateStatus(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(l), l); err != nil {
		t.Fatal(err)
	}
	if l.Status.Resources.RuntimeState != "Allocated" || l.Status.Lifecycle.ObservedState != "Running" {
		t.Fatalf("ordinary Deployment status producer: %+v %+v", l.Status.Resources, l.Status.Lifecycle)
	}
	d.Status.RuntimeReports[0].Identity.Epoch = 1
	if got := runningAllocation(t, r, l, d); got.RuntimeState != "Unknown" {
		t.Fatalf("nonpersistent nonzero state tuple accepted: %+v", got)
	}
	d.Status.RuntimeReports[0].Identity.Epoch = 0
	d.Spec.State = &lab.DeviceStateSpec{Enabled: true}
	if got := runningAllocation(t, r, l, d); got.RuntimeState != "Unknown" {
		t.Fatalf("missing persistent state accepted as ordinary: %+v", got)
	}
}

func TestRunningAllocationActualInitAndOverhead(t *testing.T) {
	r, l, d, p, _ := runningAllocationFixture(t)
	p.Spec.InitContainers = []corev1.Container{{Name: "init", Resources: deviceResources(&lab.Device{Spec: lab.DeviceSpec{Resources: &lab.DeviceResources{CPULimit: "300m", MemoryLimit: "1Gi"}}}, DeviceDefaults{})}}
	p.Spec.Overhead = deviceResources(&lab.Device{Spec: lab.DeviceSpec{Resources: &lab.DeviceResources{CPULimit: "20m", MemoryLimit: "32Mi"}}}, DeviceDefaults{}).Requests
	if err := r.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := runningAllocation(t, r, l, d); got.AllocatedRequests.CPUMillicores != 320 || got.AllocatedRequests.MemoryBytes != (1<<30)+(32<<20) || got.ConfiguredLimits.MemoryBytes != (1<<30)+(32<<20) {
		t.Fatalf("actual init/overhead allocation ignored: %+v", got)
	}
}
