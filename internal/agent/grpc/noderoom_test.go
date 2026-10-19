package grpc

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/nodecap"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// With no lab node and room for three, the capacity a tenant is told is three added nodes' room for labs (less the DaemonSets
// that would land there and the packing reserve), and the largest device is what one of them gives.
func TestCapacityReportsMaximum(t *testing.T) {
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "cilium", Namespace: "kube-system"}}
	ds.Spec.Template.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	ds.Spec.Template.Spec.Containers = []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("512Mi")}}}}
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("open", true, nil)}, ds)
	h.SetPackingReserve(nodecap.PackingReserve{Percent: 10})
	h.SetMaximum(nodecap.Maximum{Nodes: 3, Added: nodecap.Amount{CPU: 4000, Memory: 16 << 30}})
	got, err := h.GetCapacity(asClient("open"), &protobuf.Empty{})
	cpu, mem := int64(3800-380), int64(16<<30-512<<20)
	mem -= mem * 10 / 100
	if err != nil || !got.HasCpuQuota || got.CpuQuotaMillicores != 3*cpu || got.MemoryQuotaBytes != 3*mem {
		t.Fatalf("%+v %v, want 3 x %dm / %d", got, err, cpu, mem)
	}
	if !got.HasMaxDevice || got.MaxDeviceCpuMillicores != cpu || got.MaxDeviceMemoryBytes != mem {
		t.Fatalf("largest device %dm / %d, want %dm / %d", got.MaxDeviceCpuMillicores, got.MaxDeviceMemoryBytes, cpu, mem)
	}
}

// Without a maximum the capacity is that of the nodes that exist, as before.
func TestCapacityMaximumOffUnchanged(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{
		Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")},
		Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
	}}
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("open", true, nil)}, node)
	h.SetMaximum(nodecap.Maximum{})
	got, err := h.GetCapacity(asClient("open"), &protobuf.Empty{})
	if err != nil || got.CpuQuotaMillicores != 4000 || got.MemoryQuotaBytes != 8<<30 || got.MaxDeviceCpuMillicores != 4000 {
		t.Fatalf("%+v %v", got, err)
	}
	// a maximum at or below the nodes that exist adds nothing
	h = tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("open", true, nil)}, node)
	h.SetMaximum(nodecap.Maximum{Nodes: 1, Added: nodecap.Amount{CPU: 64000, Memory: 256 << 30}})
	if got, err = h.GetCapacity(asClient("open"), &protobuf.Empty{}); err != nil || got.CpuQuotaMillicores != 4000 || got.MaxDeviceCpuMillicores != 4000 {
		t.Fatalf("maximum = current: %+v %v", got, err)
	}
}
