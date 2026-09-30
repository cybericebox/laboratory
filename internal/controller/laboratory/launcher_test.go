package laboratory

import (
	"context"
	"fmt"
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

type launchFixture struct {
	t   *testing.T
	c   client.Client
	l   *Launcher
	now time.Time
}

func newLaunchFixture(t *testing.T, cfg LaunchConfig, objs ...client.Object) *launchFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := laboratoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&laboratoryv1alpha1.Lab{}).
		WithObjects(objs...).Build()
	f := &launchFixture{t: t, c: c, now: testEpoch.Add(time.Hour)}
	f.l = &Launcher{
		Client:   c,
		Config:   cfg,
		Defaults: DeviceDefaults{CPU: "100m", Memory: "100Mi"},
		Now:      func() time.Time { return f.now },
	}
	return f
}

func (f *launchFixture) tick() {
	f.t.Helper()
	if err := f.l.tick(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *launchFixture) lab(name string) *laboratoryv1alpha1.Lab {
	f.t.Helper()
	var l laboratoryv1alpha1.Lab
	if err := f.c.Get(context.Background(), types.NamespacedName{Namespace: "team-" + name, Name: name}, &l); err != nil {
		f.t.Fatal(err)
	}
	return &l
}

func (f *launchFixture) phase(name string) laboratoryv1alpha1.Phase { return f.lab(name).Status.Phase }

// setPhase simulates the lab reconciler moving a lab on.
func (f *launchFixture) setPhase(name string, ph laboratoryv1alpha1.Phase) {
	f.t.Helper()
	l := f.lab(name)
	l.Status.Phase = ph
	if err := f.c.Status().Update(context.Background(), l); err != nil {
		f.t.Fatal(err)
	}
}

func (f *launchFixture) admitted() []string {
	var out []string
	var list laboratoryv1alpha1.LabList
	if err := f.c.List(context.Background(), &list); err != nil {
		f.t.Fatal(err)
	}
	for i := range list.Items {
		if labAdmitted(&list.Items[i]) {
			out = append(out, list.Items[i].Name)
		}
	}
	return sortedStrings(out)
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

func labObjs(labs ...*laboratoryv1alpha1.Lab) []client.Object {
	var out []client.Object
	for _, l := range labs {
		out = append(out, l)
	}
	return out
}

// team namespaces in these tests are "team-<name>" (see queuedLab).
func teamLab(name, class string, sec int) *laboratoryv1alpha1.Lab {
	l := queuedLab(name, class, "reg/"+class, sec)
	l.Namespace = "team-" + name
	return l
}

func baseCfg() LaunchConfig {
	return LaunchConfig{MaxInFlight: 2, WaveTimeout: 3 * time.Minute}
}

func TestLauncherAdmitsUpToMaxInFlightInOrder(t *testing.T) {
	f := newLaunchFixture(t, baseCfg(), labObjs(
		teamLab("b1", "B", 1), teamLab("a1", "A", 2), teamLab("a2", "A", 3), teamLab("a3", "A", 4),
	)...)
	f.tick()
	// The class of the oldest lab (B) goes first, then class A in creation order.
	if got := f.admitted(); !equalStrings(got, []string{"a1", "b1"}) {
		t.Fatalf("admitted = %v, want b1 (class B) then a1 (class A)", got)
	}
	for _, n := range []string{"a2", "a3"} {
		l := f.lab(n)
		if l.Status.Phase != laboratoryv1alpha1.PhaseQueued || l.Status.Launch == nil {
			t.Fatalf("%s: %+v", n, l.Status)
		}
	}
	a2, a3 := f.lab("a2").Status.Launch, f.lab("a3").Status.Launch
	if a2.Position != 1 || a2.Length != 2 || a3.Position != 2 || a3.Length != 2 {
		t.Fatalf("positions: a2 %+v a3 %+v", a2, a3)
	}
	if a2.Reason != laboratoryv1alpha1.LaunchReasonInFlightLimit || a2.Class != "A" {
		t.Fatalf("reason/class: %+v", a2)
	}
	if ready := f.lab("a2").Status.Conditions; len(ready) != 1 || ready[0].Reason != "Queued" {
		t.Fatalf("conditions: %+v", ready)
	}
	adm := f.lab("b1").Status
	if adm.Phase != laboratoryv1alpha1.PhaseProvisioning || adm.Launch.AdmittedAt == nil || adm.Launch.Position != 0 || adm.Launch.Reason != "" {
		t.Fatalf("admitted status: %+v", adm)
	}
}

// Nothing more is admitted while the slots are taken; a Ready lab frees its
// slot, and so does an expired wave timeout.
func TestLauncherSlotsFreeOnReadyAndTimeout(t *testing.T) {
	f := newLaunchFixture(t, baseCfg(), labObjs(
		teamLab("l1", "A", 1), teamLab("l2", "A", 2), teamLab("l3", "A", 3), teamLab("l4", "A", 4), teamLab("l5", "A", 5),
	)...)
	f.tick()
	f.tick()
	if got := f.admitted(); !equalStrings(got, []string{"l1", "l2"}) {
		t.Fatalf("admitted = %v", got)
	}

	f.now = f.now.Add(10 * time.Second)
	f.setPhase("l1", laboratoryv1alpha1.PhaseReady)
	f.tick()
	if got := f.admitted(); !equalStrings(got, []string{"l1", "l2", "l3"}) {
		t.Fatalf("after one Ready: admitted = %v", got)
	}

	// l2 and l3 still provision; once the wave timeout passes they release their slots.
	f.now = f.now.Add(3 * time.Minute)
	f.tick()
	if got := f.admitted(); !equalStrings(got, []string{"l1", "l2", "l3", "l4", "l5"}) {
		t.Fatalf("after the wave timeout: admitted = %v", got)
	}
}

// A lab admitted in the last tick still looks queued in a lagging informer
// cache; it must neither be admitted again nor free its slot.
func TestLauncherDoesNotReadmitFromStaleCache(t *testing.T) {
	f := newLaunchFixture(t, baseCfg(), labObjs(teamLab("l1", "A", 1), teamLab("l2", "A", 2), teamLab("l3", "A", 3))...)
	f.tick()
	// Simulate the stale cache: the admission is forgotten by the reader.
	for _, n := range []string{"l1", "l2"} {
		l := f.lab(n)
		l.Status = laboratoryv1alpha1.LabStatus{}
		if err := f.c.Status().Update(context.Background(), l); err != nil {
			t.Fatal(err)
		}
	}
	f.now = f.now.Add(2 * time.Second)
	f.tick()
	if phase := f.phase("l3"); phase != laboratoryv1alpha1.PhaseQueued {
		t.Fatalf("l3 must stay queued, phase %q", phase)
	}
}

func TestLauncherIgnoresLegacyAndDeletingLabs(t *testing.T) {
	legacy := teamLab("old", "A", 0)
	legacy.Status.Phase = laboratoryv1alpha1.PhaseReady
	del := teamLab("gone", "A", 1)
	del.Finalizers = []string{"x"}
	f := newLaunchFixture(t, baseCfg(), labObjs(legacy, del, teamLab("new", "A", 2))...)
	if err := f.c.Delete(context.Background(), del); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if phase := f.phase("old"); phase != laboratoryv1alpha1.PhaseReady {
		t.Fatalf("legacy lab touched: %q", phase)
	}
	if l := f.lab("gone"); l.Status.Phase != "" {
		t.Fatalf("a lab being deleted must not be queued: %q", l.Status.Phase)
	}
	if got := f.admitted(); !equalStrings(got, []string{"new", "old"}) {
		t.Fatalf("admitted = %v", got)
	}
}

func TestLauncherNoLimitAdmitsEverything(t *testing.T) {
	cfg := baseCfg()
	cfg.MaxInFlight = 0
	f := newLaunchFixture(t, cfg, labObjs(teamLab("a", "A", 1), teamLab("b", "B", 2), teamLab("c", "C", 3))...)
	f.tick()
	if got := f.admitted(); len(got) != 3 {
		t.Fatalf("admitted = %v", got)
	}
}

func node(name, cpu, mem string) *corev1.Node {
	n := testNode(name, cpu, mem)
	return &n
}

func TestLauncherResourceCheckWaitsAndReports(t *testing.T) {
	cfg := baseCfg()
	cfg.MaxInFlight = 10
	cfg.ResourceCheck = true
	cfg.HeadroomPercent = 0
	// Every lab needs 100m/100Mi (defaults); the node allows two of them.
	n := node("n1", "250m", "1Gi")
	f := newLaunchFixture(t, cfg, append(labObjs(teamLab("l1", "A", 1), teamLab("l2", "A", 2), teamLab("l3", "A", 3)), n)...)
	f.tick()
	if got := f.admitted(); !equalStrings(got, []string{"l1", "l2"}) {
		t.Fatalf("admitted = %v", got)
	}
	l3 := f.lab("l3")
	if l3.Status.Phase != laboratoryv1alpha1.PhaseQueued || l3.Status.Launch.Reason != laboratoryv1alpha1.LaunchReasonInsufficientResources ||
		l3.Status.Launch.Position != 1 || l3.Status.Launch.Length != 1 {
		t.Fatalf("l3: %+v %+v", l3.Status, l3.Status.Launch)
	}

	// The two admitted labs have no pods yet: their needs stay reserved, so l3 keeps waiting.
	f.now = f.now.Add(5 * time.Second)
	f.tick()
	if got := f.admitted(); len(got) != 2 {
		t.Fatalf("reservation lost, admitted = %v", got)
	}

	// Their pods are now scheduled and counted as requests, not reserved twice.
	for _, name := range []string{"l1", "l2"} {
		p := testPod("n1", "100m", "100Mi")
		p.Name, p.Namespace = "dev-"+name, "team-"+name
		p.Labels = map[string]string{names.LabelLab: name}
		if err := f.c.Create(context.Background(), &p); err != nil {
			t.Fatal(err)
		}
	}
	f.now = f.now.Add(5 * time.Second)
	f.tick()
	if got := f.admitted(); len(got) != 2 {
		t.Fatalf("a scheduled pod must not be counted twice and must not free room: %v", got)
	}

	// More room: the node grows.
	var cur corev1.Node
	if err := f.c.Get(context.Background(), types.NamespacedName{Name: "n1"}, &cur); err != nil {
		t.Fatal(err)
	}
	cur.Status.Allocatable[corev1.ResourceCPU] = resource.MustParse("1")
	if err := f.c.Status().Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(5 * time.Second)
	f.tick()
	if got := f.admitted(); !equalStrings(got, []string{"l1", "l2", "l3"}) {
		t.Fatalf("admitted = %v", got)
	}
}

func TestLauncherHeadroomKeepsShareFree(t *testing.T) {
	cfg := baseCfg()
	cfg.MaxInFlight = 10
	cfg.ResourceCheck = true
	cfg.HeadroomPercent = 50
	n := node("n1", "400m", "1Gi")
	f := newLaunchFixture(t, cfg, append(labObjs(teamLab("l1", "A", 1), teamLab("l2", "A", 2), teamLab("l3", "A", 3)), n)...)
	f.tick()
	// 400m with 50% headroom leaves 200m to use: two labs of 100m.
	if got := f.admitted(); !equalStrings(got, []string{"l1", "l2"}) {
		t.Fatalf("admitted = %v", got)
	}
}

func TestLauncherNoNodesHoldsTheQueue(t *testing.T) {
	cfg := baseCfg()
	cfg.ResourceCheck = true
	f := newLaunchFixture(t, cfg, labObjs(teamLab("l1", "A", 1))...)
	f.tick()
	if len(f.admitted()) != 0 {
		t.Fatal("no schedulable node: nothing may be admitted")
	}
	if r := f.lab("l1").Status.Launch.Reason; r != laboratoryv1alpha1.LaunchReasonNoSchedulableNodes {
		t.Fatalf("reason = %q", r)
	}
}

// A lab larger than the whole cluster steps aside instead of blocking the queue.
func TestLauncherOversizedLabDoesNotBlockOthers(t *testing.T) {
	cfg := baseCfg()
	cfg.MaxInFlight = 10
	cfg.ResourceCheck = true
	huge := teamLab("huge", "A", 1)
	huge.Spec.Devices[0].Resources = &laboratoryv1alpha1.DeviceResources{CPULimit: "64"}
	f := newLaunchFixture(t, cfg, append(labObjs(huge, teamLab("small", "A", 2)), node("n1", "2", "4Gi"))...)
	f.tick()
	if got := f.admitted(); !equalStrings(got, []string{"small"}) {
		t.Fatalf("admitted = %v", got)
	}
	h := f.lab("huge").Status.Launch
	if h.Reason != laboratoryv1alpha1.LaunchReasonInsufficientResources || h.Position != 1 {
		t.Fatalf("huge: %+v", h)
	}
}

func prepullCfg() LaunchConfig {
	cfg := baseCfg()
	cfg.Prepull = true
	cfg.PrepullTimeout = 5 * time.Minute
	return cfg
}

// stampDS gives the prepull daemonset the creation time and generation the API
// server would set; the launcher measures the prepull timeout from the former.
func (f *launchFixture) stampDS(class string) {
	f.t.Helper()
	ds, ok := f.prepullDS(class)
	if !ok {
		f.t.Fatalf("no prepull daemonset for %s", class)
	}
	ds.CreationTimestamp = metav1.NewTime(f.now)
	ds.Generation = 1
	if err := f.c.Update(context.Background(), ds); err != nil {
		f.t.Fatal(err)
	}
}

func (f *launchFixture) prepullDS(class string) (*appsv1.DaemonSet, bool) {
	f.t.Helper()
	var ds appsv1.DaemonSet
	err := f.c.Get(context.Background(), types.NamespacedName{Namespace: names.SystemNamespace, Name: prepullName(class)}, &ds)
	return &ds, err == nil
}

func TestLauncherPrepullBlocksUntilDone(t *testing.T) {
	f := newLaunchFixture(t, prepullCfg(), labObjs(teamLab("a1", "A", 1), teamLab("a2", "A", 2))...)
	f.l.ImagePullSecrets = []string{"regcred"}
	f.l.LabNodeSelector = map[string]string{"pool": "labs"}

	f.tick()
	if len(f.admitted()) != 0 {
		t.Fatal("nothing may be admitted before the images are pulled")
	}
	if r := f.lab("a1").Status.Launch.Reason; r != laboratoryv1alpha1.LaunchReasonPreparingImages {
		t.Fatalf("reason = %q", r)
	}
	f.stampDS("A")
	ds, ok := f.prepullDS("A")
	if !ok {
		t.Fatal("prepull daemonset not created")
	}
	spec := ds.Spec.Template.Spec
	if len(spec.Containers) != 1 || spec.Containers[0].Image != "reg/A" {
		t.Fatalf("containers: %+v", spec.Containers)
	}
	if len(spec.ImagePullSecrets) != 1 || spec.ImagePullSecrets[0].Name != "regcred" || spec.NodeSelector["pool"] != "labs" {
		t.Fatalf("pull secrets / selector not honoured: %+v", spec)
	}

	// The DaemonSet controller reports one scheduled pod; its image is not pulled yet.
	ds.Status.ObservedGeneration = ds.Generation
	ds.Status.DesiredNumberScheduled = 1
	if err := f.c.Status().Update(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "prepull-x", Namespace: names.SystemNamespace, Labels: ds.Spec.Template.Labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "i0", Image: "reg/A"}}},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "i0", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}},
	}
	if err := f.c.Create(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(10 * time.Second)
	f.tick()
	if len(f.admitted()) != 0 {
		t.Fatal("still pulling")
	}

	pod.Status.ContainerStatuses[0].ImageID = "reg/A@sha256:abc"
	if err := f.c.Status().Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(10 * time.Second)
	f.tick()
	if got := f.admitted(); !equalStrings(got, []string{"a1", "a2"}) {
		t.Fatalf("admitted = %v", got)
	}
	f.tick()
	if _, ok := f.prepullDS("A"); ok {
		t.Fatal("the prepull daemonset must be removed afterwards")
	}
}

