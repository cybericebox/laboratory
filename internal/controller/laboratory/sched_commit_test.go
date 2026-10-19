package laboratory

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// onNode gives the device a running pod on the node.
func (f *schedFixture) onNode(lab, dev, node string) {
	f.t.Helper()
	f.devicePod(lab, dev, func(p *corev1.Pod) {
		p.Spec.NodeName = node
		p.Status.Phase = corev1.PodRunning
	})
}

// noNode gives the device a pod no node takes: Pending, PodScheduled=False.
func (f *schedFixture) noNode(lab, dev string) {
	f.t.Helper()
	f.devicePod(lab, dev, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodPending
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/1 nodes are available: 1 Insufficient cpu."}}
	})
}

// dropPods deletes the pods of the lab, as the device reconciler does for a withdrawn device.
func (f *schedFixture) dropPods(lab string) {
	f.t.Helper()
	if err := f.c.DeleteAllOf(context.Background(), &corev1.Pod{}, client.InNamespace("ns-"+lab)); err != nil {
		f.t.Fatal(err)
	}
}

// metricValue reads a gauge or counter without labels from the operator's registry.
func metricValue(t *testing.T, name string) float64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range families {
		if mf.GetName() != name || len(mf.GetMetric()) == 0 {
			continue
		}
		m := mf.GetMetric()[0]
		if m.GetCounter() != nil {
			return m.GetCounter().GetValue()
		}
		return m.GetGauge().GetValue()
	}
	t.Fatalf("no metric %s", name)
	return 0
}

func resourceQ(s string) resource.Quantity { return resource.MustParse(s) }

// at sets the clock so that the next tick runs at t.
func (f *schedFixture) at(t time.Time) { f.now = t.Add(-time.Second) }

func commitObj(id, group string, ps ...*schedPod) *schedObject {
	return &schedObject{id: id, group: group, pods: ps}
}

func sentAt(state laboratoryv1alpha1.PodScheduleState, at time.Time) *schedPod {
	return &schedPod{key: at.String() + string(state), state: state, dispatchedAt: at}
}

// The deadline counts from the last dispatch of the gang, and only a pod with no node makes it due.
func TestCommitDueDeadlineFromLastDispatch(t *testing.T) {
	t0 := planEpoch
	waiting := sentAt(st, t0)
	late := sentAt(sd, t0.Add(5*time.Minute))
	objs := []*schedObject{commitObj("lab/ns/a", "g", waiting), commitObj("lab/ns/b", "g", late), commitObj("lab/ns/c", "", sentAt(st, t0))}
	nodeless := map[*schedPod]bool{waiting: true}
	if due := commitDue(objs, nodeless, 15*time.Minute, t0.Add(19*time.Minute)); len(due) != 0 {
		t.Fatalf("due before last dispatch + timeout: %+v", due)
	}
	due := commitDue(objs, nodeless, 15*time.Minute, t0.Add(20*time.Minute))
	if len(due) != 1 || len(due[0].objs) != 2 || due[0].attempt != 1 || !due[0].deadline.Equal(t0.Add(20*time.Minute)) {
		t.Fatalf("due = %+v", due)
	}
}

func TestWithdrawBackoff(t *testing.T) {
	for n, want := range map[int32]time.Duration{0: time.Minute, 1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 4: 8 * time.Minute, 5: 15 * time.Minute, 30: 15 * time.Minute} {
		if got := withdrawBackoff(n, 15*time.Minute); got != want {
			t.Errorf("backoff(%d) = %s, want %s", n, got, want)
		}
	}
}

