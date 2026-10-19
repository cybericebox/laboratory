package laboratory

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/nodecap"
)

// maxFixture is a scheduler with the resource check on, devices of 1 CPU and 1Gi each, and a budget of n times 2 CPU and 4Gi for
// the autoscaled nodes.
func maxFixture(t *testing.T, n int) *schedFixture {
	t.Helper()
	cfg := schedCfg()
	cfg.MaxPods = 20
	cfg.ResourceCheck = true
	cfg.Maximum = nodecap.Maximum{Budget: nodecap.Amount{CPU: int64(n) * 2000, Memory: int64(n) * 4 << 30}}
	f := newSchedFixture(t, cfg)
	f.s.Defaults = DeviceDefaults{CPU: "1", Memory: "1Gi"}
	return f
}

// passEnv runs one plan on the fixture's cluster as a tick would, without writing its outcome.
func (f *schedFixture) passEnv() (*clusterEnv, schedPlan) {
	f.t.Helper()
	snap, err := f.s.load(context.Background(), false)
	if err != nil {
		f.t.Fatal(err)
	}
	objs := f.s.objects(snap, f.now)
	if f.s.prepared == nil {
		f.s.prepared, f.s.recent, f.s.lastWrite = map[string]struct{}{}, map[string]time.Time{}, map[types.UID]time.Time{}
	}
	f.s.preparing = map[string]bool{}
	env := &clusterEnv{s: f.s, ctx: context.Background(), snap: snap, now: f.now, objs: objs}
	plan := planSchedule(objs, f.s.Config.MaxPods, false, env)
	if env.err != nil {
		f.t.Fatal(env.err)
	}
	return env, plan
}

// groupNode is a node of an autoscaled node group: its room counts against the budget.
func groupNode(name, cpu, mem string) *corev1.Node {
	n := node(name, cpu, mem)
	n.Labels[nodecap.LabelNodeGroup] = "g"
	return n
}

func roomNames(rooms []nodeRoom) []string {
	var out []string
	for _, r := range rooms {
		out = append(out, r.Name)
	}
	return out
}

// No lab node and a budget: the pass does not stop, a gang of one and a half nodes' worth is sent whole.
func TestScaleFromZeroDispatches(t *testing.T) {
	f := maxFixture(t, 2)
	f.addLab("a", "g", nil, "p1", "p2", "p3")
	f.tick()
	f.wantStates("a/p1=S a/p2=S a/p3=S")
	if s := f.labStatus("a"); s == nil || s.Reason == laboratoryv1alpha1.WaitNoSchedulableNodes {
		t.Fatalf("status = %+v", s)
	}
}

// The pods sent onto the virtual room hold it while they wait for a node: no later gang is sent past the budget.
func TestUnboundPodsHoldVirtualRooms(t *testing.T) {
	f := maxFixture(t, 2)
	f.addLab("a", "g1", nil, "p1", "p2", "p3")
	f.tick()
	f.addLab("b", "g2", nil, "p1", "p2")
	f.addLab("c", "g3", nil, "p1")
	f.tick()
	f.tick()
	f.wantStates("a/p1=S a/p2=S a/p3=S b/p1=Q b/p2=Q c/p1=S")
	if s := f.labStatus("b"); s.Reason != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("b status = %+v", s)
	}
}

// A gang that fits the maximum is sent, across nodes; only one beyond it is too large. An autoscaled node counts against the
// budget: here the maximum is 4 CPU in all.
func TestOversizedOnlyBeyondMaximum(t *testing.T) {
	f := maxFixture(t, 2)
	f.create(groupNode("n1", "2", "4Gi"))
	f.addLab("a", "g", nil, "p1", "p2", "p3", "p4")
	f.tick()
	f.wantStates("a/p1=S a/p2=S a/p3=S a/p4=S")

	f2 := maxFixture(t, 2)
	f2.create(groupNode("n1", "2", "4Gi"))
	f2.addLab("a", "g", nil, "p1", "p2", "p3", "p4", "p5")
	f2.tick()
	f2.wantStates("a/p1=Q a/p2=Q a/p3=Q a/p4=Q a/p5=Q")
	if s := f2.labStatus("a"); s.Reason != laboratoryv1alpha1.WaitDoesNotFit {
		t.Fatalf("status = %+v", s)
	}
	if !f2.s.oversizeWarned["uid-a"] {
		t.Fatal("GroupTooLarge is told once the gang is beyond the maximum")
	}
}

// With a budget left nothing is failed for good (a node still to come may be larger than every node that exists); without a
// budget a pod larger than every node fails as before.
func TestNoNeverFitsWithBudget(t *testing.T) {
	for _, tc := range []struct {
		maxNodes int
		want     string
	}{{2, "a/big=S"}, {0, "a/big=F"}} {
		f := maxFixture(t, tc.maxNodes)
		f.create(node("n1", "1", "1Gi"))
		f.addLab("a", "g", nil, "big")
		setDeviceResources(t, f, "a", &laboratoryv1alpha1.DeviceResources{CPULimit: "2", MemoryLimit: "2Gi"})
		f.tick()
		f.wantStates(tc.want)
	}
}

// The real nodes come first: a gang that fits there leaves the virtual room untouched.
func TestRealRoomsPreferred(t *testing.T) {
	f := maxFixture(t, 3)
	f.create(groupNode("n1", "2", "4Gi"))
	f.addLab("a", "g", nil, "p1", "p2")
	env, plan := f.passEnv()
	if got := roomNames(env.capacity().rooms()); len(got) != 2 || got[0] != "n1" || got[1] != nodecap.VirtualRoomName {
		t.Fatalf("rooms = %v", got)
	}
	if len(plan.dispatch) != 2 || plan.left[0].CPU != 0 || plan.left[1].CPU != 4000 {
		t.Fatalf("dispatch %d, left %+v", len(plan.dispatch), plan.left)
	}
}

