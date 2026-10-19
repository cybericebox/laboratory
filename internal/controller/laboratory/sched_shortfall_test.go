package laboratory

import (
	"context"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func windowed(name, tenantName, cpu, mem string, from, to time.Duration) laboratoryv1alpha1.Reservation {
	return laboratoryv1alpha1.Reservation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: laboratoryv1alpha1.ReservationSpec{Tenant: tenantName, From: metav1.NewTime(planEpoch.Add(from)), To: metav1.NewTime(planEpoch.Add(to)),
			Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)}},
	}
}

// The peak is the largest sum of the reservations open at the same time, per resource: windows are half open (one ending when
// another starts do not overlap), the CPU and memory peaks may lie in different windows, ended, deleted and invalid
// reservations reserve nothing, and only [from, to) is looked at.
func TestPeakDemandOverlap(t *testing.T) {
	gone := windowed("gone", "t1", "10", "10Gi", 0, 4*time.Hour)
	gone.DeletionTimestamp = &metav1.Time{Time: planEpoch}
	ended := windowed("ended", "t1", "10", "10Gi", 0, 4*time.Hour)
	ended.Status.Phase = laboratoryv1alpha1.ReservationPhaseEnded
	invalid := windowed("invalid", "t1", "0", "10Gi", 0, 4*time.Hour)
	res := []laboratoryv1alpha1.Reservation{
		windowed("a", "t1", "1", "1Gi", 0, 2*time.Hour),
		windowed("b", "t2", "2", "2Gi", time.Hour, 3*time.Hour),
		windowed("c", "t1", "500m", "4Gi", 2*time.Hour, 4*time.Hour),
		gone, ended, invalid,
	}
	cases := []struct {
		name     string
		from, to time.Duration
		cpu      int64
		mem      int64
	}{
		{"whole", 0, 5 * time.Hour, 3000, 6 * gi},
		{"after a ends", 2*time.Hour + 30*time.Minute, 5 * time.Hour, 2500, 6 * gi},
		{"a alone", 0, time.Hour, 1000, gi},
		{"nothing open", 4 * time.Hour, 5 * time.Hour, 0, 0},
	}
	for _, c := range cases {
		got := peakDemand(res, planEpoch.Add(c.from), planEpoch.Add(c.to))
		if got.Cpu().MilliValue() != c.cpu || got.Memory().Value() != c.mem {
			t.Errorf("%s: peak = %v, want cpu %dm memory %d", c.name, got, c.cpu, c.mem)
		}
	}
}

// shortFixture is a scheduler with the per-node check on one lab node of 1 CPU and 1 GiB (the devices default to 100m and
// 100Mi), the tenant t1 and a notice of five minutes for unreserved work.
func shortFixture(t *testing.T) *schedFixture {
	t.Helper()
	cfg := schedCfg()
	cfg.MaxPods = 10
	cfg.ResourceCheck = true
	cfg.UnreservedNotice = 5 * time.Minute
	f := newSchedFixture(t, cfg)
	f.create(node("n1", "1", "1Gi"))
	f.create(&laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "t1", UID: "uid-t1"}})
	return f
}

func (f *schedFixture) tenant(name string) *laboratoryv1alpha1.Tenant {
	f.t.Helper()
	var t laboratoryv1alpha1.Tenant
	if err := f.c.Get(context.Background(), types.NamespacedName{Name: name}, &t); err != nil {
		f.t.Fatal(err)
	}
	return &t
}

func (f *schedFixture) addReservationWindow(r laboratoryv1alpha1.Reservation) {
	f.t.Helper()
	r.UID = types.UID("uid-res-" + r.Name)
	f.createPlain(&r)
}

// lab is the Lab of the fixture; nil when it is gone.
func (f *schedFixture) lab(name string) *laboratoryv1alpha1.Lab {
	f.t.Helper()
	var l laboratoryv1alpha1.Lab
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "ns-" + name, Name: name}, &l); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		f.t.Fatal(err)
	}
	return &l
}

func (f *schedFixture) evictAtOf(name string) string {
	f.t.Helper()
	l := f.lab(name)
	if l == nil {
		f.t.Fatalf("lab %s is gone", name)
	}
	return l.Annotations[names.AnnotationEvictAt]
}