// Three pods run, one has no node at the deadline: all four go back to the queue, with the attempt and the retry time.
func TestWithdrawWholeGang(t *testing.T) {
	f := maxFixture(t, 2)
	f.create(node("n1", "2", "4Gi"))
	f.addLab("a", "g", nil, "p1", "p2", "p3", "p4")
	f.tick()
	sent := f.now
	f.wantStates("a/p1=S a/p2=S a/p3=S a/p4=S")
	for _, d := range []string{"p1", "p2", "p3"} {
		f.onNode("a", d, "n1")
		f.ready("a", d)
	}
	f.noNode("a", "p4")
	f.tick()
	f.wantStates("a/p1=D a/p2=D a/p3=D a/p4=S")
	before := metricValue(t, "laboratory_gang_withdrawals_total")
	f.at(sent.Add(15*time.Minute - time.Second))
	f.tick()
	f.wantStates("a/p1=D a/p2=D a/p3=D a/p4=S")
	f.at(sent.Add(15 * time.Minute))
	f.tick()
	f.wantStates("a/p1=Q a/p2=Q a/p3=Q a/p4=Q")
	for _, d := range []string{"p1", "p2", "p3", "p4"} {
		sc := f.device("a", d).Status.Scheduling
		if sc.Withdrawals != 1 || sc.RetryAfter == nil || !sc.RetryAfter.Time.Equal(f.now.Add(time.Minute)) || sc.DispatchedAt != nil || sc.StartedAt != nil {
			t.Fatalf("%s: %+v", d, sc)
		}
	}
	if got := metricValue(t, "laboratory_gang_withdrawals_total") - before; got != 1 {
		t.Fatalf("withdrawals counted %v", got)
	}
	if s := f.labStatus("a"); s.Reason != laboratoryv1alpha1.WaitWithdrawn || !strings.Contains(s.Message, "attempt 1") || !strings.Contains(s.Message, "15m0s") ||
		!strings.Contains(s.Message, "the scheduler said: Unschedulable: 0/1 nodes are available: 1 Insufficient cpu.") {
		t.Fatalf("status = %+v", s)
	}
	// every record of the commit carries what the scheduler said, the running ones too
	for _, d := range []string{"p1", "p4"} {
		if m := f.device("a", d).Status.Scheduling.SchedulerMessage; !strings.Contains(m, "Insufficient cpu") {
			t.Fatalf("%s scheduler message = %q", d, m)
		}
	}
}

// A pod that got a node and then fails is today's per-pod failure: the gang is not withdrawn.
func TestWithdrawOnlyForNoNode(t *testing.T) {
	f := maxFixture(t, 0)
	f.create(node("n1", "8", "16Gi"))
	f.addLab("a", "g", nil, "p1", "p2")
	f.tick()
	f.onNode("a", "p2", "n1")
	f.ready("a", "p2")
	f.devicePod("a", "p1", func(p *corev1.Pod) {
		p.Spec.NodeName = "n1"
		p.Status.Phase = corev1.PodPending
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
	})
	f.now = f.now.Add(20 * time.Minute)
	f.tick()
	f.tick()
	f.wantStates("a/p1=F a/p2=D")
	if sc := f.device("a", "p1").Status.Scheduling; sc.Withdrawals != 0 || sc.Failure.Reason != laboratoryv1alpha1.FailureImagePull {
		t.Fatalf("p1 = %+v %+v", sc, sc.Failure)
	}
}

// A withdrawn gang waits for its retry time, keeping its place; then it is sent again.
func TestRetryAfterHoldsGang(t *testing.T) {
	f := maxFixture(t, 1)
	f.create(node("n1", "8", "16Gi"))
	f.addLab("a", "g", nil, "p1")
	f.tick()
	sent := f.now
	f.noNode("a", "p1")
	f.at(sent.Add(15 * time.Minute))
	f.tick()
	withdrawn := f.now
	f.wantStates("a/p1=Q")
	f.dropPods("a")
	f.at(withdrawn.Add(time.Minute - time.Second))
	f.tick()
	f.wantStates("a/p1=Q")
	if s := f.labStatus("a"); s.Reason != laboratoryv1alpha1.WaitWithdrawn {
		t.Fatalf("status = %+v", s)
	}
	f.at(withdrawn.Add(time.Minute))
	f.tick()
	f.wantStates("a/p1=S")
	if sc := f.device("a", "p1").Status.Scheduling; sc.Withdrawals != 1 || sc.RetryAfter != nil || sc.DispatchedAt == nil {
		t.Fatalf("scheduling = %+v", sc)
	}
}

