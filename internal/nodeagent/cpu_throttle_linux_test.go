//go:build linux

package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func writeCPUStat(t *testing.T, dir string, periods, throttled int) {
	t.Helper()
	text := "usage_usec 1\nnr_periods " + strconv.Itoa(periods) + "\nnr_throttled " + strconv.Itoa(throttled) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A device container throttled in more than half of its periods gets status.cpuThrottle; when it stops, the field goes away.
func TestCPUThrottleMonitorPublishesAndClears(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "kubepods.slice", "kubepods-pod1.slice", "cri-containerd-abc123.scope")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := lab.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	device := &lab.Device{
		ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns"},
		Spec:       lab.DeviceSpec{Name: "web", LabRef: "l"},
		Status:     lab.DeviceStatus{NodeName: "node", PodName: "pod"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "web", ContainerID: "containerd://abc123", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}},
	}
	other := &lab.Device{ // a device of another node is none of this agent's business
		ObjectMeta: metav1.ObjectMeta{Name: "o", Namespace: "ns"},
		Spec:       lab.DeviceSpec{Name: "db", LabRef: "l"},
		Status:     lab.DeviceStatus{NodeName: "elsewhere", PodName: "pod2"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(device, pod, other).WithStatusSubresource(device, other).Build()
	clock := time.Unix(1000, 0)
	m := &CPUThrottleMonitor{Client: c, NodeName: "node", CgroupRoot: root, now: func() time.Time { return clock }}
	get := func() *lab.DeviceCPUThrottle {
		var d lab.Device
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(device), &d); err != nil {
			t.Fatal(err)
		}
		return d.Status.CPUThrottle
	}
	step := func(periods, throttled int) {
		clock = clock.Add(30 * time.Second)
		writeCPUStat(t, dir, periods, throttled)
		m.Check(context.Background())
	}

	step(1000, 0)
	if get() != nil {
		t.Fatal("a first sample reports nothing")
	}
	step(1100, 80) // 80 of 100 periods
	if got := get(); got == nil || got.Percent != 80 || got.WindowSeconds != 30 {
		t.Fatalf("throttled device: %+v", got)
	}
	first := get().ObservedAt
	step(1200, 160) // still 80%: nothing to write
	if got := get(); got == nil || !got.ObservedAt.Equal(&first) {
		t.Fatalf("an unchanged value was written again: %+v", got)
	}
	for i := 1; i <= 12; i++ { // six idle-of-throttling minutes: the window moves past it
		step(1200+i*100, 160)
	}
	if got := get(); got != nil {
		t.Fatalf("throttling is over: %+v", got)
	}
}

func TestNextCPUThrottle(t *testing.T) {
	now := metav1.Now()
	if next, change := nextCPUThrottle(nil, 0, 0, true, now); next != nil || change {
		t.Fatal("unthrottled stays absent")
	}
	if next, change := nextCPUThrottle(&lab.DeviceCPUThrottle{Percent: 70}, 0, 0, false, now); next != nil || !change {
		t.Fatal("an idle device is cleared")
	}
	if next, change := nextCPUThrottle(nil, 60, 90*time.Second, true, now); !change || next.Percent != 60 || next.WindowSeconds != 90 {
		t.Fatalf("%+v %v", next, change)
	}
}
