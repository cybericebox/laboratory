package grpc

import (
	"context"
	"reflect"
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
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodecap"
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
	// The default tenant is a tenant like any other: with a certificate it needs its Tenant object (the chart creates it), so a
	// leaked certificate of it can be revoked by enrolling it again.
	if err := h.Authorize(asClient("default")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("default without a Tenant object: %v", err)
	}
	h = tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("default", false, nil)})
	if err := h.Authorize(asClient("default")); err != nil {
		t.Fatalf("default with its object: %v", err)
	}
	// A certificate with no common name belongs to nobody.
	if err := h.Authorize(asClient("")); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("empty CN: %v", err)
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
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("64"), corev1.ResourceMemory: resource.MustParse("128Gi")}}},
	)
	got, err := h.GetCapacity(asClient("a"), &protobuf.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tenant != "a" || !got.HasCpuQuota || got.CpuQuotaMillicores != 2000 || !got.HasMemoryQuota || got.MemoryQuotaBytes != 1<<30 ||
		got.CpuReservedMillicores != 750 || got.MemoryReservedBytes != 150<<20 || got.CpuFreeMillicores != 1250 || got.MemoryFreeBytes != (1<<30)-(150<<20) || got.UsageAvailable {
		t.Fatalf("tenant a: %+v", got)
	}
	// Tenant b has no quota: it is told the real room of the lab nodes (64 CPU), and none of a's pods or the cluster's.
	b, _ := h.GetCapacity(asClient("b"), &protobuf.Empty{})
	if b.Tenant != "b" || !b.HasCpuQuota || b.CpuQuotaMillicores != 64000 || b.CpuReservedMillicores != 3000 || b.CpuFreeMillicores != 61000 {
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

// The capacity carries one number about the cluster layout: the largest device it can place (per resource, the largest
// allocatable of a ready lab node net of the platform reserve). No node list, no names, no per-node free room; not limited by
// the tenant quota.
func TestCapacityReportsTheLargestPlaceableDeviceOnly(t *testing.T) {
	node := func(name string, ready bool, cpu, mem string) *corev1.Node {
		labels := map[string]string{}
		if ready {
			labels["laboratory.cybericebox.com/node-agent-ready"] = "true"
		}
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		}}
	}
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("a", true, &laboratoryv1alpha1.TenantQuota{CPU: "1", Memory: "1Gi"})},
		node("n1", true, "4", "8Gi"), node("n2", true, "2", "16Gi"), node("n3", false, "64", "256Gi"))
	h.SetLabScheduling(map[string]string{"laboratory.cybericebox.com/node-agent-ready": "true"}, nil)
	h.SetNodeReserve(nodecap.Reserve{Node: nodecap.Amount{CPU: 500}, Percent: 0})
	got, err := h.GetCapacity(asClient("a"), &protobuf.Empty{})
	if err != nil || !got.HasMaxDevice {
		t.Fatalf("%+v %v", got, err)
	}
	// n3 has no ready node-agent; n1 gives the most CPU (4000-500), n2 the most memory.
	if got.MaxDeviceCpuMillicores != 3500 || got.MaxDeviceMemoryBytes != 16<<30 {
		t.Errorf("largest device = %dm / %d", got.MaxDeviceCpuMillicores, got.MaxDeviceMemoryBytes)
	}
	// Nothing of the layout is in the message: the generated type has no node fields at all.
	if _, found := reflect.TypeOf(got).Elem().FieldByName("Nodes"); found {
		t.Error("the capacity must not carry a node list")
	}
	// No lab node: no value, so a backend does not take it for "nothing fits".
	empty := tenantHandler(t, []*laboratoryv1alpha1.Tenant{newTenantTenant("a", true, nil)})
	if got, _ = empty.GetCapacity(asClient("a"), &protobuf.Empty{}); got.HasMaxDevice {
		t.Errorf("no node, no largest device: %+v", got)
	}
}

// The capacity a tenant is told is net of the hidden packing reserve: its quota (or, with none, the real room of the lab nodes,
// whichever is smaller) less the percentage. The reserve itself is never in the message.
func TestCapacityIsNetOfThePackingReserve(t *testing.T) {
	node := func(name string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10"), corev1.ResourceMemory: resource.MustParse("20Gi")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		}}
	}
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{
		newTenantTenant("small", true, &laboratoryv1alpha1.TenantQuota{CPU: "4", Memory: "8Gi"}),
		newTenantTenant("huge", true, &laboratoryv1alpha1.TenantQuota{CPU: "100", Memory: "100Gi"}),
		newTenantTenant("open", true, nil)}, node("n1"), node("n2"))
	h.SetPackingReserve(15)
	cases := map[string][2]int64{
		"small": {3400, 8 << 30 * 85 / 100},   // the quota is below the room: 85% of it
		"huge":  {17000, 40 << 30 * 85 / 100}, // the quota is above the room (20 CPU, 40Gi): 85% of the room
		"open":  {17000, 40 << 30 * 85 / 100}, // no quota: the room
	}
	for name, want := range cases {
		got, err := h.GetCapacity(asClient(name), &protobuf.Empty{})
		if err != nil || !got.HasCpuQuota || got.CpuQuotaMillicores != want[0] || got.MemoryQuotaBytes != want[1] {
			t.Errorf("%s: %+v %v, want %d / %d", name, got, err, want[0], want[1])
		}
	}
	f, err := h.GetFeatures(asClient("small"), &protobuf.Empty{})
	if err != nil || f.TenantQuota.CpuQuotaMillicores != 3400 {
		t.Errorf("the features quota is net of it as well: %+v %v", f.GetTenantQuota(), err)
	}
}
