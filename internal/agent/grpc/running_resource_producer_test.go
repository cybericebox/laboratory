package grpc

import (
	"context"
	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	versionedfake "github.com/cybericebox/laboratory/clientset/client/versioned/fake"
	controller "github.com/cybericebox/laboratory/internal/controller/laboratory"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"google.golang.org/protobuf/proto"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	cfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

// Full public Reconcile reaches updateStatus, persists the CRD status, and then
// feeds the actual List/Monitoring converters. Native inputs are explicit fixtures.
func TestResidualRunningProducerCRDListAndMonitoringInitialPublication(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	_ = allocation.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	specResources := &lab.DeviceResources{CPURequest: "100m", CPULimit: "100m", MemoryRequest: "256Mi", MemoryLimit: "256Mi"}
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab", Generation: 2}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Running", OperationID: "initial", Revision: 1}, Devices: []lab.DeviceTemplate{{Name: "web", Type: lab.DeviceTypeContainer, Image: "image", Resources: specResources}}}, Status: lab.LabStatus{Lifecycle: &lab.LabLifecycleStatus{LabUID: "lab", OperationID: "initial", Revision: 1, ObservedGeneration: 2, ObservedState: "Starting"}}}
	d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "l-web", Namespace: "ns", UID: "device", Labels: map[string]string{names.LabelLab: "l", names.LabelDevice: "web"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: lab.SchemeGroupVersion.String(), Kind: "Lab", Name: "l", UID: l.UID}}}, Spec: lab.DeviceSpec{Name: "web", LabRef: "l", Type: lab.DeviceTypeContainer, Image: "image", Code: "code", Resources: specResources}, Status: lab.DeviceStatus{Ready: true, NodeName: "worker", PodName: "web-pod", State: &lab.DeviceStateStatus{Incarnation: 1}}}
	rr := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-pod", Namespace: "ns", UID: "pod", Labels: map[string]string{names.LabelLab: "l", names.LabelDevice: "web"}, OwnerReferences: []metav1.OwnerReference{{Kind: "Device", Name: d.Name, UID: d.UID}}}, Spec: corev1.PodSpec{NodeName: "worker", Containers: []corev1.Container{{Name: "web", Image: "image", Resources: rr}}}}
	now := metav1.NewTime(time.Now().Add(-time.Second))
	id := lab.OwnedRuntimeIdentity{OwnerUID: "lab", ScopeUID: "device", Namespace: "ns", LabName: "l", OperationID: "initial", Revision: 1, Generation: 2, PodUID: "pod", NodeName: "worker", NodeBootID: "boot", ContainerIDs: []string{"actual-main", "actual-sandbox"}, CgroupPaths: []string{"/actual-owned"}, AttachmentsComplete: true, Incarnation: 1, Requests: lab.ResourceAmounts{CPUMillicores: 100, MemoryBytes: 256 << 20}, Limits: lab.ResourceAmounts{CPUMillicores: 100, MemoryBytes: 256 << 20}}
	d.Status.RuntimeReports = []lab.OwnedRuntimeReport{{Identity: id, RuntimeState: "Allocated", ObservedAt: &now}}
	scope := lab.OwnedRuntimeIdentity{ScopeKind: "LabFabric", OwnerUID: "lab", ScopeUID: "lab", Namespace: "ns", LabName: "l", OperationID: "initial", Revision: 1, Generation: 2, NodeName: "worker", NodeBootID: "boot"}
	l.Status.ScopeInventory = []lab.OwnedRuntimeIdentity{scope}
	l.Status.ScopeReports = []lab.OwnedRuntimeReport{{Identity: scope, RuntimeState: "Allocated", ObservedAt: &now}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{corev1.LabelOSStable: "linux", names.LabelNodeAgentReady: "true"}}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot", OperatingSystem: "linux"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	c := cfake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(l, d).WithObjects(l, d, pod, node).Build()
	r := &controller.LabReconciler{Client: c, Reader: c, Scheme: scheme, RuntimeObservation: true, Recorder: record.NewFakeRecorder(20)}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(l)}); err != nil {
		t.Fatal("producer Reconcile:", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
		t.Fatal(err)
	}
	if l.Status.Lifecycle == nil || l.Status.Lifecycle.ObservedState != "Running" {
		t.Fatalf("fixture did not reach actual current Running producer: %+v", l.Status.Lifecycle)
	}
	group := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "group", UID: "group"}, Status: lab.LabGroupStatus{Namespace: "ns"}}
	h := NewHandler(versionedfake.NewSimpleClientset(group, l), kfake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}}), nil)
	listed, err := h.ListLabs(ctx, &protobuf.ListRequest{LabGroup: "group"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 1 {
		t.Fatal("List did not return produced CRD")
	}
	projected := listed.Items[0].Status.Resources
	monitored := labMonitoringToProto(l, "group")
	if !proto.Equal(projected, monitored.Status.Resources) {
		t.Fatal("List/Monitoring resource projections diverged")
	}
	if projected.GetRuntimeState() != "Allocated" || projected.GetAllocatedRequests().GetCpuMillicores() != 100 || projected.GetAllocatedRequests().GetMemoryBytes() != 256<<20 || projected.GetObservedUnixMs() == 0 {
		t.Fatalf("current Running CRD failed strict initial publication allocation shape: %v", projected)
	}
}
