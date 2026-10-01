package laboratory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

type schedFixture struct {
	t   *testing.T
	c   client.Client
	s   *Scheduler
	now time.Time
	sec int // creation time of the next object
}

func schedCfg() SchedulerConfig {
	return SchedulerConfig{MaxPods: 3, StartupTimeout: 5 * time.Minute, RestartThreshold: 5, StatusInterval: time.Nanosecond}
}

func newSchedFixture(t *testing.T, cfg SchedulerConfig) *schedFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := laboratoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&laboratoryv1alpha1.Lab{}, &laboratoryv1alpha1.LabGroup{}, &laboratoryv1alpha1.Device{}, &laboratoryv1alpha1.Tenant{}, &corev1.Pod{}, &appsv1.DaemonSet{}, &corev1.Node{}).
		Build()
	f := &schedFixture{t: t, c: c, now: planEpoch.Add(time.Hour)}
	f.s = &Scheduler{
		Client: c, Config: cfg,
		Defaults: DeviceDefaults{CPU: "100m", Memory: "100Mi"},
		Now:      func() time.Time { return f.now },
	}
	return f
}

func (f *schedFixture) create(o client.Object) {
	f.t.Helper()
	status := o.DeepCopyObject().(client.Object)
	if err := f.c.Create(context.Background(), o); err != nil {
		f.t.Fatal(err)
	}
	// The fake client drops the status of a created object when the type has a
	// status subresource: write it the way the API server would.
	switch s := status.(type) {
	case *laboratoryv1alpha1.Device:
		o.(*laboratoryv1alpha1.Device).Status = s.Status
	case *laboratoryv1alpha1.LabGroup:
		o.(*laboratoryv1alpha1.LabGroup).Status = s.Status
	case *laboratoryv1alpha1.Lab:
		o.(*laboratoryv1alpha1.Lab).Status = s.Status
	case *laboratoryv1alpha1.Tenant:
		o.(*laboratoryv1alpha1.Tenant).Status = s.Status
	}
	if err := f.c.Status().Update(context.Background(), o); err != nil {
		f.t.Fatal(err)
	}
}

// addLab creates a Lab in its own namespace "ns-<name>" with a Queued Device per
// device name; group and after are the deploy-group markers.
func (f *schedFixture) addLab(name, group string, after []string, devices ...string) {
	f.t.Helper()
	f.sec++
	lab := &laboratoryv1alpha1.Lab{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns-" + name, UID: types.UID("uid-" + name),
			CreationTimestamp: metav1.NewTime(planEpoch.Add(time.Duration(f.sec) * time.Second)),
			Labels:            map[string]string{}, Annotations: map[string]string{},
		},
	}
	if group != "" {
		lab.Labels[names.LabelDeployGroup] = group
	}
	if len(after) > 0 {
		lab.Annotations[names.AnnotationDeployAfter] = strings.Join(after, ",")
	}
	for _, d := range devices {
		lab.Spec.Devices = append(lab.Spec.Devices, laboratoryv1alpha1.DeviceTemplate{
			Name: d, Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "reg/" + name + "-" + d,
		})
	}
	f.create(lab)
	for _, d := range devices {
		now := metav1.NewTime(f.now)
		f.create(&laboratoryv1alpha1.Device{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-" + d, Namespace: lab.Namespace, UID: types.UID("uid-" + name + "-" + d)},
			Spec:       laboratoryv1alpha1.DeviceSpec{LabRef: name, Name: d, Type: laboratoryv1alpha1.DeviceTypeContainer, Image: "x"},
			Status: laboratoryv1alpha1.DeviceStatus{Scheduling: &laboratoryv1alpha1.PodSchedule{
				State: laboratoryv1alpha1.PodQueued, QueuedAt: &now,
			}},
		})
	}
}

func (f *schedFixture) tick() {
	f.t.Helper()
	f.now = f.now.Add(time.Second) // every pass is a second later: status writes are rate limited per object
	if err := f.s.tick(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *schedFixture) device(lab, dev string) *laboratoryv1alpha1.Device {
	f.t.Helper()
	var d laboratoryv1alpha1.Device
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "ns-" + lab, Name: lab + "-" + dev}, &d); err != nil {
		f.t.Fatal(err)
	}
	return &d
}

