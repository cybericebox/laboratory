package grpc

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// bytesOf parses a quantity string to its byte value. resource.Quantity.Value
// is a pointer method, so the parsed value must be addressable (a local var).
func bytesOf(s string) int64 {
	q := resource.MustParse(s)
	return q.Value()
}

func capNode(name, cpu, mem string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			},
		},
	}
}

func capPod(name, nodeName string, phase corev1.PodPhase, reqs ...corev1.ResourceList) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.PodSpec{NodeName: nodeName},
		Status:     corev1.PodStatus{Phase: phase},
	}
	for _, r := range reqs {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{
			Resources: corev1.ResourceRequirements{Requests: r},
		})
	}
	return p
}

func TestComputeCapacity(t *testing.T) {
	nodes := []corev1.Node{
		capNode("n1", "2", "4Gi"),
		capNode("n2", "1", "2Gi"),
	}
	pods := []corev1.Pod{
		// n1: 500m/512Mi + 250m/256Mi = 750m/768Mi
		capPod("p1", "n1", corev1.PodRunning, rl("500m", "512Mi"), rl("250m", "256Mi")),
		// n2: 300m/128Mi
		capPod("p2", "n2", corev1.PodRunning, rl("300m", "128Mi")),
		// unscheduled → ignored
		capPod("p3", "", corev1.PodPending, rl("999", "9Gi")),
		// terminated → ignored
		capPod("p4", "n1", corev1.PodSucceeded, rl("999", "9Gi")),
	}

	resp := computeCapacity(nodes, pods)

	if len(resp.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(resp.Nodes))
	}
	byName := map[string]int{resp.Nodes[0].Name: 0, resp.Nodes[1].Name: 1}
	n1 := resp.Nodes[byName["n1"]]
	if n1.AllocatableCpuMillicores != 2000 || n1.AllocatableMemoryBytes != bytesOf("4Gi") {
		t.Errorf("n1 allocatable wrong: %+v", n1)
	}
	if n1.RequestedCpuMillicores != 750 || n1.RequestedMemoryBytes != bytesOf("768Mi") {
		t.Errorf("n1 requested wrong: %+v", n1)
	}
	n2 := resp.Nodes[byName["n2"]]
	if n2.RequestedCpuMillicores != 300 || n2.RequestedMemoryBytes != bytesOf("128Mi") {
		t.Errorf("n2 requested wrong: %+v", n2)
	}

	// cluster totals
	if resp.AllocatableCpuMillicores != 3000 {
		t.Errorf("total alloc cpu = %d, want 3000", resp.AllocatableCpuMillicores)
	}
	if resp.AllocatableMemoryBytes != bytesOf("6Gi") {
		t.Errorf("total alloc mem = %d, want %d", resp.AllocatableMemoryBytes, bytesOf("6Gi"))
	}
	if resp.RequestedCpuMillicores != 1050 {
		t.Errorf("total req cpu = %d, want 1050", resp.RequestedCpuMillicores)
	}
	if resp.RequestedMemoryBytes != bytesOf("896Mi") {
		t.Errorf("total req mem = %d, want %d", resp.RequestedMemoryBytes, bytesOf("896Mi"))
	}
}
