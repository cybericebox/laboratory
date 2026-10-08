package grpc

import (
	"context"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	csfake "github.com/cybericebox/laboratory/clientset/client/versioned/fake"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"google.golang.org/protobuf/reflect/protoreflect"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"testing"
)

func TestCurrentVPNBootProjectionUsesIndependentCurrentStartupRecord(t *testing.T) {
	ctx := context.Background()
	g := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g", UID: "group-uid"}, Status: lab.LabGroupStatus{Namespace: "ns"}}
	boot := lab.VPNRuntimeIdentity{BootID: "current-boot", PodName: "vpn-pod", PodUID: "pod-uid", ContainerID: "containerd://1"}
	report := &lab.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: "ns"}, Spec: lab.LabTrafficReportSpec{Kind: lab.LabTrafficSurfaceVPN, Instance: boot.PodName}, Status: lab.LabTrafficReportStatus{BootID: "old-counter-boot", CurrentVPNRuntime: &lab.VPNBootRecord{GroupUID: string(g.UID), VPNRuntimeIdentity: boot, PublishedAt: metav1.Now()}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: boot.PodName, Namespace: "ns", UID: "pod-uid", Labels: map[string]string{names.LabelComponent: names.ComponentVPN}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "vpn", ContainerID: boot.ContainerID, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	// Idle group: no LabVPN object exists, and neither policy nor traffic counter
	// epoch is permitted to supply the current process boot.
	h := &Handler{cs: csfake.NewSimpleClientset(report), k8s: k8sfake.NewSimpleClientset(pod)}
	out := &protobuf.LabGroupStatus{}
	h.projectCurrentVPNBoot(ctx, g, out)
	if !out.GetCurrentVpnBootAvailable() || out.GetCurrentVpnBootId() != boot.BootID || out.GetCurrentVpnBootObservedUnixMs() == 0 {
		t.Fatal("independent idle-group boot missing", out)
	}
	// Process restart in the SAME Pod/container: startup publication alone must
	// change the independent witness before a policy ACK from the old boot counts.
	report.Status.CurrentVPNRuntime.BootID = "same-container-new-process"
	if _, err := h.cs.LaboratoryV1alpha1().LabTrafficReports("ns").UpdateStatus(ctx, report, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.projectCurrentVPNBoot(ctx, g, out)
	if !out.GetCurrentVpnBootAvailable() || out.GetCurrentVpnBootId() != "same-container-new-process" {
		t.Fatal("direct projection retained cached process boot", out)
	}
	pod.Status.ContainerStatuses[0].RestartCount++
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://2"
	if _, err := h.k8s.CoreV1().Pods("ns").UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.projectCurrentVPNBoot(ctx, g, out)
	if out.GetCurrentVpnBootAvailable() || out.GetCurrentVpnBootId() != "" {
		t.Fatal("same-Pod container restart credited previous process boot", out)
	}
	pod.Status.ContainerStatuses[0].RestartCount--
	pod.Status.ContainerStatuses[0].ContainerID = boot.ContainerID
	_, _ = h.k8s.CoreV1().Pods("ns").UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	report.Status.CurrentVPNRuntime.GroupUID = "replacement-group"
	if _, err := h.cs.LaboratoryV1alpha1().LabTrafficReports("ns").UpdateStatus(ctx, report, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.projectCurrentVPNBoot(ctx, g, out)
	if out.GetCurrentVpnBootAvailable() {
		t.Fatal("stale group binding accepted")
	}
}
func TestCurrentVPNBootWireTagsRemainAdditive(t *testing.T) {
	fields := (&protobuf.LabGroupStatus{}).ProtoReflect().Descriptor().Fields()
	for name, want := range map[string]int{"phase": 1, "lifecycle": 9, "resources": 10, "current_vpn_boot_id": 11, "current_vpn_boot_available": 12, "current_vpn_boot_observed_unix_ms": 13} {
		field := fields.ByName(protoreflect.Name(name))
		if field == nil || int(field.Number()) != want {
			t.Fatal("wrong additive wire tag", name, field)
		}
	}
}