func (f *schedFixture) state(lab, dev string) laboratoryv1alpha1.PodScheduleState {
	return f.device(lab, dev).Status.Scheduling.State
}

func (f *schedFixture) ready(lab, dev string) {
	f.t.Helper()
	d := f.device(lab, dev)
	d.Status.Ready = true
	if err := f.c.Status().Update(context.Background(), d); err != nil {
		f.t.Fatal(err)
	}
}

func (f *schedFixture) labStatus(name string) *laboratoryv1alpha1.SchedulingStatus {
	f.t.Helper()
	var l laboratoryv1alpha1.Lab
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "ns-" + name, Name: name}, &l); err != nil {
		f.t.Fatal(err)
	}
	return l.Status.Scheduling
}

// states lists "lab/dev=S" for every device, sorted, for one comparison.
func (f *schedFixture) states() string {
	f.t.Helper()
	var list laboratoryv1alpha1.DeviceList
	if err := f.c.List(context.Background(), &list); err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, d := range list.Items {
		letter := map[laboratoryv1alpha1.PodScheduleState]string{
			qd: "Q", st: "S", sd: "D", fl: "F",
		}[d.Status.Scheduling.State]
		out = append(out, fmt.Sprintf("%s/%s=%s", d.Spec.LabRef, d.Spec.Name, letter))
	}
	return strings.Join(sortedStrings(out), " ")
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (f *schedFixture) wantStates(want string) {
	f.t.Helper()
	if got := f.states(); got != want {
		f.t.Fatalf("states\n got  %s\n want %s", got, want)
	}
}

// The worked example: three groups, one depending on another, an independent lab.
// Q = queued, S = starting (dispatched), D = started (ready once).
func TestSchedulerConveyorGroupsAndDependency(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	f.addLab("a", "g1", nil, "web", "db")
	f.addLab("b", "g1", nil, "web", "db")
	f.addLab("c", "g2", []string{"g1"}, "web")
	f.addLab("d", "g3", nil, "web")
	f.addLab("i", "", nil, "web")

	// Three slots: lab a completely, then lab b starts with one pod.
	f.tick()
	f.wantStates("a/db=S a/web=S b/db=S b/web=Q c/web=Q d/web=Q i/web=Q")
	f.tick()
	f.wantStates("a/db=S a/web=S b/db=S b/web=Q c/web=Q d/web=Q i/web=Q")

	// A pod gets Ready: its slot goes on with the same group, then the next group
	// that is allowed to start (g2 waits for g1, so g3).
	f.ready("a", "db")
	f.tick()
	f.wantStates("a/db=D a/web=S b/db=S b/web=S c/web=Q d/web=Q i/web=Q")
	f.ready("a", "web")
	f.ready("b", "db")
	f.tick()
	// g2 waits for g1, so the independent lab takes the free slot.
	f.wantStates("a/db=D a/web=D b/db=D b/web=S c/web=Q d/web=S i/web=S")

	// The dependency is reported while g1 is not complete.
	if s := f.labStatus("c"); s == nil || s.Reason != laboratoryv1alpha1.WaitForGroup || s.Message != "waiting for group g1" || s.Group != "g2" {
		t.Fatalf("c status = %+v", s)
	}
	f.ready("b", "web")
	f.ready("d", "web")
	f.tick()
	// g1 and g3 are complete: g2 goes.
	f.wantStates("a/db=D a/web=D b/db=D b/web=D c/web=S d/web=D i/web=S")
	f.ready("c", "web")
	f.ready("i", "web")
	f.tick()
	f.wantStates("a/db=D a/web=D b/db=D b/web=D c/web=D d/web=D i/web=D")
	if s := f.labStatus("c"); s == nil || s.Pending != 0 || s.Position != 0 || s.Reason != "" {
		t.Fatalf("a finished lab reports nothing waiting: %+v", s)
	}
}

func TestSchedulerQueueStatusOfWaitingLabs(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	f.addLab("a", "g1", nil, "p1", "p2", "p3", "p4")
	f.addLab("b", "g2", nil, "p1")
	f.tick()
	a, b := f.labStatus("a"), f.labStatus("b")
	if a.Position != 1 || a.Length != 2 || a.Pods != 4 || a.Pending != 1 || a.Reason != laboratoryv1alpha1.WaitInFlightLimit || a.Group != "g1" {
		t.Fatalf("a = %+v", a)
	}
	if b.Position != 2 || b.Length != 2 || b.Pending != 1 || b.Reason != laboratoryv1alpha1.WaitInFlightLimit {
		t.Fatalf("b = %+v", b)
	}
}

// A pod that is dispatched holds its slot even while the informer still shows it queued.
func TestSchedulerDoesNotDispatchTwiceFromAStaleCache(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	f.addLab("a", "g", nil, "p1", "p2", "p3", "p4")
	f.tick()
	for _, p := range []string{"p1", "p2", "p3"} {
		d := f.device("a", p)
		d.Status.Scheduling = &laboratoryv1alpha1.PodSchedule{State: laboratoryv1alpha1.PodQueued}
		if err := f.c.Status().Update(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	f.now = f.now.Add(2 * time.Second)
	f.tick()
	if got := f.state("a", "p4"); got != laboratoryv1alpha1.PodQueued {
		t.Fatalf("p4 = %s: the stale pods must still hold the slots", got)
	}
}

func TestSchedulerSuspendedGroupsAndDeletingLabsAreIgnored(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	f.addLab("a", "g", nil, "p1")
	f.addLab("b", "g", nil, "p1")
	f.create(&laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "ns-a", UID: "uid-grp-a"},
		Spec:       laboratoryv1alpha1.LabGroupSpec{Suspended: true},
	})
	f.tick()
	f.wantStates("a/p1=Q b/p1=S")
}

func TestSchedulerFailsAPodThatTookTooLong(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	f.addLab("a", "g1", nil, "p1")
	f.addLab("b", "g2", []string{"g1"}, "p1")
	f.tick()
	f.wantStates("a/p1=S b/p1=Q")
	f.now = f.now.Add(4 * time.Minute)
	f.tick()
	f.wantStates("a/p1=S b/p1=Q")
	// The timeout passes: the pod is failed, the group is complete, g2 goes on.
	f.now = f.now.Add(2 * time.Minute)
	f.tick()
	f.wantStates("a/p1=F b/p1=S")
	fail := f.device("a", "p1").Status.Scheduling.Failure
	if fail == nil || fail.Reason != laboratoryv1alpha1.FailureStartupTimeout || fail.Message != "no pod was created" {
		t.Fatalf("failure = %+v", fail)
	}
}

func (f *schedFixture) devicePod(lab, dev string, mutate func(p *corev1.Pod)) {
	f.t.Helper()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: lab + "-" + dev + "-1", Namespace: "ns-" + lab,
			Labels:            map[string]string{names.LabelLab: lab, names.LabelDevice: dev},
			CreationTimestamp: metav1.NewTime(f.now),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: dev}}},
	}
	mutate(p)
	status := p.Status
	if err := f.c.Create(context.Background(), p); err != nil {
		f.t.Fatal(err)
	}
	p.Status = status
	if err := f.c.Status().Update(context.Background(), p); err != nil {
		f.t.Fatal(err)
	}
}