// The quota and the room of a withdrawn gang are free at once: another gang of the tenant is admitted in the same pass.
func TestWithdrawFreesQuotaAndReservation(t *testing.T) {
	f := maxFixture(t, 1)
	f.create(node("n1", "8", "16Gi"))
	f.create(&laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "capped"},
		Spec: laboratoryv1alpha1.TenantSpec{Quota: &laboratoryv1alpha1.TenantQuota{CPU: "3"}}})
	f.create(&laboratoryv1alpha1.Reservation{ObjectMeta: metav1.ObjectMeta{Name: "r1"}, Spec: laboratoryv1alpha1.ReservationSpec{
		Tenant: "capped", From: metav1.NewTime(planEpoch), To: metav1.NewTime(planEpoch.Add(48 * time.Hour)),
		Resources: corev1.ResourceList{corev1.ResourceCPU: resourceQ("3"), corev1.ResourceMemory: resourceQ("16Gi")}}})
	label := func(lab string) {
		var l laboratoryv1alpha1.Lab
		if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "ns-" + lab, Name: lab}, &l); err != nil {
			t.Fatal(err)
		}
		l.Labels[names.LabelTenant] = "capped"
		l.Labels[names.LabelReservation] = "r1"
		if err := f.c.Update(context.Background(), &l); err != nil {
			t.Fatal(err)
		}
	}
	f.addLab("a", "g1", nil, "p1", "p2", "p3")
	label("a")
	f.tick()
	sent := f.now
	f.addLab("b", "g2", nil, "p1", "p2")
	label("b")
	f.tick()
	f.wantStates("a/p1=S a/p2=S a/p3=S b/p1=Q b/p2=Q")
	f.onNode("a", "p1", "n1")
	f.onNode("a", "p2", "n1")
	f.noNode("a", "p3")
	f.at(sent.Add(15 * time.Minute))
	f.tick()
	f.wantStates("a/p1=Q a/p2=Q a/p3=Q b/p1=S b/p2=S")
}

// Once a pod has a node, the startup timeout counts from when it got it, not from the dispatch.
func TestStartupClockFromScheduled(t *testing.T) {
	f := maxFixture(t, 0)
	f.create(node("n1", "8", "16Gi"))
	f.addLab("a", "g", nil, "p1")
	f.tick()
	sent := f.now
	f.devicePod("a", "p1", func(p *corev1.Pod) {
		p.Spec.NodeName = "n1"
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(sent.Add(4 * time.Minute))}}
	})
	f.at(sent.Add(8 * time.Minute))
	f.tick()
	f.wantStates("a/p1=S")
	f.at(sent.Add(9 * time.Minute))
	f.tick()
	f.wantStates("a/p1=F")
}

// A pod that waits for a node is never failed by the startup timeout: the gang commit decides at the node timeout.
func TestNodelessNotFailedByStartupTimeout(t *testing.T) {
	f := maxFixture(t, 2)
	f.addLab("a", "g", nil, "p1")
	f.tick()
	f.noNode("a", "p1")
	f.now = f.now.Add(10 * time.Minute)
	f.tick()
	f.wantStates("a/p1=S")
}

// While sent pods wait for a node the object says so, the gauge counts them, and the reason goes when they have one.
func TestStatusNodeStarting(t *testing.T) {
	f := maxFixture(t, 2)
	f.addLab("a", "g", nil, "p1", "p2")
	f.tick()
	sent := f.now
	f.noNode("a", "p1")
	f.noNode("a", "p2")
	f.tick()
	s := f.labStatus("a")
	if s.Reason != laboratoryv1alpha1.WaitNodeStarting || !strings.Contains(s.Message, "2 pods wait for a node since "+sent.UTC().Format(time.RFC3339)) {
		t.Fatalf("status = %+v", s)
	}
	if got := metricValue(t, "laboratory_pods_waiting_for_node"); got != 2 {
		t.Fatalf("gauge = %v", got)
	}
	if !f.s.nodeStartingTold["uid-a"] {
		t.Fatal("the NodeStarting event is given once")
	}
	f.dropPods("a")
	f.onNode("a", "p1", "n9")
	f.onNode("a", "p2", "n9")
	f.tick()
	if s := f.labStatus("a"); s.Reason != "" || metricValue(t, "laboratory_pods_waiting_for_node") != 0 {
		t.Fatalf("status = %+v, gauge %v", s, metricValue(t, "laboratory_pods_waiting_for_node"))
	}
}