// gauge reads laboratory_reservation_shortfall of a resource.
func gauge(t *testing.T, res string) float64 {
	t.Helper()
	var m dto.Metric
	if err := reservationShortfall.WithLabelValues(res).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetGauge().GetValue()
}

func shortfallCond(t *laboratoryv1alpha1.Tenant) *metav1.Condition {
	return meta.FindStatusCondition(t.Status.Conditions, laboratoryv1alpha1.TenantConditionCapacityShortfall)
}

// Every tenant with an active or future reservation gets CapacityShortfall when the peak overlap does not fit the empty lab
// nodes, with the numbers; a tenant without one does not; the condition turns False when the reservations fit again.
func TestShortfallConditionOnAllTenants(t *testing.T) {
	f := shortFixture(t)
	f.create(&laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "t2", UID: "uid-t2"}})
	f.create(&laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "t3", UID: "uid-t3"}})
	f.create(&laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "t4", UID: "uid-t4"}})
	f.addReservationWindow(windowed("now", "t1", "600m", "512Mi", 0, 3*time.Hour))
	f.addReservationWindow(windowed("later", "t2", "600m", "256Mi", 2*time.Hour, 4*time.Hour))
	f.addReservationWindow(windowed("past", "t4", "1", "1Gi", 0, 30*time.Minute)) // over before now (planEpoch + 1h)
	f.tick()
	for _, n := range []string{"t1", "t2"} {
		c := shortfallCond(f.tenant(n))
		if c == nil || c.Status != metav1.ConditionTrue || c.Reason != laboratoryv1alpha1.TenantReasonReservationsDoNotFit {
			t.Fatalf("%s: condition = %+v", n, c)
		}
		for _, want := range []string{"up to cpu 1200m and memory 805306368 bytes", "hold cpu 1000m and memory 1073741824 bytes", "short of cpu 200m"} {
			if !strings.Contains(c.Message, want) {
				t.Fatalf("%s: message %q lacks %q", n, c.Message, want)
			}
		}
	}
	for _, n := range []string{"t3", "t4"} {
		if c := shortfallCond(f.tenant(n)); c != nil {
			t.Fatalf("%s has no active or future reservation: %+v", n, c)
		}
	}
	later := f.reservation("later")
	later.Spec.Resources[corev1.ResourceCPU] = resource.MustParse("400m")
	if err := f.c.Update(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	f.tick()
	for _, n := range []string{"t1", "t2"} {
		if c := shortfallCond(f.tenant(n)); c == nil || c.Status != metav1.ConditionFalse || c.Reason != laboratoryv1alpha1.TenantReasonReservationsFit {
			t.Fatalf("%s after the fit: %+v", n, c)
		}
	}
}

// The scheduler's patch of the tenant conditions and the agent's write of status.reserved/used (UpdateStatus of the whole
// status from the copy it listed) never erase each other: each carries the resourceVersion, so the one that comes second on a
// stale copy conflicts and is done again from a fresh read.
func TestTenantStatusWritersCoexist(t *testing.T) {
	f := shortFixture(t)
	f.s.lastWrite = map[types.UID]time.Time{}
	ctx := context.Background()
	sf := shortfallReport{known: true, checked: true, tenants: map[string]bool{"t1": true}, short: amount{cpu: 100, mem: mi}, peak: amount{cpu: 1100, mem: gi}, capacity: amount{cpu: 1000, mem: gi}}
	write := func(from *laboratoryv1alpha1.Tenant) {
		t.Helper()
		f.now = f.now.Add(time.Second)
		f.s.writeTenants(ctx, &clusterView{tenants: map[string]*laboratoryv1alpha1.Tenant{"t1": from}}, sf, f.now, time.Nanosecond, 10, map[types.UID]bool{})
	}
	agent := func(from *laboratoryv1alpha1.Tenant, cpu string) error {
		from.Status.Reserved = laboratoryv1alpha1.TenantUsage{CPU: cpu, Memory: "1Gi"}
		return f.c.Status().Update(ctx, from)
	}

	// the agent read before the scheduler wrote: its update conflicts, and the next one keeps the condition
	stale := f.tenant("t1")
	write(f.tenant("t1"))
	if err := agent(stale, "1"); !apierrors.IsConflict(err) {
		t.Fatalf("the agent's update over a newer status must conflict, got %v", err)
	}
	if err := agent(f.tenant("t1"), "1"); err != nil {
		t.Fatal(err)
	}
	if got := f.tenant("t1"); shortfallCond(got) == nil || got.Status.Reserved.CPU != "1" {
		t.Fatalf("status = %+v", got.Status)
	}

	// the scheduler writes from a copy older than the agent's write: no write, and the agent's numbers stay
	stale = f.tenant("t1")
	if err := agent(f.tenant("t1"), "2"); err != nil {
		t.Fatal(err)
	}
	sf.short = amount{cpu: 300, mem: mi}
	write(stale)
	got := f.tenant("t1")
	if got.Status.Reserved.CPU != "2" || !strings.Contains(shortfallCond(got).Message, "short of cpu 100m") {
		t.Fatalf("a stale scheduler write went through: %+v", got.Status)
	}
	write(f.tenant("t1"))
	got = f.tenant("t1")
	if got.Status.Reserved.CPU != "2" || !strings.Contains(shortfallCond(got).Message, "short of cpu 300m") {
		t.Fatalf("status after the retry = %+v", got.Status)
	}
}