func TestSchedulerReportsWhyAPodFailed(t *testing.T) {
	cases := []struct {
		name   string
		pod    func(p *corev1.Pod)
		after  time.Duration
		state  laboratoryv1alpha1.PodScheduleState
		reason string
		in     string
		count  int32
	}{
		{"image pull", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "ImagePullBackOff", Message: "Back-off pulling image reg/x:nope"}}}}
		}, 6 * time.Minute, laboratoryv1alpha1.PodFailed, laboratoryv1alpha1.FailureImagePull, "reg/x:nope", 0},
		{"image pull is not failed before the timeout", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}}}
		}, time.Minute, laboratoryv1alpha1.PodStarting, "", "", 0},
		{"crash loop reaches the threshold at once", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				RestartCount: 5,
				State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137, Reason: "OOMKilled"}},
			}}
		}, 30 * time.Second, laboratoryv1alpha1.PodFailed, laboratoryv1alpha1.FailureCrashLoop, "OOMKilled", 5},
		{"a few restarts are tolerated", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 4}}
		}, time.Minute, laboratoryv1alpha1.PodStarting, "", "", 0},
		{"unschedulable", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: "Unschedulable", Message: "0/3 nodes are available"}}
		}, 6 * time.Minute, laboratoryv1alpha1.PodFailed, laboratoryv1alpha1.FailureUnschedulable, "0/3 nodes", 0},
		{"running but never ready", func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning }, 6 * time.Minute,
			laboratoryv1alpha1.PodFailed, laboratoryv1alpha1.FailureStartupTimeout, "did not become Ready", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFixture(t, schedCfg())
			f.addLab("a", "g", nil, "p1")
			f.tick()
			f.devicePod("a", "p1", tc.pod)
			f.now = f.now.Add(tc.after)
			f.tick()
			d := f.device("a", "p1").Status.Scheduling
			if d.State != tc.state {
				t.Fatalf("state = %s, want %s (%+v)", d.State, tc.state, d.Failure)
			}
			if tc.state != laboratoryv1alpha1.PodFailed {
				return
			}
			if d.Failure.Reason != tc.reason || !strings.Contains(d.Failure.Message, tc.in) || d.Failure.RestartCount != tc.count || d.Failure.At == nil {
				t.Fatalf("failure = %+v", d.Failure)
			}
		})
	}
}