// A node a cluster autoscaler is removing is not schedulable: its room becomes virtual.
func TestCATaintedNodeLeavesRooms(t *testing.T) {
	f := maxFixture(t, 2)
	f.create(groupNode("n1", "2", "4Gi"))
	leaving := groupNode("n2", "8", "16Gi")
	leaving.Spec.Taints = []corev1.Taint{{Key: "ToBeDeletedByClusterAutoscaler", Effect: corev1.TaintEffectNoSchedule}}
	f.create(leaving)
	env, _ := f.passEnv()
	c := env.capacity()
	if got := roomNames(c.rooms()); c.nodes != 1 || len(got) != 2 || got[1] != nodecap.VirtualRoomName || c.rooms()[1].CPU != 2000 {
		t.Fatalf("nodes %d, rooms %v %+v", c.nodes, got, c.rooms())
	}
}

// The budget covers the autoscaled nodes only: a fixed node (the controller) adds its room beside the budget, an autoscaled
// node takes its room off it.
func TestFixedNodesOutsideBudget(t *testing.T) {
	f := maxFixture(t, 2)
	f.create(node("cp-1", "2", "4Gi"))
	env, _ := f.passEnv()
	if r := env.capacity().rooms(); len(r) != 2 || r[0].CPU != 2000 || r[1].CPU != 4000 || r[1].Memory != 8<<30 {
		t.Fatalf("rooms = %+v", r)
	}
	f.create(groupNode("w1", "2", "4Gi"))
	env, _ = f.passEnv()
	if r := env.capacity().rooms(); len(r) != 3 || r[2].CPU != 2000 || r[2].Memory != 4<<30 {
		t.Fatalf("rooms = %+v", r)
	}
}

// With no budget nothing changes: the rooms are the real ones.
func TestMaximumOffUnchanged(t *testing.T) {
	f := maxFixture(t, 0)
	f.create(node("n1", "2", "4Gi"))
	f.addLab("a", "g", nil, "p1", "p2", "p3")
	f.tick()
	f.wantStates("a/p1=Q a/p2=Q a/p3=Q")
	if s := f.labStatus("a"); s.Reason != laboratoryv1alpha1.WaitDoesNotFit {
		t.Fatalf("status = %+v", s)
	}
	env, _ := f.passEnv()
	if got := roomNames(env.capacity().rooms()); len(got) != 1 || got[0] != "n1" {
		t.Fatalf("rooms %v", got)
	}
}

// shortMaxFixture is shortFixture with a budget of 1 CPU and 1Gi for autoscaled nodes beside its fixed node.
func shortMaxFixture(t *testing.T) *schedFixture {
	t.Helper()
	f := shortFixture(t)
	f.s.Config.Maximum = nodecap.Maximum{Budget: nodecap.Amount{CPU: 1000, Memory: 1 << 30}}
	return f
}

// Reservations the nodes that exist cannot hold, but the maximum can, are covered: no shortfall and no unreserved work marked.
func TestShortfallCoveredByMaximum(t *testing.T) {
	f := shortMaxFixture(t)
	unreservedHolding(f)
	f.tick()
	f.tick()
	for _, n := range []string{"u", "v"} {
		if got := f.evictAtOf(n); got != "" {
			t.Fatalf("%s is marked: %q", n, got)
		}
	}
	if c := shortfallCond(f.tenant("t1")); c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "nodes not yet present") {
		t.Fatalf("t1 condition = %+v", c)
	}
}

// Beyond the maximum the shortfall and the cleanup of unreserved work are as before.
func TestEvictOnlyBeyondMaximum(t *testing.T) {
	f := shortMaxFixture(t)
	f.addLab("u", "", nil, "p1", "p2")
	f.addLab("v", "", nil, "p1")
	f.tick()
	f.running("u", "p1")
	f.running("u", "p2")
	f.running("v", "p1")
	f.addReservation("r1", "t1", "1900m", "1900Mi")
	f.tick()
	for _, n := range []string{"u", "v"} {
		if got := f.evictAtOf(n); got == "" {
			t.Fatalf("%s is not marked", n)
		}
	}
}

// The condition says what part of the capacity is on nodes not yet present, and when the breaker holds admission.
func TestShortfallMessageNamesVirtual(t *testing.T) {
	r := shortfallReport{known: true, checked: true, peak: amount{cpu: 3000, mem: 3 << 30}, capacity: amount{cpu: 2000, mem: 2 << 30},
		short: amount{cpu: 1000, mem: 1 << 30}, virtual: amount{cpu: 1000, mem: 1 << 30}}
	if c := r.condition(); !strings.Contains(c.Message, "of which cpu 1000m and memory 1073741824 bytes on nodes not yet present (capacity budget)") {
		t.Fatalf("message = %q", c.Message)
	}
	r.breaker, r.timeout = true, 15*time.Minute
	if c := r.condition(); !strings.Contains(c.Message, "no new lab node came within 15m0s: admission uses the current nodes only") {
		t.Fatalf("message = %q", c.Message)
	}
	r.short = amount{}
	if c := r.condition(); c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "admission uses the current nodes only") {
		t.Fatalf("fit message = %+v", c)
	}
}