// running binds a pod of the device (100m, 100Mi) to n1 and makes the device Ready, so its pod is Started on the next pass.
func (f *schedFixture) running(lab, dev string) { f.runningOn(lab, dev, "n1", "100m", "100Mi") }

func (f *schedFixture) runningOn(lab, dev, node, cpu, mem string) {
	f.t.Helper()
	f.devicePod(lab, dev, func(p *corev1.Pod) {
		p.Spec.NodeName = node
		p.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)}
		p.Status.Phase = corev1.PodRunning
	})
	f.ready(lab, dev)
}

// unreservedHolding starts the unreserved labs u (two pods) and v (one pod) of the default tenant, then opens r1 of t1 for
// 900m and 900Mi: 700m are left beside the labs, so the reservation is short and the default tenant holds the room.
func unreservedHolding(f *schedFixture) {
	f.t.Helper()
	f.addLab("u", "", nil, "p1", "p2")
	f.addLab("v", "", nil, "p1")
	f.tick()
	f.wantStates("u/p1=S u/p2=S v/p1=S")
	f.running("u", "p1")
	f.running("u", "p2")
	f.running("v", "p1")
	f.addReservation("r1", "t1", "900m", "900Mi")
}

// Unreserved work that holds the room of an active reservation is marked with the time it goes (now + the notice), all of
// the tenant's unreserved objects at once, and deleted once that time passed; the gauge shows the shortfall until then.
func TestUnreservedMarkedThenDeleted(t *testing.T) {
	f := shortFixture(t)
	unreservedHolding(f)
	f.addLab("w", "", nil, "p1") // queued, holds nothing: marked too, the cluster does not pick
	f.tick()
	at := f.now.Add(5 * time.Minute).UTC().Format(time.RFC3339)
	for _, n := range []string{"u", "v", "w"} {
		if got := f.evictAtOf(n); got != at {
			t.Fatalf("%s evict-at = %q, want %q", n, got, at)
		}
	}
	if c := shortfallCond(f.tenant("t1")); c == nil || c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "find no room on the nodes now for cpu 200m") {
		t.Fatalf("t1 condition = %+v", c)
	}
	if cpu, mem := gauge(t, "cpu"), gauge(t, "memory"); cpu != 0.2 || mem != float64(200*mi) {
		t.Fatalf("gauge cpu %v memory %v", cpu, mem)
	}
	f.tick()
	if f.evictAtOf("u") != at {
		t.Fatal("the mark moved")
	}
	f.now = f.now.Add(5 * time.Minute)
	f.tick()
	for _, n := range []string{"u", "v", "w"} {
		if f.lab(n) != nil {
			t.Fatalf("%s survived its eviction time", n)
		}
	}
	// their pods go with them
	if err := f.c.DeleteAllOf(context.Background(), &corev1.Pod{}, client.InNamespace("ns-u")); err != nil {
		t.Fatal(err)
	}
	if err := f.c.DeleteAllOf(context.Background(), &corev1.Pod{}, client.InNamespace("ns-v")); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if cpu := gauge(t, "cpu"); cpu != 0 {
		t.Fatalf("gauge after the cleanup = %v", cpu)
	}
}