// A device whose pod is recreated by state persistence counts the recreations as restarts.
func TestSchedulerCountsRecreatedStatePodsAsRestarts(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	f.addLab("a", "g", nil, "p1")
	f.tick()
	d := f.device("a", "p1")
	d.Status.State = &laboratoryv1alpha1.DeviceStateStatus{Incarnation: 6}
	if err := f.c.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if got := f.device("a", "p1").Status.Scheduling; got.State != laboratoryv1alpha1.PodFailed || got.Failure.RestartCount != 5 {
		t.Fatalf("scheduling = %+v %+v", got, got.Failure)
	}
}

// A failed pod that starts after all is Started and its warning is gone; a pod
// that became Ready never holds a slot again, even when it restarts later.
func TestSchedulerFailedPodThatBecomesReady(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	f.addLab("a", "g", nil, "p1")
	f.tick()
	f.now = f.now.Add(6 * time.Minute)
	f.tick()
	f.wantStates("a/p1=F")
	f.ready("a", "p1")
	f.tick()
	f.wantStates("a/p1=D")
	if d := f.device("a", "p1").Status.Scheduling; d.Failure != nil || d.StartedAt == nil {
		t.Fatalf("scheduling = %+v", d)
	}
	d := f.device("a", "p1")
	d.Status.Ready = false
	if err := f.c.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	f.addLab("b", "g", nil, "p1", "p2", "p3", "p4")
	f.tick()
	f.wantStates("a/p1=D b/p1=S b/p2=S b/p3=S b/p4=Q")
}

