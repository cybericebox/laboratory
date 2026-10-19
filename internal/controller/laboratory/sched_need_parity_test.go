package laboratory

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/limits"
)

// The agent refuses a lab or group that does not fit its reservation with limits.LabNeed and grouppods.Config.Need; the
// scheduler counts the same objects pod by pod. Both must give the same need, or the agent's early refusal and the
// operator's ledger disagree.
func TestLabNeedMatchesOperator(t *testing.T) {
	defaults := DeviceDefaults{CPU: "100m", Memory: "128Mi", MaxCPU: "1", MaxMemory: "2Gi"}
	s := &Scheduler{Defaults: defaults, GroupPods: grouppods.Config{VPNCPU: "50m", VPNMemory: "64Mi", GatewayCPU: "10m", GatewayMemory: "32Mi"}}
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns"}, Spec: laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{
		{Name: "plain", Type: laboratoryv1alpha1.DeviceTypeContainer},
		{Name: "sized", Type: laboratoryv1alpha1.DeviceTypeContainer, Resources: &laboratoryv1alpha1.DeviceResources{CPURequest: "200m", CPULimit: "300m", MemoryRequest: "256Mi"}},
		{Name: "big", Type: laboratoryv1alpha1.DeviceTypeContainer, Resources: &laboratoryv1alpha1.DeviceResources{CPULimit: "4", MemoryLimit: "8Gi"}}, // clamped
		{Name: "hub", Type: laboratoryv1alpha1.DeviceTypeHub},
	}}}
	var op amount
	for _, d := range lab.Spec.Devices {
		if p := s.devicePod(&clusterView{}, lab, d, time.Now()); p != nil {
			op = op.add(p.need)
		}
	}
	cpu, mem := limits.LabNeed(&lab.Spec, defaults)
	if op.cpu != cpu || op.mem != mem || cpu != 100+300+1000 {
		t.Fatalf("lab: operator %+v, agent cpu %d memory %d", op, cpu, mem)
	}

	for _, g := range []*laboratoryv1alpha1.LabGroup{
		{},
		{Spec: laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Disabled: true}}},
		{Spec: laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Size: &laboratoryv1alpha1.GroupPodSize{CPUMillicores: 300, MemoryBytes: 1 << 30}}}},
	} {
		var gop amount
		for _, name := range groupPodNames(g) {
			n := s.groupPodNeed(name, g)
			gop = gop.add(amount{cpu: n.Cpu().MilliValue(), mem: n.Memory().Value()})
		}
		if need := s.GroupPods.Need(g); need.CPU != gop.cpu || need.Memory != gop.mem {
			t.Fatalf("group %+v: operator %+v, agent %+v", g.Spec, gop, need)
		}
	}
}