// The mark goes when the shortfall clears before its time (here the reservation is made smaller), and nothing is deleted.
func TestEvictMarkClearedWhenRoomReturns(t *testing.T) {
	f := shortFixture(t)
	unreservedHolding(f)
	f.tick()
	if f.evictAtOf("u") == "" {
		t.Fatal("not marked")
	}
	r := f.reservation("r1")
	r.Spec.Resources = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("500Mi")}
	if err := f.c.Update(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	f.tick()
	for _, n := range []string{"u", "v"} {
		if got := f.evictAtOf(n); got != "" {
			t.Fatalf("%s still marked: %q", n, got)
		}
	}
	f.now = f.now.Add(10 * time.Minute)
	f.tick()
	if f.lab("u") == nil || f.lab("v") == nil {
		t.Fatal("deleted after the shortfall cleared")
	}
	if c := shortfallCond(f.tenant("t1")); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("t1 condition = %+v", c)
	}
}

// The cleanup takes only unreserved objects of the tenants whose work holds the room: never a labelled object (an empty
// label too), not even of the blocking tenant, never another tenant's object that holds nothing, and never a LabGroup whose
// namespace holds a lab that is not the tenant's unreserved work (deleting the group would drain it).
func TestEvictNeverTouchesReservedOrOtherTenant(t *testing.T) {
	f := shortFixture(t)
	ctx := context.Background()
	f.addReservation("rd", "default", "100m", "100Mi")
	f.addLab("u", "", nil, "p1", "p2")
	f.addLab("y", "", nil, "p1") // the blocking tenant's lab under its own reservation
	f.labUnder("y", "rd", "default")
	f.addLab("e", "", nil, "p1") // an empty reservation label
	f.labUnder("e", "", "default")
	f.addLab("w", "", nil, "p1") // another tenant, nothing out
	f.labUnder("w", "", "t2")
	l := f.lab("w")
	delete(l.Labels, names.LabelReservation)
	if err := f.c.Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	f.addLab("x", "", nil, "p1") // in the namespace of the unreserved group "team"
	f.labUnder("x", "rd", "default")
	f.create(&laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "uid-team", CreationTimestamp: metav1.NewTime(planEpoch)},
		Status:     laboratoryv1alpha1.LabGroupStatus{Namespace: "ns-x"},
	})
	f.create(&laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", UID: "uid-solo", CreationTimestamp: metav1.NewTime(planEpoch)},
		Status:     laboratoryv1alpha1.LabGroupStatus{Namespace: "ns-solo"},
	})
	f.tick()
	f.wantStates("e/p1=S u/p1=S u/p2=S w/p1=S x/p1=Q y/p1=S")
	for _, p := range []string{"e/p1", "u/p1", "u/p2", "y/p1"} {
		lab, dev, _ := strings.Cut(p, "/")
		f.running(lab, dev)
	}
	// w holds nothing: its pod is stopped and gone
	d := f.device("w", "p1")
	d.Status.Scheduling.State = laboratoryv1alpha1.PodQueued
	if err := f.c.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(recentDispatchTTL) // the dispatch of w is no longer taken as in flight
	f.addReservation("r1", "t1", "900m", "900Mi")
	f.tick()
	if f.evictAtOf("u") == "" {
		t.Fatal("u not marked")
	}
	group := func(name string) *laboratoryv1alpha1.LabGroup {
		var g laboratoryv1alpha1.LabGroup
		if err := f.c.Get(ctx, types.NamespacedName{Name: name}, &g); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			t.Fatal(err)
		}
		return &g
	}
	if group("solo").Annotations[names.AnnotationEvictAt] == "" {
		t.Fatal("the unreserved group of the blocking tenant is not marked")
	}
	f.now = f.now.Add(5 * time.Minute)
	f.tick()
	if f.lab("u") != nil || group("solo") != nil {
		t.Fatal("the blocking tenant's unreserved work survived")
	}
	for _, n := range []string{"y", "e", "w", "x"} {
		if f.lab(n) == nil || f.evictAtOf(n) != "" {
			t.Fatalf("%s was touched", n)
		}
	}
	if g := group("team"); g == nil || g.Annotations[names.AnnotationEvictAt] != "" {
		t.Fatalf("the group holding a reserved lab was touched: %+v", g)
	}
}