func TestSchedulerPodsOfALabGroup(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	now := metav1.NewTime(f.now)
	queued := laboratoryv1alpha1.PodSchedule{State: laboratoryv1alpha1.PodQueued, QueuedAt: &now}
	f.create(&laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name: "team", UID: "uid-team", CreationTimestamp: metav1.NewTime(planEpoch),
			Labels: map[string]string{names.LabelDeployGroup: "g1"},
		},
		Status: laboratoryv1alpha1.LabGroupStatus{Pods: []laboratoryv1alpha1.NamedPodSchedule{
			{Name: "vpn", PodSchedule: queued}, {Name: "gateway", PodSchedule: queued},
		}},
	})
	f.addLab("a", "g1", nil, "web", "db")
	grp := func() map[string]laboratoryv1alpha1.PodScheduleState {
		var g laboratoryv1alpha1.LabGroup
		if err := f.c.Get(context.Background(), types.NamespacedName{Name: "team"}, &g); err != nil {
			t.Fatal(err)
		}
		out := map[string]laboratoryv1alpha1.PodScheduleState{}
		for _, p := range g.Status.Pods {
			out[p.Name] = p.State
		}
		return out
	}
	// Both group pods are dispatched (the group arrived first), then one lab pod.
	f.tick()
	if got := grp(); got["vpn"] != laboratoryv1alpha1.PodStarting || got["gateway"] != laboratoryv1alpha1.PodStarting {
		t.Fatalf("group pods = %v", got)
	}
	f.wantStates("a/db=S a/web=Q")

	// The VPN pod becomes Ready: it is Started and frees its slot.
	f.create(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vpn-x", Namespace: "team", Labels: map[string]string{"app": "vpn"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	})
	f.tick()
	if got := grp(); got["vpn"] != laboratoryv1alpha1.PodStarted || got["gateway"] != laboratoryv1alpha1.PodStarting {
		t.Fatalf("group pods = %v", got)
	}
	f.wantStates("a/db=S a/web=S")

	// The gateway never comes up: failed after the timeout.
	f.now = f.now.Add(6 * time.Minute)
	f.tick()
	if got := grp(); got["gateway"] != laboratoryv1alpha1.PodFailed {
		t.Fatalf("group pods = %v", got)
	}
}

func TestSchedulerLabGroupStatusAndPodCount(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	f.create(&laboratoryv1alpha1.LabGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "uid-team", CreationTimestamp: metav1.NewTime(planEpoch)},
		Spec:       laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Disabled: true}},
	})
	if got := groupPodNames(&laboratoryv1alpha1.LabGroup{Spec: laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Disabled: true}}}); len(got) != 1 || got[0] != "gateway" {
		t.Fatalf("pods of a group without VPN = %v", got)
	}
	// A group whose pods are not recorded yet is counted (2 pods) but not dispatched.
	f.tick()
	var g laboratoryv1alpha1.LabGroup
	if err := f.c.Get(context.Background(), types.NamespacedName{Name: "team"}, &g); err != nil {
		t.Fatal(err)
	}
	if g.Status.Scheduling == nil || g.Status.Scheduling.Pods != 1 || g.Status.Scheduling.Pending != 1 {
		t.Fatalf("status = %+v", g.Status.Scheduling)
	}
}

func node(name, cpu, mem string) *corev1.Node {
	n := testNode(name, cpu, mem)
	return &n
}

