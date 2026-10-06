package grpc

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func window(name, reason string, from time.Duration, to *time.Duration, tenants ...string) *laboratoryv1alpha1.MaintenanceWindow {
	f := metav1.NewTime(time.Now().Add(from))
	w := &laboratoryv1alpha1.MaintenanceWindow{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       laboratoryv1alpha1.MaintenanceWindowSpec{From: f, Reason: reason, Tenants: tenants},
	}
	if to != nil {
		t := metav1.NewTime(time.Now().Add(*to))
		w.Spec.To = &t
	}
	return w
}

func dp(d time.Duration) *time.Duration { return &d }

func addWindows(t *testing.T, h *Handler, ws ...*laboratoryv1alpha1.MaintenanceWindow) {
	t.Helper()
	for _, w := range ws {
		if _, err := h.cs.LaboratoryV1alpha1().MaintenanceWindows().Create(context.Background(), w, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

func windowNames(l *protobuf.MaintenanceWindowList) string {
	var out []string
	for _, it := range l.Items {
		out = append(out, it.Name+":"+it.State)
	}
	return strings.Join(out, " ")
}

func TestListMaintenanceWindowsStatesAndOrder(t *testing.T) {
	h, _ := newFinalizerHandler(t)
	addWindows(t, h,
		window("later", "upgrade", 48*time.Hour, dp(50*time.Hour)),
		window("now", "restart", -time.Hour, dp(time.Hour)),
		window("open", "open ended", -2*time.Hour, nil),
		window("over", "done", -10*time.Hour, dp(-9*time.Hour)),
	)
	got, err := h.ListMaintenanceWindows(context.Background(), &protobuf.ListMaintenanceWindowsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Soonest first; the one that is over is left out.
	if want := "open:Active now:Active later:Upcoming"; windowNames(got) != want {
		t.Fatalf("windows = %q, want %q", windowNames(got), want)
	}
	open := got.Items[0]
	if open.ToUnixMs != 0 || open.Reason != "open ended" || !open.AllTenants || open.FromUnixMs == 0 {
		t.Fatalf("open = %+v", open)
	}
	if got.Items[1].ToUnixMs == 0 {
		t.Fatalf("a window with an end reports it: %+v", got.Items[1])
	}
	all, _ := h.ListMaintenanceWindows(context.Background(), &protobuf.ListMaintenanceWindowsRequest{IncludePast: true})
	if want := "over:Past open:Active now:Active later:Upcoming"; windowNames(all) != want {
		t.Fatalf("with the past: %q", windowNames(all))
	}
}

// A window with tenants applies to those only; one without applies to everybody. A tenant
// never learns that another tenant has a window of its own.
func TestListMaintenanceWindowsIsPerTenant(t *testing.T) {
	h, _ := newFinalizerHandler(t)
	addWindows(t, h,
		window("everyone", "all", time.Hour, nil),
		window("only-a", "a", 2*time.Hour, nil, "tenant-a"),
		window("a-and-b", "ab", 3*time.Hour, nil, "tenant-a", "tenant-b"),
		window("only-c", "c", 4*time.Hour, nil, "tenant-c"),
	)
	list := func(tenant string) *protobuf.MaintenanceWindowList {
		got, err := h.ListMaintenanceWindows(asClient(tenant), &protobuf.ListMaintenanceWindowsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := windowNames(list("tenant-a")); got != "everyone:Upcoming only-a:Upcoming a-and-b:Upcoming" {
		t.Fatalf("a sees %q", got)
	}
	if got := windowNames(list("tenant-b")); got != "everyone:Upcoming a-and-b:Upcoming" {
		t.Fatalf("b sees %q", got)
	}
	if got := windowNames(list("nobody")); got != "everyone:Upcoming" {
		t.Fatalf("a tenant not named sees %q", got)
	}
	for _, it := range list("tenant-a").Items {
		if it.AllTenants != (it.Name == "everyone") {
			t.Fatalf("all_tenants of %s = %v", it.Name, it.AllTenants)
		}
	}
	// A long certificate CN is hashed to a tenant key; the window may name the CN itself.
	long := strings.Repeat("c", 70)
	addWindows(t, h, window("long", "l", time.Hour, nil, long))
	if got := windowNames(list(long)); !strings.Contains(got, "long:") {
		t.Fatalf("a long CN sees %q", got)
	}
	if got := windowNames(list("tenant-a")); strings.Contains(got, "long:") {
		t.Fatalf("another tenant sees %q", got)
	}
}

// The agent only reports: a call in the middle of a window still works for any tenant.
func TestMaintenanceWindowDoesNotRefuseWork(t *testing.T) {
	h, _ := newFinalizerHandler(t)
	addWindows(t, h, window("now", "everything is down", -time.Hour, dp(time.Hour)))
	res, err := h.CreateLabGroups(context.Background(), &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: "g"}}})
	wantStates(t, res, err, stCreated)
}

// Against a real API server: the schema accepts the object, rejects a window that ends before
// it starts, and the RPC reads it back.
func TestMaintenanceWindowsAgainstTheAPIServer(t *testing.T) {
	h, _ := newTestHandler(t)
	ctx := context.Background()
	cs := h.cs.LaboratoryV1alpha1().MaintenanceWindows()

	good := window("kernel", "kernel upgrade", time.Hour, dp(3*time.Hour), "tenant-a")
	if _, err := cs.Create(ctx, good, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Create(ctx, window("open", "open", 2*time.Hour, nil), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Create(ctx, window("backwards", "bad", 3*time.Hour, dp(time.Hour)), metav1.CreateOptions{}); err == nil {
		t.Fatal("a window that ends before it starts must be refused by the schema")
	}
	long := window("long-reason", strings.Repeat("r", 501), time.Hour, nil)
	if _, err := cs.Create(ctx, long, metav1.CreateOptions{}); err == nil {
		t.Fatal("a reason of more than 500 characters must be refused")
	}

	// A window may leave some capacity; the schema names only cpu and memory.
	partial := window("partial", "rolling restart", 4*time.Hour, nil)
	partial.Spec.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}
	if _, err := cs.Create(ctx, partial, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pods := window("pods", "bad", 4*time.Hour, nil)
	pods.Spec.Capacity = corev1.ResourceList{corev1.ResourcePods: resource.MustParse("5")}
	if _, err := cs.Create(ctx, pods, metav1.CreateOptions{}); err == nil {
		t.Fatal("a capacity that names a resource other than cpu and memory must be refused")
	}

	got, err := h.ListMaintenanceWindows(asClient("tenant-a"), &protobuf.ListMaintenanceWindowsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if last := got.Items[len(got.Items)-1]; last.Name != "partial" || !last.HasCapacity || last.CapacityCpuMillicores != 500 || last.CapacityMemoryBytes != 0 {
		t.Fatalf("a window with a capacity reports it (a resource not named is zero): %+v", last)
	}
	if got.Items[0].HasCapacity {
		t.Fatalf("a window without a capacity leaves nothing: %+v", got.Items[0])
	}
	got.Items = got.Items[:len(got.Items)-1]
	if windowNames(got) != "kernel:Upcoming open:Upcoming" || got.Items[0].Reason != "kernel upgrade" || got.Items[0].AllTenants {
		t.Fatalf("tenant-a: %q %+v", windowNames(got), got.Items)
	}
	got, _ = h.ListMaintenanceWindows(asClient("tenant-b"), &protobuf.ListMaintenanceWindowsRequest{})
	if windowNames(got) != "open:Upcoming partial:Upcoming" {
		t.Fatalf("tenant-b: %q", windowNames(got))
	}
}