func TestLauncherPrepullTimeoutAdmitsAnyway(t *testing.T) {
	f := newLaunchFixture(t, prepullCfg(), labObjs(teamLab("a1", "A", 1))...)
	f.tick()
	if len(f.admitted()) != 0 {
		t.Fatal("blocked while pulling")
	}
	f.stampDS("A")
	f.now = f.now.Add(6 * time.Minute)
	f.tick()
	if got := f.admitted(); !equalStrings(got, []string{"a1"}) {
		t.Fatalf("admitted = %v", got)
	}
	f.tick()
	if _, ok := f.prepullDS("A"); ok {
		t.Fatal("the prepull daemonset must be removed after the timeout")
	}
}

// A class is prepared once; the next class starts its own prepull only after the
// first is admitted, and the old daemonset is gone by then.
func TestLauncherPrepullsClassesOneAfterAnother(t *testing.T) {
	cfg := prepullCfg()
	cfg.MaxInFlight = 10
	f := newLaunchFixture(t, cfg, labObjs(teamLab("a1", "A", 1), teamLab("b1", "B", 2))...)
	f.tick()
	if _, ok := f.prepullDS("A"); !ok {
		t.Fatal("class A prepull expected first")
	}
	if _, ok := f.prepullDS("B"); ok {
		t.Fatal("class B must wait for its turn")
	}
	// No eligible node: the daemonset is done at once (desired 0).
	f.stampDS("A")
	ds, _ := f.prepullDS("A")
	ds.Status.ObservedGeneration = ds.Generation
	if err := f.c.Status().Update(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if got := f.admitted(); !equalStrings(got, []string{"a1"}) {
		t.Fatalf("admitted = %v", got)
	}
	if _, ok := f.prepullDS("B"); !ok {
		t.Fatal("class B prepull expected after A was admitted")
	}
	if _, ok := f.prepullDS("A"); ok {
		t.Fatal("class A daemonset must be gone")
	}
}

func TestLauncherPrepullSkippedWhenDisabled(t *testing.T) {
	f := newLaunchFixture(t, baseCfg(), labObjs(teamLab("a1", "A", 1))...)
	f.tick()
	if _, ok := f.prepullDS("A"); ok {
		t.Fatal("prepull is off")
	}
	if len(f.admitted()) != 1 {
		t.Fatal("lab must be admitted")
	}
}

func TestPrepullProgress(t *testing.T) {
	ds := &appsv1.DaemonSet{}
	ds.Generation = 1
	if _, _, done := prepullProgress(ds, nil); done {
		t.Fatal("status not observed yet: not done")
	}
	ds.Status.ObservedGeneration = 1
	if _, desired, done := prepullProgress(ds, nil); !done || desired != 0 {
		t.Fatal("no eligible node: done")
	}
	ds.Status.DesiredNumberScheduled = 2
	pod := func(ready bool) corev1.Pod {
		st := corev1.ContainerStatus{Name: "i0"}
		if ready {
			st.State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 127}
		}
		return corev1.Pod{
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "i0"}}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{st}},
		}
	}
	pulled, desired, done := prepullProgress(ds, []corev1.Pod{pod(true), pod(false)})
	if pulled != 1 || desired != 2 || done {
		t.Fatalf("pulled %d desired %d done %v", pulled, desired, done)
	}
	if _, _, done := prepullProgress(ds, []corev1.Pod{pod(true), pod(true)}); !done {
		t.Fatal("all pulled: done")
	}
	// A pod with no container status yet has pulled nothing.
	if pulled, _, _ := prepullProgress(ds, []corev1.Pod{{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "i0"}}}}}); pulled != 0 {
		t.Fatal("pod without statuses")
	}
}

