package grpc

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/nodecap"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func roomTestNode(name, group string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}, Status: corev1.NodeStatus{
		Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")},
		Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
	}}
	if group != "" {
		n.Labels[nodecap.LabelNodeGroup] = group
	}
	return n
}

// The capacity a tenant is told is the maximum: the fixed nodes' room plus the larger of the autoscaled nodes' room and the
// budget (net of the packing reserve); the largest device stays that of the nodes that exist.
func TestCapacityReportsMaximum(t *testing.T) {
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("open", true, nil)}, roomTestNode("cp-1", ""), roomTestNode("w1", "m7i"))
	h.SetPackingReserve(nodecap.PackingReserve{Percent: 10})
	h.SetMaximum(nodecap.Maximum{Budget: nodecap.Amount{CPU: 20000, Memory: 40 << 30}})
	got, err := h.GetCapacity(asClient("open"), &protobuf.Empty{})
	node := nodecap.Amount{CPU: 3600, Memory: 8<<30 - 8<<30*10/100}
	virtual := nodecap.Amount{CPU: (20000 - 4000) * 9 / 10, Memory: 32<<30 - 32<<30*10/100}
	if err != nil || got.CpuQuotaMillicores != 2*node.CPU+virtual.CPU || got.MemoryQuotaBytes != 2*node.Memory+virtual.Memory {
		t.Fatalf("%+v %v", got, err)
	}
	if got.MaxDeviceCpuMillicores != 3600 {
		t.Fatalf("largest device %d", got.MaxDeviceCpuMillicores)
	}
	// scale from zero: no lab node at all, the budget alone
	h = tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("open", true, nil)})
	h.SetMaximum(nodecap.Maximum{Budget: nodecap.Amount{CPU: 8000, Memory: 16 << 30}})
	if got, err = h.GetCapacity(asClient("open"), &protobuf.Empty{}); err != nil || !got.HasCpuQuota || got.CpuQuotaMillicores != 8000 {
		t.Fatalf("from zero: %+v %v", got, err)
	}
}

// Without a budget, or with autoscaled nodes beyond it, the capacity is that of the nodes that exist.
func TestCapacityMaximumOffUnchanged(t *testing.T) {
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("open", true, nil)}, roomTestNode("n1", ""))
	h.SetMaximum(nodecap.Maximum{})
	got, err := h.GetCapacity(asClient("open"), &protobuf.Empty{})
	if err != nil || got.CpuQuotaMillicores != 4000 || got.MemoryQuotaBytes != 8<<30 || got.MaxDeviceCpuMillicores != 4000 {
		t.Fatalf("%+v %v", got, err)
	}
	h = tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("open", true, nil)}, roomTestNode("w1", "g"), roomTestNode("w2", "g"))
	h.SetMaximum(nodecap.Maximum{Budget: nodecap.Amount{CPU: 4000, Memory: 8 << 30}})
	if got, err = h.GetCapacity(asClient("open"), &protobuf.Empty{}); err != nil || got.CpuQuotaMillicores != 8000 {
		t.Fatalf("budget below the nodes: %+v %v", got, err)
	}
}
