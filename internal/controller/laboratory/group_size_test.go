package laboratory

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// A group created with an explicit size gets its pods Guaranteed at exactly that size; one without gets the chart's default. A later
// reconcile with another size in the spec changes nothing: the pods are never resized.
func TestGroupPodsAreCreatedAtTheSizeOfTheSpec(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	r := &LabGroupReconciler{Client: c, VPNBaseNetwork: "10.8.0.0/10", VPNImage: "lab:v1", GatewayImage: "lab:v1"}
	vpn := &laboratoryv1alpha1.GroupPodSize{CPUMillicores: 140, MemoryBytes: 200 << 20}
	gw := &laboratoryv1alpha1.GroupPodSize{CPUMillicores: 15, MemoryBytes: 24 << 20}
	if err := r.ensureVPNDeployment(ctx, "sized", false, vpn); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureGatewayDeployment(ctx, "sized", false, gw); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureVPNDeployment(ctx, "plain", false, nil); err != nil {
		t.Fatal(err)
	}
	res := func(ns, name string) corev1.ResourceRequirements {
		var d appsv1.Deployment
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &d); err != nil {
			t.Fatal(err)
		}
		return d.Spec.Template.Spec.Containers[0].Resources
	}
	for _, tc := range []struct {
		ns, name string
		cpu, mem int64
	}{{"sized", "vpn", 140, 200 << 20}, {"sized", "gateway", 15, 24 << 20}, {"plain", "vpn", 100, 320 << 20}} {
		got := res(tc.ns, tc.name)
		if got.Requests.Cpu().MilliValue() != tc.cpu || got.Requests.Memory().Value() != tc.mem {
			t.Errorf("%s/%s: requests %v, want %dm %d", tc.ns, tc.name, got.Requests, tc.cpu, tc.mem)
		}
		if got.Limits.Cpu().Cmp(*got.Requests.Cpu()) != 0 || got.Limits.Memory().Cmp(*got.Requests.Memory()) != 0 {
			t.Errorf("%s/%s: limits %v differ from requests %v: the pod must be Guaranteed", tc.ns, tc.name, got.Limits, got.Requests)
		}
	}
	// the spec's size changed (it is fixed by the CRD, but the operator must not act on it anyway)
	if err := r.ensureVPNDeployment(ctx, "sized", false, &laboratoryv1alpha1.GroupPodSize{CPUMillicores: 1, MemoryBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if got := res("sized", "vpn"); got.Requests.Cpu().MilliValue() != 140 {
		t.Errorf("a pod that exists is never resized, got %v", got.Requests)
	}
}

// The scheduler reserves what the group's pods really request.
func TestGroupPodNeedFollowsTheGroupSize(t *testing.T) {
	s := &Scheduler{}
	g := &laboratoryv1alpha1.LabGroup{Spec: laboratoryv1alpha1.LabGroupSpec{
		VPN:     laboratoryv1alpha1.LabGroupVPNSpec{Size: &laboratoryv1alpha1.GroupPodSize{CPUMillicores: 140, MemoryBytes: 200 << 20}},
		Gateway: laboratoryv1alpha1.LabGroupGatewaySpec{Size: &laboratoryv1alpha1.GroupPodSize{CPUMillicores: 15, MemoryBytes: 24 << 20}},
	}}
	if n := s.groupPodNeed("vpn", g); n.Cpu().MilliValue() != 140 || n.Memory().Value() != 200<<20 {
		t.Fatalf("vpn %v", n)
	}
	if n := s.groupPodNeed("gateway", g); n.Cpu().MilliValue() != 15 || n.Memory().Value() != 24<<20 {
		t.Fatalf("gateway %v", n)
	}
}