func TestBuildPrepullDaemonSetCapsImages(t *testing.T) {
	var images []string
	for i := 0; i < maxPrepullImages+10; i++ {
		images = append(images, fmt.Sprintf("img-%d", i))
	}
	ds := buildPrepullDaemonSet("ns", "cls", images, nil, nil, nil)
	if len(ds.Spec.Template.Spec.Containers) != maxPrepullImages {
		t.Fatalf("containers = %d", len(ds.Spec.Template.Spec.Containers))
	}
	if ds.Spec.Template.Spec.AutomountServiceAccountToken == nil || *ds.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("prepull pods need no service account token")
	}
	if ds.Labels[managedByLabel] != managedByValue {
		t.Fatal("managed-by label missing")
	}
}

// Queue status writes are limited per tick, the lab nearest the head first, and
// an unchanged position is not written again.
func TestLauncherQueueStatusBudgetAndInterval(t *testing.T) {
	cfg := baseCfg()
	cfg.MaxInFlight = 1
	cfg.StatusBudget = 2
	cfg.StatusInterval = time.Minute
	var labs []*laboratoryv1alpha1.Lab
	for i := 1; i <= 5; i++ {
		labs = append(labs, teamLab(fmt.Sprintf("l%d", i), "A", i))
	}
	f := newLaunchFixture(t, cfg, labObjs(labs...)...)
	f.tick() // l1 admitted; budget 2: l2 and l3 marked
	queuedCount := func() int {
		n := 0
		for i := 2; i <= 5; i++ {
			if f.phase(fmt.Sprintf("l%d", i)) == laboratoryv1alpha1.PhaseQueued {
				n++
			}
		}
		return n
	}
	if got := queuedCount(); got != 2 {
		t.Fatalf("marked after one tick = %d, want the budget of 2", got)
	}
	if f.phase("l2") != laboratoryv1alpha1.PhaseQueued || f.phase("l3") != laboratoryv1alpha1.PhaseQueued {
		t.Fatal("the labs nearest the head are marked first")
	}
	f.tick()
	if got := queuedCount(); got != 4 {
		t.Fatalf("marked after two ticks = %d", got)
	}
	if p := f.lab("l5").Status.Launch; p == nil || p.Position != 4 || p.Length != 4 {
		t.Fatalf("l5 launch: %+v", p)
	}
}
