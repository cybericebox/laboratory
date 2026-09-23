package grpc

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func podMetrics(ns, name, lab, device string, containers ...corev1.ResourceList) *metricsv1beta1.PodMetrics {
	pm := &metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    map[string]string{},
		},
	}
	if lab != "" {
		pm.Labels[names.LabelLab] = lab
	}
	if device != "" {
		pm.Labels[names.LabelDevice] = device
	}
	for i, u := range containers {
		pm.Containers = append(pm.Containers, metricsv1beta1.ContainerMetrics{
			Name:  string(rune('a' + i)),
			Usage: u,
		})
	}
	return pm
}

func rl(cpu, mem string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
}

func TestNamespaceUsage(t *testing.T) {
	items := []metricsv1beta1.PodMetrics{
		// attacker: two containers → summed (100m+50m=150m, 64Mi+16Mi=80Mi)
		*podMetrics("team-alpha", "attacker-pod", "ctf1", "attacker", rl("100m", "64Mi"), rl("50m", "16Mi")),
		// victim: single container
		*podMetrics("team-alpha", "victim-pod", "ctf1", "victim", rl("200m", "128Mi")),
		// unlabeled pod (e.g. the VPN server) → ignored
		*podMetrics("team-alpha", "infra-pod", "", "", rl("10m", "8Mi")),
	}
	// The metrics fake clientset does not surface objects seeded via
	// NewSimpleClientset through List, so install a list reactor directly.
	fake := metricsfake.NewSimpleClientset()
	fake.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &metricsv1beta1.PodMetricsList{Items: items}, nil
	})
	h := &Handler{metrics: fake}

	usage := h.namespaceUsage(context.Background(), "team-alpha")
	if usage == nil {
		t.Fatalf("expected usage map, got nil")
	}
	if len(usage) != 2 {
		t.Fatalf("expected 2 devices, got %d: %+v", len(usage), usage)
	}

	att := usage[usageKey{lab: "ctf1", device: "attacker"}]
	if att.cpuMillicores != 150 {
		t.Errorf("attacker cpu = %d, want 150", att.cpuMillicores)
	}
	if att.memoryBytes != 80*1024*1024 {
		t.Errorf("attacker mem = %d, want %d", att.memoryBytes, 80*1024*1024)
	}
	vic := usage[usageKey{lab: "ctf1", device: "victim"}]
	if vic.cpuMillicores != 200 || vic.memoryBytes != 128*1024*1024 {
		t.Errorf("victim usage wrong: %+v", vic)
	}
}

// On a recreation overlap two pods share the (lab, device) labels; usage must
// reflect the last one, not the sum of both.
func TestNamespaceUsageOverlapTakesLast(t *testing.T) {
	items := []metricsv1beta1.PodMetrics{
		*podMetrics("team-alpha", "attacker-old", "ctf1", "attacker", rl("100m", "64Mi")),
		*podMetrics("team-alpha", "attacker-new", "ctf1", "attacker", rl("300m", "256Mi")),
	}
	fake := metricsfake.NewSimpleClientset()
	fake.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &metricsv1beta1.PodMetricsList{Items: items}, nil
	})
	h := &Handler{metrics: fake}

	usage := h.namespaceUsage(context.Background(), "team-alpha")
	att := usage[usageKey{lab: "ctf1", device: "attacker"}]
	if att.cpuMillicores != 300 || att.memoryBytes != 256*1024*1024 {
		t.Errorf("overlap usage should be the last pod (300m/256Mi), got %+v", att)
	}
}

// A nil metrics client (metrics-server absent) must not error — usage is simply
// unavailable.
func TestNamespaceUsageNilMetrics(t *testing.T) {
	h := &Handler{metrics: nil}
	if usage := h.namespaceUsage(context.Background(), "team-alpha"); usage != nil {
		t.Errorf("expected nil usage without metrics client, got %+v", usage)
	}
}

func TestFillLabUsage(t *testing.T) {
	lab := &protobuf.Lab{
		Name: "ctf1",
		Status: &protobuf.LabStatus{
			Devices: []*protobuf.LabDeviceStatus{
				{Name: "attacker"},
				{Name: "victim"},
			},
		},
	}
	usage := map[usageKey]deviceUsage{
		{lab: "ctf1", device: "attacker"}: {cpuMillicores: 150, memoryBytes: 1024},
	}
	fillLabUsage(lab, usage)

	if lab.Status.Devices[0].CpuMillicores != 150 || lab.Status.Devices[0].MemoryBytes != 1024 {
		t.Errorf("attacker usage not filled: %+v", lab.Status.Devices[0])
	}
	if !lab.Status.Devices[0].UsageAvailable || !lab.Status.Devices[1].UsageAvailable {
		t.Errorf("metrics availability should be set for every device: %+v", lab.Status.Devices)
	}
	// victim had no metrics entry → stays zero
	if lab.Status.Devices[1].CpuMillicores != 0 || lab.Status.Devices[1].MemoryBytes != 0 {
		t.Errorf("victim should be zero: %+v", lab.Status.Devices[1])
	}
}

func TestFillLabUsageNil(t *testing.T) {
	lab := &protobuf.Lab{
		Name:   "ctf1",
		Status: &protobuf.LabStatus{Devices: []*protobuf.LabDeviceStatus{{Name: "attacker"}}},
	}
	fillLabUsage(lab, nil) // must not panic
	if lab.Status.Devices[0].CpuMillicores != 0 {
		t.Errorf("expected zero usage with nil map")
	}
	if lab.Status.Devices[0].UsageAvailable {
		t.Errorf("usage must be marked unavailable without metrics API")
	}
}
