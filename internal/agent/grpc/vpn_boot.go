package grpc

import (
	"context"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The startup record and current Pod are direct reads: an informer retaining an
// older same-Pod boot is not a current acknowledgement. Unknown never falls back
// to policy.status.vpn_boot_id or traffic.status.boot_id.
func (h *Handler) projectCurrentVPNBoot(ctx context.Context, g *lab.LabGroup, out *protobuf.LabGroupStatus) {
	attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx = attempt
	out.CurrentVpnBootId = ""
	out.CurrentVpnBootAvailable = false
	out.CurrentVpnBootObservedUnixMs = 0
	if h.cs == nil || h.k8s == nil || g.UID == "" || g.Status.Namespace == "" || g.Spec.VPN.Disabled || !g.DeletionTimestamp.IsZero() {
		return
	}
	report, err := h.cs.LaboratoryV1alpha1().LabTrafficReports(g.Status.Namespace).Get(ctx, flowacct.ReportName, metav1.GetOptions{})
	if err != nil {
		return
	}
	boot := report.Status.CurrentVPNRuntime
	if !report.DeletionTimestamp.IsZero() || report.Spec.Kind != lab.LabTrafficSurfaceVPN || boot == nil || boot.GroupUID != string(g.UID) || boot.BootID == "" || boot.PodUID == "" || boot.ContainerID == "" || boot.PublishedAt.IsZero() || report.Spec.Instance != boot.PodName {
		return
	}
	p, err := h.k8s.CoreV1().Pods(g.Status.Namespace).Get(ctx, boot.PodName, metav1.GetOptions{})
	if err != nil {
		return
	}
	if !p.DeletionTimestamp.IsZero() || p.Status.Phase != corev1.PodRunning || p.Labels[names.LabelComponent] != names.ComponentVPN || string(p.UID) != boot.PodUID {
		return
	}
	for _, s := range p.Status.ContainerStatuses {
		if s.Name == names.ComponentVPN && s.State.Running != nil && s.ContainerID == boot.ContainerID && s.RestartCount == boot.RestartCount {
			out.CurrentVpnBootId = boot.BootID
			out.CurrentVpnBootAvailable = true
			out.CurrentVpnBootObservedUnixMs = time.Now().UnixMilli()
			return
		}
	}
}
func (h *Handler) currentLabGroupProto(ctx context.Context, g *lab.LabGroup) *protobuf.LabGroup {
	out := labGroupToProto(g)
	h.projectCurrentVPNBoot(ctx, g, out.Status)
	return out
}