func TestSchedulerResourceCheckWaitsAndFails(t *testing.T) {
	cfg := schedCfg()
	cfg.MaxPods = 10
	cfg.ResourceCheck = true
	f := newSchedFixture(t, cfg)
	f.create(node("n1", "250m", "1Gi"))
	f.addLab("a", "g", nil, "p1", "p2", "p3")
	f.tick()
	// Each pod needs 100m: two fit in 250m, the third waits and says why.
	f.wantStates("a/p1=S a/p2=S a/p3=Q")
	if s := f.labStatus("a"); s.Reason != laboratoryv1alpha1.WaitInsufficient || s.Pending != 1 {
		t.Fatalf("status = %+v", s)
	}
	// The dispatched pods hold their requests until they are on a node.
	f.now = f.now.Add(time.Second)
	f.tick()
	f.wantStates("a/p1=S a/p2=S a/p3=Q")

	// A pod larger than the whole node is failed, and the queue goes on.
	f2 := newSchedFixture(t, cfg)
	f2.create(node("n1", "2", "4Gi"))
	f2.addLab("huge", "g", nil, "big")
	f2.addLab("ok", "g", nil, "p1")
	l := &laboratoryv1alpha1.Lab{}
	if err := f2.c.Get(context.Background(), types.NamespacedName{Namespace: "ns-huge", Name: "huge"}, l); err != nil {
		t.Fatal(err)
	}
	l.Spec.Devices[0].Resources = &laboratoryv1alpha1.DeviceResources{CPULimit: "64"}
	if err := f2.c.Update(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	f2.tick()
	f2.wantStates("huge/big=F ok/p1=S")
	if fail := f2.device("huge", "big").Status.Scheduling.Failure; fail == nil || fail.Reason != laboratoryv1alpha1.FailureDoesNotFit {
		t.Fatalf("failure = %+v", fail)
	}
}

func TestSchedulerNoNodesHoldsTheQueue(t *testing.T) {
	cfg := schedCfg()
	cfg.ResourceCheck = true
	f := newSchedFixture(t, cfg)
	f.addLab("a", "g", nil, "p1")
	f.tick()
	f.wantStates("a/p1=Q")
	if s := f.labStatus("a"); s.Reason != laboratoryv1alpha1.WaitNoSchedulableNodes {
		t.Fatalf("status = %+v", s)
	}
}

func TestSchedulerPrepullsGroupImagesBeforeItsFirstPod(t *testing.T) {
	cfg := schedCfg()
	cfg.MaxPods = 10
	cfg.Prepull = true
	cfg.PrepullTimeout = 5 * time.Minute
	f := newSchedFixture(t, cfg)
	f.s.ImagePullSecrets = []string{"regcred"}
	f.s.LabNodeSelector = map[string]string{"pool": "labs"}
	f.addLab("a", "g1", nil, "web")
	f.addLab("b", "g1", nil, "web")
	f.addLab("c", "g2", nil, "web")

	f.tick()
	f.wantStates("a/web=Q b/web=Q c/web=Q")
	if s := f.labStatus("a"); s.Reason != laboratoryv1alpha1.WaitPreparingImages {
		t.Fatalf("status = %+v", s)
	}
	ds := f.prepullDS("g/g1")
	if ds == nil {
		t.Fatal("prepull daemonset for g1 not created")
	}
	// One DaemonSet carries the images of every lab of the group.
	if got := len(ds.Spec.Template.Spec.Containers); got != 2 {
		t.Fatalf("images = %d", got)
	}
	if sp := ds.Spec.Template.Spec; len(sp.ImagePullSecrets) != 1 || sp.ImagePullSecrets[0].Name != "regcred" || sp.NodeSelector["pool"] != "labs" {
		t.Fatalf("pull secrets / selector not honoured: %+v", sp)
	}
	if f.prepullDS("g/g2") != nil {
		t.Fatal("g2 waits for its turn")
	}

	// No eligible node: done at once. Group g1 goes; g2 starts its prepull.
	f.stampDS(ds)
	ds.Status.ObservedGeneration = ds.Generation
	if err := f.c.Status().Update(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	f.tick()
	f.wantStates("a/web=S b/web=S c/web=Q")
	if f.prepullDS("g/g2") == nil {
		t.Fatal("g2 prepull expected")
	}
	// Its timeout passes: dispatch goes on, the daemonset is removed.
	f.ready("a", "web")
	f.ready("b", "web")
	f.stampDS(f.prepullDS("g/g2"))
	f.now = f.now.Add(6 * time.Minute)
	f.tick()
	f.wantStates("a/web=D b/web=D c/web=S")
	f.tick()
	if f.prepullDS("g/g2") != nil || f.prepullDS("g/g1") != nil {
		t.Fatal("prepull daemonsets must be removed afterwards")
	}
}

func (f *schedFixture) prepullDS(key string) *appsv1.DaemonSet {
	f.t.Helper()
	var ds appsv1.DaemonSet
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: names.SystemNamespace, Name: prepullName(key)}, &ds); err != nil {
		return nil
	}
	return &ds
}

// stampDS gives a prepull daemonset the creation time and generation the API
// server would set; the scheduler measures the prepull timeout from the former.
func (f *schedFixture) stampDS(ds *appsv1.DaemonSet) {
	f.t.Helper()
	ds.CreationTimestamp = metav1.NewTime(f.now)
	ds.Generation = 1
	if err := f.c.Update(context.Background(), ds); err != nil {
		f.t.Fatal(err)
	}
}