// A tenant's unreserved work is taken only when the reservations' shortfall shrinks without it: w of t2 holds CPU on n2, where
// the memory is taken by a reserved lab, so its room cannot help r1 and t2 is left alone, while the default tenant's u on n1 is
// marked.
func TestEvictOnlyTenantsWhoseRoomHelps(t *testing.T) {
	f := shortFixture(t)
	f.create(node("n2", "1", "200Mi"))
	f.addReservation("rd", "t3", "100m", "100Mi")
	f.addLab("u", "", nil, "p1", "p2")
	f.addLab("w", "", nil, "p1")
	f.labUnder("w", "", "t2")
	l := f.lab("w")
	delete(l.Labels, names.LabelReservation)
	if err := f.c.Update(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	f.addLab("y", "", nil, "p1")
	f.labUnder("y", "rd", "t3")
	f.tick()
	f.wantStates("u/p1=S u/p2=S w/p1=S y/p1=S")
	f.running("u", "p1")
	f.running("u", "p2")
	f.runningOn("w", "p1", "n2", "500m", "0")
	f.runningOn("y", "p1", "n2", "0", "200Mi")
	f.addReservation("r1", "t1", "900m", "900Mi")
	f.tick()
	if f.evictAtOf("u") == "" {
		t.Fatal("u holds the room r1 needs and is not marked")
	}
	if got := f.evictAtOf("w"); got != "" {
		t.Fatalf("w holds room that cannot help r1 and is marked: %q", got)
	}
}

// At its time the eviction is checked past the cache: when the server says the shortfall is gone (the reservation was made
// smaller, the cache does not know yet), nothing is deleted; the same object goes once the server agrees.
func TestEvictRechecksUncached(t *testing.T) {
	f := shortFixture(t)
	unreservedHolding(f)
	f.tick()
	if f.evictAtOf("u") == "" {
		t.Fatal("not marked")
	}
	shrunk := true
	f.s.Reader = interceptor.NewClient(f.c.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if rl, ok := list.(*laboratoryv1alpha1.ReservationList); ok && shrunk {
				for i := range rl.Items {
					rl.Items[i].Spec.Resources = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("100Mi")}
				}
			}
			return nil
		},
	})
	f.now = f.now.Add(5 * time.Minute)
	f.tick()
	if f.lab("u") == nil || f.lab("v") == nil {
		t.Fatal("deleted on the cache alone")
	}
	if f.evictAtOf("u") == "" {
		t.Fatal("the cache still sees the shortfall: the mark stays")
	}
	shrunk = false
	f.tick()
	if f.lab("u") != nil || f.lab("v") != nil {
		t.Fatal("not deleted once the server agrees")
	}
}

// The scheduler writes status.usedAt with the usage, and again (unchanged usage) once it is half the freshness bound old; an
// ended reservation gets no heartbeat.
func TestUsedAtHeartbeat(t *testing.T) {
	cfg := schedCfg()
	cfg.StatusInterval = 10 * time.Second
	f := newSchedFixture(t, cfg)
	f.addReservation("r1", "t1", "1", "1Gi")
	f.addReservation("old", "t1", "1", "1Gi")
	old := f.reservation("old")
	old.Status.Phase = laboratoryv1alpha1.ReservationPhaseEnded
	old.Status.Used = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0"), corev1.ResourceMemory: resource.MustParse("0")}
	if err := f.c.Status().Update(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	f.tick()
	first := f.reservation("r1").Status.UsedAt
	if first == nil || !first.Time.Equal(f.now) {
		t.Fatalf("usedAt = %v, want %v", first, f.now)
	}
	if f.reservation("old").Status.UsedAt != nil {
		t.Fatal("an ended reservation got a heartbeat")
	}
	f.now = f.now.Add(20 * time.Second)
	f.tick()
	if got := f.reservation("r1").Status.UsedAt; !got.Equal(first) {
		t.Fatalf("written again before the heartbeat: %v", got)
	}
	f.now = f.now.Add(10 * time.Second)
	f.tick()
	if got := f.reservation("r1").Status.UsedAt; !got.Time.Equal(f.now) {
		t.Fatalf("heartbeat missing: %v at %v", got, f.now)
	}
}
