package laboratory

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/nodecap"
)

// maxFixture is a scheduler with the resource check on, devices of 1 CPU and 1Gi each, and a static maximum of nodes of
// 2 CPU and 4Gi.
func maxFixture(t *testing.T, maxNodes int) *schedFixture {
	t.Helper()
	cfg := schedCfg()
	cfg.MaxPods = 20
	cfg.ResourceCheck = true
	cfg.Maximum = nodecap.Maximum{Nodes: maxNodes, Added: nodecap.Amount{CPU: 2000, Memory: 4 << 30}}
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

func roomNames(rooms []nodeRoom) []string {
	var out []string
	for _, r := range rooms {
		out = append(out, r.Name)
	}
	return out
}

// No lab node and room for two more: the pass does not stop, a gang of one and a half nodes is sent whole.
func TestScaleFromZeroDispatches(t *testing.T) {
	f := maxFixture(t, 2)
	f.addLab("a", "g", nil, "p1", "p2", "p3")
	f.tick()
	f.wantStates("a/p1=S a/p2=S a/p3=S")
	if s := f.labStatus("a"); s == nil || s.Reason == laboratoryv1alpha1.WaitNoSchedulableNodes {
		t.Fatalf("status = %+v", s)
	}
}

// The pods sent onto virtual room hold it while they wait for a node: no later gang is sent past the maximum.
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

// A gang that fits the nodes that exist plus the ones still to come is sent; only one beyond the maximum is too large.
func TestOversizedOnlyBeyondMaximum(t *testing.T) {
	f := maxFixture(t, 2)
	f.create(node("n1", "2", "4Gi"))
	f.addLab("a", "g", nil, "p1", "p2", "p3", "p4")
	f.tick()
	f.wantStates("a/p1=S a/p2=S a/p3=S a/p4=S")

	f2 := maxFixture(t, 2)
	f2.create(node("n1", "2", "4Gi"))
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

// A pod larger than every node that exists but within an added node waits for one; without a maximum it fails.
func TestNeverFitsUsesAddedNode(t *testing.T) {
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
	f.create(node("n1", "2", "4Gi"))
	f.addLab("a", "g", nil, "p1", "p2")
	env, plan := f.passEnv()
	if got := roomNames(env.capacity().rooms()); len(got) != 3 || got[0] != "n1" || got[1] != "virtual/0" || got[2] != "virtual/1" {
		t.Fatalf("rooms = %v", got)
	}
	if len(plan.dispatch) != 2 || plan.left[0].CPU != 0 || plan.left[1].CPU != 2000 || plan.left[2].CPU != 2000 {
		t.Fatalf("dispatch %d, left %+v", len(plan.dispatch), plan.left)
	}
}

// A node a cluster autoscaler is removing is not schedulable: its room becomes virtual.
func TestCATaintedNodeLeavesRooms(t *testing.T) {
	f := maxFixture(t, 2)
	f.create(node("n1", "2", "4Gi"))
	leaving := node("n2", "8", "16Gi")
	leaving.Spec.Taints = []corev1.Taint{{Key: "ToBeDeletedByClusterAutoscaler", Effect: corev1.TaintEffectNoSchedule}}
	f.create(leaving)
	env, _ := f.passEnv()
	c := env.capacity()
	if got := roomNames(c.rooms()); c.nodes != 1 || len(got) != 2 || got[1] != "virtual/0" || c.rooms()[1].CPU != 2000 {
		t.Fatalf("nodes %d, rooms %v %+v", c.nodes, got, c.rooms())
	}
}

// The DaemonSets that would land on a node still to come take their requests off its room.
func TestVirtualRoomLessDaemonSets(t *testing.T) {
	f := maxFixture(t, 1)
	ds := &appsv1.DaemonSet{}
	ds.Name, ds.Namespace = "cilium", "kube-system"
	ds.Spec.Template.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	ds.Spec.Template.Spec.Containers = []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi")}}}}
	f.createPlain(ds)
	env, _ := f.passEnv()
	if r := env.capacity().rooms(); len(r) != 1 || r[0].CPU != 1500 || r[0].Memory != 4<<30-512<<20 {
		t.Fatalf("rooms = %+v", r)
	}
}

// With no maximum nothing changes: the rooms are the real ones, and no DaemonSet is read.
func TestMaximumOffUnchanged(t *testing.T) {
	f := maxFixture(t, 0)
	read := 0
	f.s.Client = interceptorClient(f.c.(client.WithWatch), &read)
	f.create(node("n1", "2", "4Gi"))
	f.addLab("a", "g", nil, "p1", "p2", "p3")
	f.tick()
	f.wantStates("a/p1=Q a/p2=Q a/p3=Q")
	if s := f.labStatus("a"); s.Reason != laboratoryv1alpha1.WaitDoesNotFit {
		t.Fatalf("status = %+v", s)
	}
	env, _ := f.passEnv()
	if got := roomNames(env.capacity().rooms()); len(got) != 1 || got[0] != "n1" || read != 0 {
		t.Fatalf("rooms %v, DaemonSet lists %d", got, read)
	}
}

// interceptorClient counts the lists of DaemonSets.
func interceptorClient(c client.WithWatch, lists *int) client.WithWatch {
	return interceptor.NewClient(c, interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*appsv1.DaemonSetList); ok {
			*lists++
		}
		return cl.List(ctx, list, opts...)
	}})
}