// Queue status writes are limited per tick, nearest the head first.
func TestSchedulerQueueStatusBudget(t *testing.T) {
	cfg := schedCfg()
	cfg.MaxPods = 1
	cfg.StatusBudget = 2
	f := newSchedFixture(t, cfg)
	for i := 1; i <= 5; i++ {
		f.addLab(fmt.Sprintf("l%d", i), fmt.Sprintf("g%d", i), nil, "p")
	}
	f.tick()
	written := func() int {
		n := 0
		for i := 1; i <= 5; i++ {
			if f.labStatus(fmt.Sprintf("l%d", i)) != nil {
				n++
			}
		}
		return n
	}
	if got := written(); got != 2 {
		t.Fatalf("written after one tick = %d, want the budget of 2", got)
	}
	f.tick()
	f.tick()
	if got := written(); got != 5 {
		t.Fatalf("written = %d", got)
	}
}

func TestSchedulerKeepsRunningWorkloadsOfOldGroupsUntouched(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	// A device that predates the scheduler is recorded Started by its reconciler.
	f.addLab("old", "g", nil, "p1")
	d := f.device("old", "p1")
	d.Status.Scheduling = &laboratoryv1alpha1.PodSchedule{State: laboratoryv1alpha1.PodStarted}
	if err := f.c.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	f.addLab("new", "g", nil, "p1", "p2", "p3", "p4")
	f.tick()
	f.wantStates("new/p1=S new/p2=S new/p3=S new/p4=Q old/p1=D")
}

func TestStartupVerdict(t *testing.T) {
	now := planEpoch
	if failed, _ := startupVerdict(now, now.Add(-time.Minute), nil, 0, 5*time.Minute, 5); failed {
		t.Fatal("not yet")
	}
	if failed, f := startupVerdict(now, now.Add(-time.Hour), nil, 0, 5*time.Minute, 5); !failed || f.Reason != laboratoryv1alpha1.FailureStartupTimeout {
		t.Fatalf("timeout: %v %+v", failed, f)
	}
	if failed, f := startupVerdict(now, now, nil, 5, 5*time.Minute, 5); !failed || f.Reason != laboratoryv1alpha1.FailureCrashLoop {
		t.Fatalf("restarts: %v %+v", failed, f)
	}
	// The newest pod decides.
	old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}}}}}
	newer := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now)}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if _, f := startupVerdict(now, now.Add(-time.Hour), []*corev1.Pod{old, newer}, 0, time.Minute, 5); f.Reason != laboratoryv1alpha1.FailureStartupTimeout {
		t.Fatalf("reason = %s", f.Reason)
	}
	long := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
		Reason: "ErrImagePull", Message: strings.Repeat("x", 2000)}}}}}}
	if _, f := startupVerdict(now, now.Add(-time.Hour), []*corev1.Pod{long}, 0, time.Minute, 5); len(f.Message) > 500 {
		t.Fatalf("message length %d", len(f.Message))
	}
}

func TestDeployAfterParsing(t *testing.T) {
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{names.AnnotationDeployAfter: " a, b ,,c "}}}
	if got := deployAfter(lab); strings.Join(got, "|") != "a|b|c" {
		t.Fatalf("after = %v", got)
	}
	if got := deployAfter(&laboratoryv1alpha1.Lab{}); len(got) != 0 {
		t.Fatalf("after = %v", got)
	}
}

func TestTopologyClassIgnoresNames(t *testing.T) {
	mk := func(img string) *laboratoryv1alpha1.Lab {
		return &laboratoryv1alpha1.Lab{Spec: laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{{Name: "web", Type: laboratoryv1alpha1.DeviceTypeContainer, Image: img}}}}
	}
	a, b := mk("x:1"), mk("x:1")
	a.Name, b.Name = "one", "two"
	if topologyClass(a) != topologyClass(b) || topologyClass(a) == topologyClass(mk("x:2")) {
		t.Fatal("class must follow the topology and images only")
	}
}