// withdrawnFixture is a cluster of one lab node of 2 CPU and a maximum of 2 nodes whose lab a, of 3 pods, was just withdrawn
// (the pods of the node still to come never got a node); its pods are gone. It returns when.
func withdrawnFixture(t *testing.T) (*schedFixture, time.Time) {
	f := maxFixture(t, 2)
	f.create(node("n1", "2", "4Gi"))
	f.addLab("a", "g1", nil, "p1", "p2", "p3")
	f.tick()
	sent := f.now
	f.onNode("a", "p1", "n1")
	f.onNode("a", "p2", "n1")
	f.noNode("a", "p3")
	f.at(sent.Add(15 * time.Minute))
	f.tick()
	f.wantStates("a/p1=Q a/p2=Q a/p3=Q")
	f.dropPods("a")
	return f, f.now
}

// After a withdrawal no new node came: admission uses the current nodes only, so a gang that needs a node still to come
// waits, while one that fits the current nodes goes.
func TestBreakerAfterWithdrawal(t *testing.T) {
	f, _ := withdrawnFixture(t)
	f.addLab("b", "g2", nil, "p1", "p2", "p3")
	f.addLab("c", "g3", nil, "p1")
	f.tick()
	f.wantStates("a/p1=Q a/p2=Q a/p3=Q b/p1=Q b/p2=Q b/p3=Q c/p1=S")
	if s := f.labStatus("b"); s.Reason != laboratoryv1alpha1.WaitInsufficient {
		t.Fatalf("b status = %+v", s)
	}
	if metricValue(t, "laboratory_capacity_breaker") != 1 {
		t.Fatal("the breaker gauge is 1")
	}
}

// The breaker opens when a new lab node becomes schedulable, or one node timeout after the withdrawal.
func TestBreakerOpens(t *testing.T) {
	f, _ := withdrawnFixture(t)
	f.addLab("b", "g2", nil, "p1", "p2", "p3")
	f.tick()
	f.wantStates("a/p1=Q a/p2=Q a/p3=Q b/p1=Q b/p2=Q b/p3=Q")
	f.create(node("n2", "2", "4Gi"))
	f.tick()
	if f.state("b", "p1") != st || metricValue(t, "laboratory_capacity_breaker") != 0 {
		t.Fatalf("a new node opens the breaker: %s, gauge %v", f.states(), metricValue(t, "laboratory_capacity_breaker"))
	}

	f2, withdrawn := withdrawnFixture(t)
	f2.addLab("b", "g2", nil, "p1", "p2", "p3")
	f2.at(withdrawn.Add(15*time.Minute - time.Second))
	f2.tick()
	if f2.state("b", "p1") != qd {
		t.Fatalf("still closed: %s", f2.states())
	}
	f2.at(withdrawn.Add(15 * time.Minute))
	f2.tick()
	if f2.state("b", "p1") != st && f2.state("a", "p1") != st {
		t.Fatalf("one timeout later the maximum is back: %s", f2.states())
	}
}

// A lab that joins a deploy group long after the group started is its own commit: its withdrawal leaves the running labs alone.
func TestOldLabsOfGroupNotWithdrawn(t *testing.T) {
	f := maxFixture(t, 1)
	f.create(node("n1", "8", "16Gi"))
	f.addLab("a", "g", nil, "p1")
	f.tick()
	f.onNode("a", "p1", "n1")
	f.ready("a", "p1")
	f.now = f.now.Add(time.Hour)
	f.addLab("b", "g", nil, "p1")
	f.tick()
	f.tick()
	sent := f.now
	f.wantStates("a/p1=D b/p1=S")
	f.noNode("b", "p1")
	f.at(sent.Add(15 * time.Minute))
	f.tick()
	f.wantStates("a/p1=D b/p1=Q")
}

