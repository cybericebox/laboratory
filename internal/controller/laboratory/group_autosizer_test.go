package laboratory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/names"
)

type autosizeFixture struct {
	t       *testing.T
	c       client.Client
	a       *GroupAutosizer
	now     time.Time
	lg      *laboratoryv1alpha1.LabGroup
	resizes []grouppods.Overhead
	freeCPU int64
	freeMem int64
	clients int
}

func gp(cpu, mem int64) *laboratoryv1alpha1.GroupPodSize {
	return &laboratoryv1alpha1.GroupPodSize{CPUMillicores: cpu, MemoryBytes: mem}
}

func newAutosizeFixture(t *testing.T, qos corev1.PodQOSClass) *autosizeFixture {
	t.Helper()
	s := clientgoscheme.Scheme
	_ = laboratoryv1alpha1.AddToScheme(s)
	f := &autosizeFixture{t: t, now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), freeCPU: 4000, freeMem: 8 << 30}
	f.lg = &laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "g"},
		Spec: laboratoryv1alpha1.LabGroupSpec{
			VPN:     laboratoryv1alpha1.LabGroupVPNSpec{Size: gp(20, 64<<20), MaxSize: gp(140, 480<<20)},
			Gateway: laboratoryv1alpha1.LabGroupGatewaySpec{},
		},
		Status: laboratoryv1alpha1.LabGroupStatus{Namespace: "g-ns"},
	}
	requests := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse("64Mi")}
	limits := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("30m"), corev1.ResourceMemory: resource.MustParse("80Mi")}
	if qos == corev1.PodQOSGuaranteed {
		limits = requests.DeepCopy()
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn-1", Namespace: "g-ns", Labels: map[string]string{labelApp: names.ComponentVPN}},
		Spec:       corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: names.ComponentVPN, Resources: corev1.ResourceRequirements{Requests: requests, Limits: limits}}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: qos},
	}
	f.c = fake.NewClientBuilder().WithScheme(s).WithObjects(f.lg, pod).WithStatusSubresource(&laboratoryv1alpha1.LabGroup{}).Build()
	f.a = &GroupAutosizer{
		Client:    f.c,
		GroupPods: grouppods.Config{Autosize: grouppods.Autosize{ShrinkAfter: 30 * time.Minute}},
		Now:       func() time.Time { return f.now },
		resize: func(_ context.Context, p *corev1.Pod, container string, cpu, mem int64) error {
			f.resizes = append(f.resizes, grouppods.Overhead{CPU: cpu, Memory: mem})
			live := p.DeepCopy()
			live.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
				corev1.ResourceCPU: *resource.NewMilliQuantity(cpu, resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(mem, resource.BinarySI)}
			return f.c.Update(context.Background(), live)
		},
		nodeFree: func(context.Context, string) (int64, int64, error) { return f.freeCPU, f.freeMem, nil },
	}
	return f
}

func (f *autosizeFixture) addClients(n int) {
	for i := 0; i < n; i++ {
		f.clients++
		c := &laboratoryv1alpha1.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("c%d", f.clients), Namespace: "g-ns"}}
		if err := f.c.Create(context.Background(), c); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *autosizeFixture) sweep() laboratoryv1alpha1.GroupPodSizeStatus {
	f.t.Helper()
	var lg laboratoryv1alpha1.LabGroup
	if err := f.c.Get(context.Background(), client.ObjectKey{Name: "g"}, &lg); err != nil {
		f.t.Fatal(err)
	}
	if err := f.a.SizeGroup(context.Background(), &lg); err != nil {
		f.t.Fatal(err)
	}
	if err := f.c.Get(context.Background(), client.ObjectKey{Name: "g"}, &lg); err != nil {
		f.t.Fatal(err)
	}
	if len(lg.Status.PodSizing) != 1 {
		f.t.Fatalf("status: %+v", lg.Status.PodSizing)
	}
	return lg.Status.PodSizing[0]
}

// The limits follow the clients: base 20m/64Mi + 6m/20Mi per client, rounded up to 5m/8Mi, never above the max, no restart.
func TestAutosizerGrowsTheLimitsWithTheClients(t *testing.T) {
	f := newAutosizeFixture(t, corev1.PodQOSBurstable)
	f.addClients(5) // 20+30 = 50m, 64+100 = 164Mi
	st := f.sweep()
	if st.State != podSizeApplied || st.CPULimitMillicores != 50 || st.MemoryLimitBytes != 168<<20 || st.Units != 5 || len(f.resizes) != 1 {
		t.Fatalf("5 clients: %+v resizes=%v", st, f.resizes)
	}
	f.addClients(30) // far above the max: 140m / 480Mi
	st = f.sweep()
	if st.CPULimitMillicores != 140 || st.MemoryLimitBytes != 480<<20 || len(f.resizes) != 2 {
		t.Fatalf("35 clients: %+v", st)
	}
	f.sweep()
	if len(f.resizes) != 2 {
		t.Fatalf("a settled pod is not resized again: %v", f.resizes)
	}
}

