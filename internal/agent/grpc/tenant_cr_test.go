package grpc

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/clientset/client/versioned/fake"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func newTenantTenant(name string, allowed bool, quota *laboratoryv1alpha1.TenantQuota) *laboratoryv1alpha1.Tenant {
	return &laboratoryv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       laboratoryv1alpha1.TenantSpec{Persistence: laboratoryv1alpha1.TenantPersistence{Allowed: allowed}, Quota: quota},
	}
}

func labPod(ns, name, tenant, cpu, mem string, phase corev1.PodPhase) *corev1.Pod {
	labels := map[string]string{names.LabelLab: "lab"}
	if tenant != "" {
		labels[names.LabelTenant] = tenant
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
		}}}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func tenantHandler(t *testing.T, tenants []*laboratoryv1alpha1.Tenant, k8sObjs ...runtime.Object) *Handler {
	t.Helper()
	var objs []runtime.Object
	for _, ten := range tenants {
		objs = append(objs, ten)
	}
	h := NewHandler(fake.NewSimpleClientset(objs...), k8sfake.NewSimpleClientset(k8sObjs...), nil)
	h.SetStatePersistence(true)
	return h
}

func TestAuthorizeKnowsTenantsOnly(t *testing.T) {
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("platform", false, nil)})
	if err := h.Authorize(asClient("platform")); err != nil {
		t.Fatalf("a tenant: %v", err)
	}
	if err := h.Authorize(asClient("intruder")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("an unknown CN: %v", err)
	}
	if err := h.Authorize(context.Background()); err != nil {
		t.Fatalf("no certificate is the default tenant: %v", err)
	}
	// The default tenant needs no object (the chart creates it, a bare cluster has none).
	if err := h.Authorize(asClient("default")); err != nil {
		t.Fatalf("default: %v", err)
	}
}

func TestTenantPolicyDecidesPersistence(t *testing.T) {
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("allowed", true, nil), newTenantTenant("locked", false, nil)})
	spec := []byte(`{"devices":[{"name":"web","type":"container","image":"x","persistence":{"enabled":true}}]}`)
	create := func(ctx context.Context) error {
		_, err := h.CreateLabs(ctx, &protobuf.CreateLabsRequest{
			Variants: []*protobuf.LabVariant{{VariantId: "v", SpecJson: spec}},
			Items:    []*protobuf.LabItem{{LabGroup: "g", Name: "c", VariantId: "v"}},
		})
		return err
	}
	if err := create(asClient("locked")); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "does not allow") {
		t.Fatalf("a tenant without persistence: %v", err)
	}
	// An allowed tenant passes validation (the group does not exist: NOT_FOUND per item, not a refusal).
	if err := create(asClient("allowed")); err != nil {
		t.Fatalf("an allowed tenant: %v", err)
	}
	// The platform switch is the ceiling.
	h.SetStatePersistence(false)
	if err := create(asClient("allowed")); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("platform off: %v", err)
	}
}

func TestCapacityIsTheCallersTenantView(t *testing.T) {
	quota := &laboratoryv1alpha1.TenantQuota{CPU: "2", Memory: "1Gi"}
	h := tenantHandler(t,
		[]*laboratoryv1alpha1.Tenant{newTenantTenant("a", true, quota), newTenantTenant("b", true, nil)},
		labPod("ns1", "a1", "a", "500m", "100Mi", corev1.PodRunning),
		labPod("ns1", "a2", "a", "250m", "50Mi", corev1.PodPending),
		labPod("ns1", "a3", "a", "4", "1Gi", corev1.PodSucceeded), // finished: reserves nothing
		labPod("ns2", "b1", "b", "3", "3Gi", corev1.PodRunning),
		labPod("ns3", "legacy", "", "100m", "10Mi", corev1.PodRunning), // before tenancy: default tenant
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "system", Namespace: "kube-system"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")}}}}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("64")}}},
	)
	got, err := h.GetCapacity(asClient("a"), &protobuf.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tenant != "a" || !got.HasCpuQuota || got.CpuQuotaMillicores != 2000 || !got.HasMemoryQuota || got.MemoryQuotaBytes != 1<<30 ||
		got.CpuReservedMillicores != 750 || got.MemoryReservedBytes != 150<<20 || got.CpuFreeMillicores != 1250 || got.MemoryFreeBytes != (1<<30)-(150<<20) || got.UsageAvailable {
		t.Fatalf("tenant a: %+v", got)
	}
	// Tenant b has no quota: no limit and no free numbers, and none of a's pods or the cluster's.
	b, _ := h.GetCapacity(asClient("b"), &protobuf.Empty{})
	if b.Tenant != "b" || b.HasCpuQuota || b.CpuReservedMillicores != 3000 || b.CpuFreeMillicores != 0 {
		t.Fatalf("tenant b: %+v", b)
	}
	// The default tenant owns the pod that predates tenancy, not the system pod.
	d, _ := h.GetCapacity(context.Background(), &protobuf.Empty{})
	if d.Tenant != "default" || d.CpuReservedMillicores != 100 {
		t.Fatalf("default tenant: %+v", d)
	}
}

func TestPercentQuotaFollowsTheLabNodes(t *testing.T) {
	node := func(name string, labels map[string]string, cpu string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse("10Gi")}}}
	}
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("a", true, &laboratoryv1alpha1.TenantQuota{CPU: "50%", Memory: "10%"})},
		node("lab1", map[string]string{"pool": "labs"}, "8"), node("lab2", map[string]string{"pool": "labs"}, "8"), node("other", nil, "32"))
	h.SetLabScheduling(map[string]string{"pool": "labs"}, nil)
	got, err := h.GetCapacity(asClient("a"), &protobuf.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if got.CpuQuotaMillicores != 8000 || got.MemoryQuotaBytes != 2<<30 {
		t.Fatalf("50%% of the 16 CPU on the lab nodes, 10%% of their 20Gi: %+v", got)
	}
}