// A pod that was Ready once and lost its node later never makes its gang due.
func TestStartedPodWithoutNodeNotWithdrawn(t *testing.T) {
	f := maxFixture(t, 0)
	f.create(node("n1", "8", "16Gi"))
	f.addLab("a", "g", nil, "p1")
	f.tick()
	f.onNode("a", "p1", "n1")
	f.ready("a", "p1")
	f.tick()
	f.dropPods("a")
	f.noNode("a", "p1")
	f.now = f.now.Add(time.Hour)
	f.tick()
	f.wantStates("a/p1=D")
}

// After three withdrawals in a row the tenant shows a Warning condition naming the group; the backoff grows.
func TestRepeatedWithdrawalsWarnTenant(t *testing.T) {
	f := maxFixture(t, 1)
	f.create(node("n1", "8", "16Gi"))
	f.create(&laboratoryv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: names.DefaultTenant}})
	f.addLab("a", "", nil, "p1")
	for attempt := int32(1); attempt <= 3; attempt++ {
		f.tick()
		f.wantStates("a/p1=S")
		sent := f.now
		f.noNode("a", "p1")
		f.at(sent.Add(15 * time.Minute))
		f.tick()
		f.wantStates("a/p1=Q")
		sc := f.device("a", "p1").Status.Scheduling
		if sc.Withdrawals != attempt || !sc.RetryAfter.Time.Equal(f.now.Add(withdrawBackoff(attempt, 15*time.Minute))) {
			t.Fatalf("attempt %d: %+v", attempt, sc)
		}
		f.dropPods("a")
		f.at(sc.RetryAfter.Time)
	}
	f.tick()
	var ten laboratoryv1alpha1.Tenant
	if err := f.c.Get(context.Background(), types.NamespacedName{Name: names.DefaultTenant}, &ten); err != nil {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(ten.Status.Conditions, laboratoryv1alpha1.TenantConditionGroupsWithdrawn)
	if c == nil || c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "lab ns-a/a (attempt 3)") {
		t.Fatalf("condition = %+v", c)
	}
	// it goes once the group starts
	f.onNode("a", "p1", "n1")
	f.ready("a", "p1")
	f.tick()
	f.tick()
	if err := f.c.Get(context.Background(), types.NamespacedName{Name: names.DefaultTenant}, &ten); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(ten.Status.Conditions, laboratoryv1alpha1.TenantConditionGroupsWithdrawn); c != nil {
		t.Fatalf("condition stays: %+v", c)
	}
}

// The VPN and gateway records of a withdrawn LabGroup delete their workloads; a Queued record that was never withdrawn (a
// group stopped and started) keeps its retained workload.
func TestGroupPodsScaledDownWhenQueued(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	ns := "lg-x"
	for _, n := range []string{names.ComponentVPN, names.ComponentGateway} {
		f.createPlain(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: ns}})
	}
	r := &LabGroupReconciler{Client: f.c, Scheduled: true}
	lg := &laboratoryv1alpha1.LabGroup{Status: laboratoryv1alpha1.LabGroupStatus{Pods: []laboratoryv1alpha1.NamedPodSchedule{
		{Name: names.ComponentVPN, PodSchedule: laboratoryv1alpha1.PodSchedule{State: qd, Withdrawals: 1}},
		{Name: names.ComponentGateway, PodSchedule: laboratoryv1alpha1.PodSchedule{State: qd}},
	}}}
	if err := r.deleteWithdrawnGroupPods(context.Background(), lg, ns); err != nil {
		t.Fatal(err)
	}
	var dep appsv1.Deployment
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: names.ComponentVPN}, &dep); err == nil {
		t.Fatal("the withdrawn VPN workload is deleted")
	}
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: names.ComponentGateway}, &dep); err != nil {
		t.Fatalf("the gateway that was never withdrawn stays: %v", err)
	}
}