// Hysteresis: memory is never lowered; the CPU goes down only after the lower need lasted for ShrinkAfter.
func TestAutosizerShrinksOnlyTheCPUAndOnlyAfterTheDelay(t *testing.T) {
	f := newAutosizeFixture(t, corev1.PodQOSBurstable)
	f.addClients(10)
	f.sweep()
	var list laboratoryv1alpha1.LabGroupClientList
	_ = f.c.List(context.Background(), &list)
	for i := 0; i < 8; i++ {
		_ = f.c.Delete(context.Background(), &list.Items[i])
	}
	f.sweep()
	grew := len(f.resizes)
	f.now = f.now.Add(29 * time.Minute)
	f.sweep()
	if len(f.resizes) != grew {
		t.Fatalf("no shrink inside the delay: %v", f.resizes)
	}
	f.now = f.now.Add(2 * time.Minute)
	st := f.sweep()
	if len(f.resizes) != grew+1 || st.CPULimitMillicores != 35 {
		t.Fatalf("CPU should shrink to 35m after the delay: %+v %v", st, f.resizes)
	}
	if st.MemoryLimitBytes != 264<<20 {
		t.Fatalf("memory must keep its limit (10 clients): %d", st.MemoryLimitBytes)
	}
}

// A node without room for the whole growth gives what it has, and the status says why; no restart.
func TestAutosizerGrowsToWhatTheNodeHas(t *testing.T) {
	f := newAutosizeFixture(t, corev1.PodQOSBurstable)
	f.freeCPU, f.freeMem = 12, 20<<20
	f.addClients(10) // wants 80m/264Mi, has +12m (-> 40m with 5m steps down: 30+10) and +20Mi (-> +16Mi)
	st := f.sweep()
	if st.State != podSizeLimited || st.CPULimitMillicores != 40 || st.MemoryLimitBytes != 96<<20 || !strings.Contains(st.Message, "node n1") {
		t.Fatalf("%+v", st)
	}
}

// A pod created Guaranteed cannot change its QoS class: it is left alone and the status says it grows at its next recreation.
func TestAutosizerLeavesAGuaranteedPod(t *testing.T) {
	f := newAutosizeFixture(t, corev1.PodQOSGuaranteed)
	f.addClients(5)
	st := f.sweep()
	if st.State != podSizeRecreate || len(f.resizes) != 0 {
		t.Fatalf("%+v %v", st, f.resizes)
	}
}

// A resize the API server refuses is reported, not retried into a restart.
func TestAutosizerReportsARefusedResize(t *testing.T) {
	f := newAutosizeFixture(t, corev1.PodQOSBurstable)
	f.a.resize = func(context.Context, *corev1.Pod, string, int64, int64) error { return context.DeadlineExceeded }
	f.addClients(5)
	st := f.sweep()
	if st.State != podSizePending || !strings.Contains(st.Message, "not accepted") {
		t.Fatalf("%+v", st)
	}
}

func TestLimitFormula(t *testing.T) {
	sz := grouppods.PodSizing{BaseCPU: 20, BaseMemory: 64 << 20, PerUnitCPU: 6, PerUnitMemory: 20 << 20}
	req, max, head := grouppods.Overhead{CPU: 20, Memory: 64 << 20}, grouppods.Overhead{CPU: 140, Memory: 480 << 20}, grouppods.Overhead{CPU: 10, Memory: 16 << 20}
	for _, c := range []struct {
		units    int
		cpu, mem int64
	}{{0, 30, 80 << 20}, {1, 30, 88 << 20}, {5, 50, 168 << 20}, {20, 140, 464 << 20}, {100, 140, 480 << 20}} {
		got := grouppods.Limit(sz, req, max, c.units, head)
		if got.CPU != c.cpu || got.Memory != c.mem {
			t.Errorf("units %d: %dm %dMi, want %dm %dMi", c.units, got.CPU, got.Memory>>20, c.cpu, c.mem>>20)
		}
	}
	// A max not above the request is fixed at the request.
	if got := grouppods.Limit(sz, req, req, 9, head); got != req {
		t.Errorf("fixed pod: %+v", got)
	}
}

// A pod with a max size is created Burstable (requests = the size, limits above), and counted at the max by the scheduler.
func TestPodWithAMaxSizeIsBurstableAndCountedAtTheMax(t *testing.T) {
	cfg := grouppods.Config{}
	res := cfg.PodFor(names.ComponentVPN, gp(20, 64<<20), gp(140, 480<<20), 0)
	if res.Requests.Cpu().MilliValue() != 20 || res.Limits.Cpu().MilliValue() != 30 || res.Limits.Memory().Value() != 80<<20 {
		t.Fatalf("%v / %v", res.Requests, res.Limits)
	}
	fixed := cfg.PodFor(names.ComponentVPN, gp(20, 64<<20), nil, 0)
	if fixed.Limits.Cpu().MilliValue() != 20 {
		t.Fatalf("no max size: fixed, Guaranteed: %v", fixed.Limits)
	}
	lg := &laboratoryv1alpha1.LabGroup{Spec: laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Size: gp(20, 64<<20), MaxSize: gp(140, 480<<20)}}}
	if n := cfg.Requests(names.ComponentVPN, lg); n.Cpu().MilliValue() != 140 || n.Memory().Value() != 480<<20 {
		t.Fatalf("the scheduler counts the max: %v", n)
	}
}
