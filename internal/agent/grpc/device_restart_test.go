package grpc

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// A device whose container was OOM-killed and restarted in place looks healthy (Running) but reports why and when it
// restarted: the kubelet's last termination, with the restart count.
func TestDeviceRestartedInPlaceReportsOOMKilled(t *testing.T) {
	oomAt := time.Date(2026, 10, 26, 11, 30, 0, 0, time.UTC)
	pod := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "device", RestartCount: 2, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137, FinishedAt: metav1.NewTime(oomAt)}},
		}},
	}}
	reason, at := podLastRestart(pod)
	if reason != "OOMKilled" || !at.Equal(oomAt) {
		t.Fatalf("got %q %v", reason, at)
	}
	lab := &protobuf.Lab{Name: "l", Status: &protobuf.LabStatus{Devices: []*protobuf.LabDeviceStatus{{Name: "d"}}}}
	fillLabPodStatus(lab, map[usageKey]devicePodStatus{{lab: "l", device: "d"}: {phase: "Running", restartCount: podRestartCount(pod), lastRestartReason: reason, lastRestartAt: at}})
	d := lab.Status.Devices[0]
	if d.RestartCount != 2 || d.LastRestartReason != "OOMKilled" || d.LastRestartUnixMs != oomAt.UnixMilli() || d.PodPhase != "Running" {
		t.Fatalf("got %+v", d)
	}

	// Never restarted: nothing reported, even if a termination is recorded.
	pod.Status.ContainerStatuses[0].RestartCount = 0
	if reason, at := podLastRestart(pod); reason != "" || !at.IsZero() {
		t.Fatalf("no restart, got %q %v", reason, at)
	}
}