// Without a budget no node will come: a pod without a node fails after the startup timeout as before (Unschedulable, with what
// the scheduler said), and its gang is not withdrawn.
func TestNoBudgetUnschedulableFailsAsBefore(t *testing.T) {
	f := maxFixture(t, 0)
	f.create(node("n1", "8", "16Gi"))
	f.addLab("a", "g", nil, "p1")
	f.tick()
	f.noNode("a", "p1")
	f.now = f.now.Add(6 * time.Minute)
	f.tick()
	f.wantStates("a/p1=F")
	if fail := f.device("a", "p1").Status.Scheduling.Failure; fail == nil || fail.Reason != laboratoryv1alpha1.FailureUnschedulable || !strings.Contains(fail.Message, "Insufficient cpu") {
		t.Fatalf("failure = %+v", fail)
	}
	if s := f.labStatus("a"); s.Reason == laboratoryv1alpha1.WaitNodeStarting {
		t.Fatalf("no node is coming without a budget: %+v", s)
	}
}

// With a budget, a pod that has no node for a reason a new node does not fix (a taint, a selector, an extended resource) fails
// after the startup timeout as before; it never makes its gang wait for a node.
func TestNonResourceReasonFailsAsBefore(t *testing.T) {
	f := maxFixture(t, 2)
	f.addLab("a", "g", nil, "p1")
	f.tick()
	f.devicePod("a", "p1", func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodPending
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
			Message: "0/1 nodes are available: 1 Insufficient cybericebox.com/tun, 1 node(s) had untolerated taint {x: y}."}}
	})
	f.now = f.now.Add(6 * time.Minute)
	f.tick()
	f.wantStates("a/p1=F")
	if fail := f.device("a", "p1").Status.Scheduling.Failure; fail == nil || fail.Reason != laboratoryv1alpha1.FailureUnschedulable || !strings.Contains(fail.Message, "untolerated taint") {
		t.Fatalf("failure = %+v", fail)
	}
}

// A pod that waits for a node holds no in-flight slot: a gang larger than maxPods goes out in passes of seconds, not one
// node start per slice.
func TestWaitingPodsHoldNoSlot(t *testing.T) {
	f := maxFixture(t, 10)
	f.s.Config.MaxPods = 3
	f.addLab("a", "g", nil, "p1", "p2", "p3", "p4", "p5", "p6")
	f.tick()
	f.wantStates("a/p1=S a/p2=S a/p3=S a/p4=Q a/p5=Q a/p6=Q")
	for _, d := range []string{"p1", "p2", "p3"} {
		f.noNode("a", d)
	}
	f.tick()
	f.wantStates("a/p1=S a/p2=S a/p3=S a/p4=S a/p5=S a/p6=S")
}

// A lab that joins its deploy group after the node timeout of a stuck commit neither delays that commit's withdrawal nor is
// withdrawn with it.
func TestLateJoinerDoesNotDelayCommit(t *testing.T) {
	t0 := planEpoch
	stuck := sentAt(st, t0)
	joiner := sentAt(st, t0.Add(16*time.Minute))
	objs := []*schedObject{commitObj("lab/ns/a", "g", stuck), commitObj("lab/ns/b", "g", joiner)}
	due := commitDue(objs, map[*schedPod]bool{stuck: true}, 15*time.Minute, t0.Add(17*time.Minute))
	if len(due) != 1 || len(due[0].objs) != 1 || due[0].objs[0].id != "lab/ns/a" || !due[0].deadline.Equal(t0.Add(15*time.Minute)) {
		t.Fatalf("due = %+v", due)
	}
}

// A commit withdrawn already (a write failed, some records are left) is withdrawn again with the same attempt, not the next.
func TestWithdrawAgainKeepsAttempt(t *testing.T) {
	t0 := planEpoch
	left := sentAt(st, t0)
	left.withdrawals = 1
	done := &schedPod{key: "done", state: qd, withdrawals: 2, retryAfter: t0.Add(17 * time.Minute)}
	due := commitDue([]*schedObject{commitObj("lab/ns/a", "", left, done)}, map[*schedPod]bool{left: true}, 15*time.Minute, t0.Add(16*time.Minute))
	if len(due) != 1 || due[0].attempt != 2 || !due[0].again {
		t.Fatalf("due = %+v", due)
	}
}
