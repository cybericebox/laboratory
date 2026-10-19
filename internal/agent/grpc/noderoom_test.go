package grpc

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	laboratorycontroller "github.com/cybericebox/laboratory/internal/controller/laboratory"
	"github.com/cybericebox/laboratory/internal/limits"
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
	// no device limit: the largest device is bounded by the virtual room only
	if got.MaxDeviceCpuMillicores != virtual.CPU {
		t.Fatalf("largest device %d", got.MaxDeviceCpuMillicores)
	}
	// the nodes that exist now, beside it
	if !got.HasCurrent || got.CurrentCpuMillicores != 2*node.CPU || got.CurrentMemoryBytes != 2*node.Memory {
		t.Fatalf("current = %v %d / %d", got.HasCurrent, got.CurrentCpuMillicores, got.CurrentMemoryBytes)
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
	if err != nil || got.CpuQuotaMillicores != 4000 || got.MemoryQuotaBytes != 8<<30 || got.MaxDeviceCpuMillicores != 4000 ||
		!got.HasCurrent || got.CurrentCpuMillicores != 4000 || got.CurrentMemoryBytes != 8<<30 {
		t.Fatalf("%+v %v", got, err)
	}
	h = tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("open", true, nil)}, roomTestNode("w1", "g"), roomTestNode("w2", "g"))
	h.SetMaximum(nodecap.Maximum{Budget: nodecap.Amount{CPU: 4000, Memory: 8 << 30}})
	if got, err = h.GetCapacity(asClient("open"), &protobuf.Empty{}); err != nil || got.CpuQuotaMillicores != 8000 {
		t.Fatalf("budget below the nodes: %+v %v", got, err)
	}
}

// A percentage quota is a share of the maximum: the nodes that exist plus what the budget still allows.
func TestPercentQuotaOfTheMaximum(t *testing.T) {
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("a", true, &laboratoryv1alpha1.TenantQuota{CPU: "50%"})}, roomTestNode("cp-1", ""))
	h.SetMaximum(nodecap.Maximum{Budget: nodecap.Amount{CPU: 12000, Memory: 24 << 30}})
	got, err := h.GetCapacity(asClient("a"), &protobuf.Empty{})
	if err != nil || got.CpuQuotaMillicores != 8000 {
		t.Fatalf("50%% of 4 + 12 CPU: %+v %v", got, err)
	}
}

// Scale from zero: with no lab node and a budget the largest device is the device limit, never 0/0 (the operator places it).
func TestMaxDeviceFromZeroIsTheDeviceLimit(t *testing.T) {
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("open", true, nil)})
	h.SetFeatures(Features{Limits: limits.Limits{DeviceMaxCPU: 2000, DeviceMaxMemory: 4 << 30}})
	h.SetMaximum(nodecap.Maximum{Budget: nodecap.Amount{CPU: 12000, Memory: 48 << 30}})
	got, err := h.GetCapacity(asClient("open"), &protobuf.Empty{})
	if err != nil || !got.HasMaxDevice || got.MaxDeviceCpuMillicores != 2000 || got.MaxDeviceMemoryBytes != 4<<30 {
		t.Fatalf("%+v %v", got, err)
	}
	// a real node larger than the limit still counts
	h = tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("open", true, nil)}, roomTestNode("cp-1", ""))
	h.SetFeatures(Features{Limits: limits.Limits{DeviceMaxCPU: 2000, DeviceMaxMemory: 4 << 30}})
	h.SetMaximum(nodecap.Maximum{Budget: nodecap.Amount{CPU: 12000, Memory: 48 << 30}})
	if got, err = h.GetCapacity(asClient("open"), &protobuf.Empty{}); err != nil || got.MaxDeviceCpuMillicores != 4000 || got.MaxDeviceMemoryBytes != 8<<30 {
		t.Fatalf("%+v %v", got, err)
	}
}

// The capacity the agent reports to a tenant without a quota is exactly the room the operator's scheduler starts a pass from:
// the same nodes, pods, reserves and budget give the same number (the real nodes plus the virtual room of the budget).
func TestCapacityEqualsOperatorRoomsWithBudget(t *testing.T) {
	sys := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "cilium", Namespace: "kube-system"}, Spec: corev1.PodSpec{NodeName: "w1",
		Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("300Mi")}}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	n1, n2 := roomTestNode("cp-1", ""), roomTestNode("w1", "m7i")
	reserve := nodecap.Reserve{Node: nodecap.Amount{CPU: 100, Memory: 256 << 20}, Percent: 5}
	packing := nodecap.PackingReserve{Percent: 10}
	for _, m := range []nodecap.Maximum{{}, {Budget: nodecap.Amount{CPU: 20000, Memory: 40 << 30}}} {
		h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("open", true, nil)}, n1, n2, sys)
		h.SetNodeReserve(reserve)
		h.SetPackingReserve(packing)
		h.SetMaximum(m)
		got, err := h.GetCapacity(asClient("open"), &protobuf.Empty{})
		if err != nil {
			t.Fatal(err)
		}
		want := laboratorycontroller.RoomForLabs([]corev1.Node{*n1, *n2}, []corev1.Pod{*sys}, nil, nil, reserve, packing, m)
		if got.CpuQuotaMillicores != want.CPU || got.MemoryQuotaBytes != want.Memory {
			t.Fatalf("budget %+v: agent cpu %d memory %d, operator %+v", m.Budget, got.CpuQuotaMillicores, got.MemoryQuotaBytes, want)
		}
	}
}