var _ = resource.MustParse

// The group reconciler records its pods: Queued when the Deployment is missing,
// Started when it exists (a group that predates the scheduler), and holds back
// only the queued ones.
func TestEnsureGroupScheduling(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	lg := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "uid-team"}}
	f.create(lg)
	f.create(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "team"}})
	r := &LabGroupReconciler{Client: f.c, Scheme: f.c.Scheme(), Scheduled: true}
	if err := r.ensureGroupScheduling(context.Background(), lg); err != nil {
		t.Fatal(err)
	}
	got := map[string]laboratoryv1alpha1.PodScheduleState{}
	for _, p := range lg.Status.Pods {
		got[p.Name] = p.State
	}
	if len(got) != 2 || got["vpn"] != laboratoryv1alpha1.PodQueued || got["gateway"] != laboratoryv1alpha1.PodStarted {
		t.Fatalf("pods = %v", got)
	}
	if !r.groupPodQueued(lg, "vpn") || r.groupPodQueued(lg, "gateway") || r.groupPodQueued(lg, "other") {
		t.Fatal("only the queued pod is held back")
	}
	// Saved, and not rewritten the second time.
	var stored laboratoryv1alpha1.LabGroup
	if err := f.c.Get(context.Background(), types.NamespacedName{Name: "team"}, &stored); err != nil || len(stored.Status.Pods) != 2 {
		t.Fatalf("stored: %v %+v", err, stored.Status.Pods)
	}
	r.Scheduled = false
	if r.groupPodQueued(lg, "vpn") {
		t.Fatal("a scheduler that is off holds nothing back")
	}
	r.Scheduled = true
	// A group with VPN disabled has the gateway only.
	lg2 := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "novpn", UID: "uid-novpn"},
		Spec: laboratoryv1alpha1.LabGroupSpec{VPN: laboratoryv1alpha1.LabGroupVPNSpec{Disabled: true}}}
	f.create(lg2)
	if err := r.ensureGroupScheduling(context.Background(), lg2); err != nil || len(lg2.Status.Pods) != 1 || lg2.Status.Pods[0].Name != "gateway" {
		t.Fatalf("no-VPN group: %v %+v", err, lg2.Status.Pods)
	}
}

// A tenant's quota caps the requests of its dispatched pods; its next pod waits with the
// reason TenantQuota while another tenant's labs go on.
func TestSchedulerEnforcesTenantQuota(t *testing.T) {
	cfg := schedCfg()
	cfg.MaxPods = 0 // no in-flight limit: only the quota holds pods back
	f := newSchedFixture(t, cfg)
	f.create(&laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "capped"},
		Spec: laboratoryv1alpha1.TenantSpec{Quota: &laboratoryv1alpha1.TenantQuota{CPU: "250m"}}}) // two pods of 100m
	label := func(lab, tenant string) {
		var l laboratoryv1alpha1.Lab
		if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "ns-" + lab, Name: lab}, &l); err != nil {
			t.Fatal(err)
		}
		if l.Labels == nil {
			l.Labels = map[string]string{}
		}
		l.Labels[names.LabelTenant] = tenant
		if err := f.c.Update(context.Background(), &l); err != nil {
			t.Fatal(err)
		}
	}
	f.addLab("a", "", nil, "web", "db", "cache")
	label("a", "capped")
	f.addLab("b", "", nil, "web")
	label("b", "other")

	f.tick()
	f.wantStates("a/cache=S a/db=S a/web=Q b/web=S")
	if st := f.labStatus("a"); st == nil || st.Reason != laboratoryv1alpha1.WaitTenantQuota || st.Pending != 1 {
		t.Fatalf("status of the held lab: %+v", st)
	}
	// Pods that started (or failed) still count against the quota.
	f.ready("a", "cache")
	f.tick()
	f.wantStates("a/cache=D a/db=S a/web=Q b/web=S")
}