func TestTenantStatusIsRefreshed(t *testing.T) {
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("a", true, nil)},
		labPod("ns1", "a1", "a", "500m", "100Mi", corev1.PodRunning), labPod("ns1", "a2", "a", "250m", "50Mi", corev1.PodRunning))
	h.refreshTenantStatus(context.Background())
	ten, err := h.cs.LaboratoryV1alpha1().Tenants().Get(context.Background(), "a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ten.Status.Reserved.CPU != "750m" || ten.Status.Reserved.Memory != "150Mi" || ten.Status.ObservedAt == nil || ten.Status.Used.CPU != "" {
		t.Fatalf("status %+v", ten.Status)
	}
}

func TestMonitoringCapacityIsPerTenant(t *testing.T) {
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{
		newTenantTenant("a", true, &laboratoryv1alpha1.TenantQuota{CPU: "1"}), newTenantTenant("b", true, nil)},
		labPod("ns1", "a1", "a", "500m", "1Mi", corev1.PodRunning))
	st, err := h.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.update.Capacity != nil {
		t.Fatal("the shared observation carries no cluster capacity")
	}
	for tenant, want := range map[string]int64{"a": 500, "b": 0} {
		c, err := h.tenantCapacity(asClient(tenant))
		if err != nil || c.CpuReservedMillicores != want || c.Tenant != tenant {
			t.Fatalf("%s: %+v %v", tenant, c, err)
		}
	}
}

// The Tenant resource on a real API server: resolution by CN, the status subresource the
// agent keeps fresh, and the validation of the name.
func TestTenantResourceOnARealAPIServer(t *testing.T) {
	h, k8s := newTestHandler(t)
	ctx := context.Background()
	tenants := h.cs.LaboratoryV1alpha1().Tenants()
	if _, err := tenants.Create(ctx, newTenantTenant("platform", true, &laboratoryv1alpha1.TenantQuota{CPU: "2", Memory: "1Gi"}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenants.Create(ctx, newTenantTenant("Not_A_Label", false, nil), metav1.CreateOptions{}); err == nil {
		t.Fatal("a tenant name is the certificate CN, a DNS-1123 label")
	}
	if err := h.Authorize(asClient("platform")); err != nil {
		t.Fatalf("known: %v", err)
	}
	if err := h.Authorize(asClient("stranger")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unknown: %v", err)
	}

	// Two pods of the tenant: the capacity and the status show their requests.
	mustNamespace(t, k8s, "lab-ns")
	for _, p := range []*corev1.Pod{labPod("lab-ns", "p1", "platform", "500m", "100Mi", corev1.PodRunning), labPod("lab-ns", "p2", "platform", "250m", "50Mi", corev1.PodRunning)} {
		p.Spec.Containers[0].Image = "x"
		if _, err := k8s.CoreV1().Pods("lab-ns").Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := h.GetCapacity(asClient("platform"), &protobuf.Empty{})
	if err != nil || c.CpuReservedMillicores != 750 || c.CpuFreeMillicores != 1250 || !c.HasMemoryQuota {
		t.Fatalf("capacity %+v %v", c, err)
	}
	h.refreshTenantStatus(ctx)
	got, err := tenants.Get(ctx, "platform", metav1.GetOptions{})
	if err != nil || got.Status.Reserved.CPU != "750m" || got.Status.ObservedAt == nil {
		t.Fatalf("status through the status subresource: %+v %v", got.Status, err)
	}
}

func TestCapacityReportsTheGroupOverhead(t *testing.T) {
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("a", true, nil)})
	h.SetGroupOverhead(grouppods.Config{VPNCPU: "100m", VPNMemory: "64Mi", GatewayCPU: "50m", GatewayMemory: "32Mi"}.Overhead())
	got, err := h.GetCapacity(asClient("a"), &protobuf.Empty{})
	if err != nil || got.GroupOverheadCpuMillicores != 150 || got.GroupOverheadMemoryBytes != 96<<20 {
		t.Fatalf("%+v %v", got, err)
	}
}
