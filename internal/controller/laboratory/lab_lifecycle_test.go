package laboratory

import (
	"context"
	"encoding/json"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/devicestate"
	"github.com/cybericebox/laboratory/internal/names"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func lifecycleFixture(t *testing.T, mode string) (*LabReconciler, *lab.Lab, *lab.Device, client.Client) {
	t.Helper()
	scheme := pruneScheme(t)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g", UID: "lab-a", Generation: 3}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: mode}, Devices: []lab.DeviceTemplate{{Name: "web", Type: lab.DeviceTypeContainer, Image: "base"}}}}
	d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "a-web", Namespace: "g", UID: "device-a", Labels: map[string]string{names.LabelLab: "a"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: lab.SchemeGroupVersion.String(), Kind: "Lab", Name: "a", UID: l.UID}}}, Spec: lab.DeviceSpec{LabRef: "a", Name: "web", Type: lab.DeviceTypeContainer, Code: "fixed", Image: "base"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(l, d, &lab.LabVPN{}, &lab.LabGroupAccessPolicy{}, &lab.LabTrafficReport{}, &appsv1.Deployment{}).WithObjects(l, d).Build()
	return &LabReconciler{Client: c, Reader: c, Scheme: scheme}, l, d, c
}
func TestLifecycleSkipStopsOwnedRuntimeAndNeverRecreates(t *testing.T) {
	r, l, d, c := lifecycleFixture(t, "Skip")
	ctx := context.Background()
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: workloadName(d), Namespace: "g", OwnerReferences: []metav1.OwnerReference{{Kind: "Device", UID: d.UID, Name: d.Name}}}, Spec: appsv1.DeploymentSpec{Replicas: ptrInt32(1)}}
	sibling := dep.DeepCopy()
	sibling.Name = "sibling"
	sibling.OwnerReferences = nil
	for _, o := range []client.Object{dep, sibling} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(l)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(dep), dep); err != nil {
		t.Fatal(err)
	}
	if *dep.Spec.Replicas != 0 {
		t.Fatal("stopped deployment stayed running")
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(sibling), sibling)
	if *sibling.Spec.Replicas != 1 {
		t.Fatal("sibling stopped")
	}
	dr := &DeviceReconciler{Client: c, Reader: c, Scheme: r.Scheme}
	for i := 0; i < 3; i++ {
		if _, err := dr.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(d)}); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(dep), dep)
	if *dep.Spec.Replicas != 0 {
		t.Fatal("ordinary reconcile resurrected stopped deployment")
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(l), l)
	if l.Status.Lifecycle == nil || l.Status.Lifecycle.ObservedState == "Stopped" {
		t.Fatal("API scaling alone certified physical stop")
	}
	if l.Status.Resources != nil && l.Status.Resources.RuntimeState == "Released" {
		t.Fatal("API scaling certified release")
	}
}
func TestLifecycleRequiredRejectsNonpersistentAndPreservesRuntime(t *testing.T) {
	r, l, d, c := lifecycleFixture(t, "Required")
	ctx := context.Background()
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: workloadName(d), Namespace: "g", OwnerReferences: []metav1.OwnerReference{{Kind: "Device", UID: d.UID, Name: d.Name}}}, Spec: appsv1.DeploymentSpec{Replicas: ptrInt32(1)}}
	_ = c.Create(ctx, dep)
	handled, _, err := r.reconcileLifecycle(ctx, l)
	if err != nil || !handled {
		t.Fatal(handled, err)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(l), l)
	if l.Status.Lifecycle == nil || l.Status.Lifecycle.ObservedState != "StopFailed" {
		t.Fatal("unsupported required policy not rejected", l.Status.Lifecycle)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(dep), dep)
	if *dep.Spec.Replicas != 1 {
		t.Fatal("failed barrier stopped runtime")
	}
}
func TestLifecycleStoppedMissingPodCannotResurrectOrCertifyRelease(t *testing.T) {
	r, l, d, c := lifecycleFixture(t, "Skip")
	ctx := context.Background()
	d.Spec.State = &lab.DeviceStateSpec{Enabled: true}
	d.Status.State = &lab.DeviceStateStatus{Incarnation: 4, Image: "latest"}
	d.Status.NodeName = "lost-node"
	d.Status.PodName = "vanished"
	_ = c.Update(ctx, d)
	_ = c.Status().Update(ctx, d)
	dr := &DeviceReconciler{Client: c, Reader: c, Scheme: r.Scheme}
	for i := 0; i < 3; i++ {
		if _, err := dr.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(d)}); err != nil {
			t.Fatal(err)
		}
	}
	var pods corev1.PodList
	_ = c.List(ctx, &pods)
	if len(pods.Items) != 0 {
		t.Fatal("missing stopped pod recreated")
	}
	if _, _, err := r.reconcileLifecycle(ctx, l); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(l), l)
	if l.Status.Lifecycle.ObservedState != "Unknown" {
		t.Fatal("missing API pod treated as known absence", l.Status.Lifecycle)
	}
}
func TestStoppedSchedulingExcludesOnlyTargetLab(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	f.addLab("a", "", nil, "web")
	f.addLab("b", "", nil, "web")
	var target lab.Lab
	ctx := context.Background()
	_ = f.c.Get(ctx, types.NamespacedName{Name: "a", Namespace: "ns-a"}, &target)
	target.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Skip"}
	_ = f.c.Update(ctx, &target)
	snap, err := f.s.load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	objects := f.s.objects(snap, f.now)
	for _, o := range objects {
		if o.id == "lab/ns-a/a" {
			t.Fatal("stopped lab remained dispatchable")
		}
	}
	found := false
	for _, o := range objects {
		found = found || o.id == "lab/ns-b/b"
	}
	if !found {
		t.Fatal("sibling disappeared")
	}
}

func requiredReadyFixture(t *testing.T) (*LabReconciler, *lab.Lab, *lab.Device, client.Client, *corev1.Pod) {
	r, l, d, c := lifecycleFixture(t, "Required")
	ctx := context.Background()
	r.RequiredSnapshotAvailable = true
	l.Spec.Devices[0].Persistence = &lab.DevicePersistence{Enabled: true}
	if err := c.Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	d.Spec.State = &lab.DeviceStateSpec{Enabled: true, CaptureRequest: &lab.DeviceCaptureRequest{OperationID: "op", LifecycleRevision: 1, PodUID: "pod-a", PodResourceVersion: "audit-old-rv", Epoch: 1, Incarnation: 2, DeadlineSeconds: 300, CommitNodeAgentEpoch: "node-boot"}}
	d.Status.State = &lab.DeviceStateStatus{Epoch: 1, Incarnation: 2, Image: "snapshot", Capture: &lab.DeviceCaptureResult{OperationID: "op", LifecycleRevision: 1, PodUID: "pod-a", PodResourceVersion: "audit-old-rv", Epoch: 1, Incarnation: 2, NodeAgentEpoch: "node-boot", Result: "Succeeded", Quiesced: true, GuardState: "Held", Committed: true, Image: "snapshot"}}
	savedState := d.Status
	if err := c.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	d.Status = savedState
	if err := c.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d.Status.State.Capture)
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a-pod", Namespace: "g", UID: "pod-a", OwnerReferences: []metav1.OwnerReference{{Kind: "Device", Name: d.Name, UID: d.UID}}, Annotations: map[string]string{names.AnnotationStateEpoch: "1", names.AnnotationStateIncarnation: "2", devicestate.CaptureGuardAnnotation: string(raw)}}, Spec: corev1.PodSpec{NodeName: "node"}}
	if err := c.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	return r, l, d, c, p
}
func TestSnapshotBarrierCurrentGuardDeletesUsingCurrentPodRV(t *testing.T) {
	r, l, d, c, p := requiredReadyFixture(t)
	ctx := context.Background()
	// An unrelated current metadata update deliberately differs from capture RV.
	p.Labels = map[string]string{"unrelated": "current"}
	if err := c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.reconcileLifecycle(ctx, l); err != nil {
		t.Fatal(err)
	}
	var got corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(p), &got); err == nil {
		t.Fatal("current held pod was not deleted")
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(d), d)
	if d.Status.State.Image != "snapshot" || d.Spec.Code != "fixed" {
		t.Fatal("stop discarded retained identity/snapshot")
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(l), l)
	if !l.Status.Lifecycle.SnapshotComplete || l.Status.Lifecycle.ObservedState != "Unknown" {
		t.Fatal(l.Status.Lifecycle)
	}
}
func TestSnapshotBarrierFailureDeletesNoneIncludingSuccessfulDevice(t *testing.T) {
	r, l, d, c, p := requiredReadyFixture(t)
	ctx := context.Background()
	second := d.DeepCopy()
	second.Name = "a-db"
	second.UID = "device-db"
	second.Spec.Name = "db"
	second.ResourceVersion = ""
	second.Spec.State.CaptureRequest.PodUID = "pod-db"
	second.Status.State.Capture.PodUID = "pod-db"
	second.Status.State.Capture.Result = "Failed"
	second.Status.State.Capture.Committed = false
	second.Status.State.Capture.Quiesced = false
	second.Status.State.Capture.GuardState = "Invalidated"
	second.Status.State.Capture.Error = "upload failed"
	savedState := second.Status
	if err := c.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	second.Status = savedState
	if err := c.Status().Update(ctx, second); err != nil {
		t.Fatal(err)
	}
	failedPod := p.DeepCopy()
	failedPod.Name = "db-pod"
	failedPod.UID = "pod-db"
	failedPod.ResourceVersion = ""
	failedPod.OwnerReferences = []metav1.OwnerReference{{Kind: "Device", Name: second.Name, UID: second.UID}}
	if err := c.Create(ctx, failedPod); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.reconcileLifecycle(ctx, l); err != nil {
		t.Fatal(err)
	}
	for _, pod := range []*corev1.Pod{p, failedPod} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil || pod.DeletionTimestamp != nil {
			t.Fatal("one runtime was deleted despite failed required barrier", err)
		}
	}
	for _, device := range []*lab.Device{d, second} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(device), device); err != nil {
			t.Fatal(err)
		}
		if device.Spec.State.CaptureRequest != nil {
			t.Fatal("collective failure did not cancel held capture request")
		}
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(l), l)
	if l.Status.Lifecycle.ObservedState != "StopFailed" {
		t.Fatal(l.Status.Lifecycle)
	}
}
func TestSnapshotBarrierInvalidatedOrWrongBootGuardPreservesRuntime(t *testing.T) {
	for _, mutate := range []func(*corev1.Pod, *lab.Device){
		func(p *corev1.Pod, d *lab.Device) {
			var g lab.DeviceCaptureResult
			_ = json.Unmarshal([]byte(p.Annotations[devicestate.CaptureGuardAnnotation]), &g)
			g.GuardState = "Invalidated"
			raw, _ := json.Marshal(g)
			p.Annotations[devicestate.CaptureGuardAnnotation] = string(raw)
		},
		func(p *corev1.Pod, d *lab.Device) { d.Status.State.Capture.NodeAgentEpoch = "another-boot" },
	} {
		r, l, d, c, p := requiredReadyFixture(t)
		mutate(p, d)
		_ = c.Update(context.Background(), p)
		_ = c.Status().Update(context.Background(), d)
		if _, _, err := r.reconcileLifecycle(context.Background(), l); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(p), p); err != nil {
			t.Fatal("invalidated runtime was deleted", err)
		}
	}
}
func TestCurrentAccessFenceRejectsOldBootOperationGenerationAndGroup(t *testing.T) {
	r, l, _, c := lifecycleFixture(t, "Skip")
	ctx := context.Background()
	l.Spec.VPN.Enabled = true
	_ = c.Update(ctx, l)
	group := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "group-uid"}, Status: lab.LabGroupStatus{Namespace: "g"}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "g", Labels: map[string]string{names.LabelGroup: "team"}}}
	boot := lab.VPNRuntimeIdentity{BootID: "boot1", PodName: "vpn-pod", PodUID: "vpn-pod-uid", ContainerID: "containerd://1", RestartCount: 1}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: boot.PodName, Namespace: "g", UID: types.UID(boot.PodUID), Labels: map[string]string{names.LabelComponent: names.ComponentVPN}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "vpn", ContainerID: boot.ContainerID, RestartCount: 1, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	leg := &lab.LabVPN{ObjectMeta: metav1.ObjectMeta{Name: names.LabVPNObjectName(l.Name), Namespace: "g", OwnerReferences: []metav1.OwnerReference{{Kind: "Lab", UID: l.UID}}}, Status: lab.LabVPNStatus{Runtime: &boot, AccessFence: &lab.LabAccessFence{OperationID: "op", Revision: 1, LabUID: string(l.UID), ObservedGeneration: 3, GroupUID: "group-uid", VPNRuntimeIdentity: boot, FencedAt: metav1.Now()}}}
	witness := &lab.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: "g"}, Spec: lab.LabTrafficReportSpec{Kind: lab.LabTrafficSurfaceVPN, Instance: boot.PodName}, Status: lab.LabTrafficReportStatus{CurrentVPNRuntime: &lab.VPNBootRecord{VPNRuntimeIdentity: boot, GroupUID: "group-uid", PublishedAt: metav1.Now()}}}
	savedWitnessStatus := witness.Status
	if err := c.Create(ctx, witness); err != nil {
		t.Fatal(err)
	}
	witness.Status = savedWitnessStatus
	if err := c.Status().Update(ctx, witness); err != nil {
		t.Fatal(err)
	}
	savedLegStatus := leg.Status
	for _, o := range []client.Object{group, ns, p, leg} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	leg.Status = savedLegStatus
	_ = c.Status().Update(ctx, leg)
	if ok, _, _, err := r.currentAccessFence(ctx, l); err != nil || !ok {
		t.Fatal("fresh current fence refused", err)
	}
	for _, mutate := range []func(*lab.LabAccessFence){func(f *lab.LabAccessFence) { f.Revision++ }, func(f *lab.LabAccessFence) { f.OperationID = "old" }, func(f *lab.LabAccessFence) { f.ObservedGeneration-- }, func(f *lab.LabAccessFence) { f.GroupUID = "another-team" }, func(f *lab.LabAccessFence) { f.BootID = "old-boot" }, func(f *lab.LabAccessFence) { f.PodUID = "old-pod" }, func(f *lab.LabAccessFence) { f.RestartCount = 0 }} {
		fresh := leg.DeepCopy()
		mutate(leg.Status.AccessFence)
		_ = c.Status().Update(ctx, leg)
		if ok, _, _, err := r.currentAccessFence(ctx, l); err != nil || ok {
			t.Fatal("stale physical certificate accepted", leg.Status.AccessFence, err)
		}
		fresh.ResourceVersion = leg.ResourceVersion
		leg = fresh
		if err := c.Status().Update(ctx, leg); err != nil {
			t.Fatal(err)
		}
	}
	// Same Pod/container replacement and a replacement Pod are separately stale.
	for _, mutate := range []func(*corev1.Pod){func(p *corev1.Pod) {
		p.Status.ContainerStatuses[0].ContainerID = "containerd://2"
		p.Status.ContainerStatuses[0].RestartCount++
	}, func(p *corev1.Pod) { p.UID = "replacement-pod" }, func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "" }} {
		orig := p.DeepCopy()
		mutate(p)
		saved := p.Status
		if err := c.Update(ctx, p); err != nil {
			t.Fatal(err)
		}
		p.Status = saved
		if err := c.Status().Update(ctx, p); err != nil {
			t.Fatal(err)
		}
		if ok, _, _, err := r.currentAccessFence(ctx, l); err != nil || ok {
			t.Fatal("restarted or unknown current VPN pod accepted", err)
		}
		orig.ResourceVersion = p.ResourceVersion
		p = orig
		saved = p.Status
		if err := c.Update(ctx, p); err != nil {
			t.Fatal(err)
		}
		p.Status = saved
		if err := c.Status().Update(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
}
func TestLifecycleQueuedAndSwitchOnlyKeepHonestUnknown(t *testing.T) {
	for _, switchOnly := range []bool{false, true} {
		r, l, d, c := lifecycleFixture(t, "Skip")
		if switchOnly {
			d.Spec.Type = lab.DeviceTypeUnmanagedSwitch
			l.Spec.Devices[0].Type = lab.DeviceTypeUnmanagedSwitch
			_ = c.Update(context.Background(), d)
			_ = c.Update(context.Background(), l)
		}
		if _, _, err := r.reconcileLifecycle(context.Background(), l); err != nil {
			t.Fatal(err)
		}
		var pods corev1.PodList
		_ = c.List(context.Background(), &pods)
		if len(pods.Items) != 0 {
			t.Fatal("stop materialized queued runtime")
		}
		_ = c.Get(context.Background(), client.ObjectKeyFromObject(l), l)
		if l.Status.Lifecycle.ObservedState != "Unknown" {
			t.Fatal("invented physical observer acknowledgement")
		}
	}
}

func TestStoppedSchedulingDirectReadOverridesRunningCache(t *testing.T) {
	f := newSchedFixture(t, schedCfg())
	f.addLab("a", "", nil, "web")
	ctx := context.Background()
	var current lab.Lab
	if err := f.c.Get(ctx, client.ObjectKey{Name: "a", Namespace: "ns-a"}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Skip"}
	direct := fake.NewClientBuilder().WithScheme(f.c.Scheme()).WithObjects(&current).Build()
	f.s.Reader = direct
	if err := f.s.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.device("a", "web").Status.Scheduling.State; got != lab.PodQueued {
		t.Fatal("cached running intent dispatched a currently stopped Lab", got)
	}
}
func TestLifecycleExplicitStartRequeuesOnlyOnceAndTerminalRemainsStopped(t *testing.T) {
	r, l, d, c := lifecycleFixture(t, "Skip")
	ctx := context.Background()
	l.Status.Lifecycle = &lab.LabLifecycleStatus{ObservedState: "Unknown", OperationID: "op", Revision: 1, LabUID: string(l.UID), ObservedGeneration: l.Generation}
	if err := c.Status().Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	l.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Running", OperationID: "start", Revision: 2}
	if err := c.Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.reconcileLifecycle(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if d.Status.Scheduling == nil || d.Status.Scheduling.State != lab.PodQueued {
		t.Fatal("explicit start not queued")
	}
	rv := d.ResourceVersion
	if handled, _, err := r.reconcileLifecycle(ctx, l); err != nil || handled {
		t.Fatal("start did not return to ordinary reconciliation", handled, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if d.ResourceVersion != rv {
		t.Fatal("repeat start rewrote queue")
	}
	l.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "terminal", Revision: 3, SnapshotMode: "Skip", Terminal: true}
	if err := c.Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	dr := &DeviceReconciler{Client: c, Reader: c}
	for i := 0; i < 3; i++ {
		stopped, err := dr.deviceStopped(ctx, d)
		if err != nil || !stopped {
			t.Fatal("terminal Lab resurrected", stopped, err)
		}
	}
}

func TestStoppedMissingRuntimeKeepsCapacityReservedAndLeavesKnownRoomUsable(t *testing.T) {
	cfg := schedCfg()
	cfg.ResourceCheck = true
	f := newSchedFixture(t, cfg)
	f.create(node("n1", "250m", "1Gi"))
	f.addLab("a", "", nil, "web")
	f.addLab("b", "", nil, "web", "db")
	ctx := context.Background()
	var l lab.Lab
	_ = f.c.Get(ctx, client.ObjectKey{Name: "a", Namespace: "ns-a"}, &l)
	l.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Skip"}
	_ = f.c.Update(ctx, &l)
	d := f.device("a", "web")
	d.Status.NodeName = "n1"
	d.Status.PodName = "force-deleted"
	d.Status.Scheduling.State = lab.PodStarted
	_ = f.c.Status().Update(ctx, d)
	f.tick()
	f.wantStates("a/web=D b/db=S b/web=Q")
	// A stale Released record is not the native observation for this operation.
	l.Status.Resources = &lab.RuntimeAllocation{RuntimeState: "Released", OperationID: "old", Revision: 1}
	_ = f.c.Status().Update(ctx, &l)
	f.tick()
	f.wantStates("a/web=D b/db=S b/web=Q")
}

func TestLifecycleOldLabOwnerCannotCreateIntoReplacementLab(t *testing.T) {
	r, l, d, c := lifecycleFixture(t, "Skip")
	l.Spec.Lifecycle = nil
	l.UID = "replacement-lab"
	if err := c.Update(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	dr := &DeviceReconciler{Client: c, Reader: c, Scheme: r.Scheme}
	stopped, err := dr.deviceStopped(context.Background(), d)
	if err != nil || !stopped {
		t.Fatal("cached old Lab owner was authorized by replacement Lab", stopped, err)
	}
}
func TestLifecycleSwitchOnlyRequiredHasNoPhantomWritableCapture(t *testing.T) {
	r, l, d, c := lifecycleFixture(t, "Required")
	l.Spec.Devices[0].Type = lab.DeviceTypeUnmanagedSwitch
	d.Spec.Type = lab.DeviceTypeUnmanagedSwitch
	_ = c.Update(context.Background(), l)
	_ = c.Update(context.Background(), d)
	if err := r.ValidateRequiredSnapshot(context.Background(), l); err != nil {
		t.Fatal("switch-only required policy demanded freezer/registry for no writable device", err)
	}
	if _, _, err := r.reconcileLifecycle(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(l), l)
	if !l.Status.Lifecycle.SnapshotComplete {
		t.Fatal("switch acquired phantom snapshot requirement")
	}
}

func TestLifecycleRequiredPreparationRejectsBeforeIntentChange(t *testing.T) {
	r, l, d, c := lifecycleFixture(t, "Required")
	ctx := context.Background()
	l.Spec.Lifecycle = nil
	_ = c.Update(ctx, l)
	before := l.DeepCopy()
	if err := r.ValidateRequiredSnapshot(ctx, l); err == nil {
		t.Fatal("nonpersistent accepted")
	}
	if l.Spec.Lifecycle != before.Spec.Lifecycle {
		t.Fatal("validation changed intent")
	}
	l.Spec.Devices[0].Persistence = &lab.DevicePersistence{Enabled: true}
	if err := r.ValidateRequiredSnapshot(ctx, l); err == nil {
		t.Fatal("missing native freezer/registry capability accepted")
	}
	l.Spec.Devices[0].Type = lab.DeviceType("vm")
	if err := r.ValidateRequiredSnapshot(ctx, l); err == nil {
		t.Fatal("unsupported VM accepted")
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(d), d)
	if d.Spec.Code != "fixed" {
		t.Fatal("preparation mutated device identity")
	}
}

func TestLifecycleRequiredPreparationRejectsUnmaterializedDisabledPolicy(t *testing.T) {
	r, l, d, c := lifecycleFixture(t, "Required")
	ctx := context.Background()
	r.RequiredSnapshotAvailable = true
	l.Spec.Devices[0].Persistence = &lab.DevicePersistence{Enabled: true}
	if err := c.Delete(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateRequiredSnapshot(ctx, l); err == nil {
		t.Fatal("unmaterialized container accepted with platform persistence disabled")
	}
}

func TestSnapshotBarrierRequestsDurableCommitBeforeFirstDelete(t *testing.T) {
	r, l, d, c, p := requiredReadyFixture(t)
	ctx := context.Background()
	d.Spec.State.CaptureRequest.CommitNodeAgentEpoch = ""
	d.Status.State.Capture.Committed = false
	saved := d.Status
	if err := c.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	d.Status = saved
	if err := c.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d.Status.State.Capture)
	p.Annotations[devicestate.CaptureGuardAnnotation] = string(raw)
	if err := c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}

	if _, _, err := r.reconcileLifecycle(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil || p.DeletionTimestamp != nil {
		t.Fatal("capture succeeded but runtime deleted before durable node ACK", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if d.Spec.State.CaptureRequest.CommitNodeAgentEpoch != d.Status.State.Capture.NodeAgentEpoch {
		t.Fatal("exact capture boot commit was not requested")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
		t.Fatal(err)
	}
	if l.Status.Lifecycle.SnapshotComplete {
		t.Fatal("unacknowledged commit credited collective stop barrier")
	}
}

func TestSnapshotBarrierEveryDeviceMustACKCommittedHold(t *testing.T) {
	r, l, d, c, p := requiredReadyFixture(t)
	ctx := context.Background()
	second := d.DeepCopy()
	second.Name = "a-db"
	second.UID = "device-db"
	second.Spec.Name = "db"
	second.ResourceVersion = ""
	second.Spec.State.CaptureRequest.PodUID = "pod-db"
	second.Spec.State.CaptureRequest.CommitNodeAgentEpoch = ""
	second.Status.State.Capture.PodUID = "pod-db"
	second.Status.State.Capture.Committed = false
	status := second.Status
	if err := c.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	second.Status = status
	if err := c.Status().Update(ctx, second); err != nil {
		t.Fatal(err)
	}
	secondPod := p.DeepCopy()
	secondPod.Name = "db-pod"
	secondPod.UID = "pod-db"
	secondPod.ResourceVersion = ""
	secondPod.OwnerReferences = []metav1.OwnerReference{{Kind: "Device", Name: second.Name, UID: second.UID}}
	raw, _ := json.Marshal(second.Status.State.Capture)
	secondPod.Annotations[devicestate.CaptureGuardAnnotation] = string(raw)
	if err := c.Create(ctx, secondPod); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.reconcileLifecycle(ctx, l); err != nil {
		t.Fatal(err)
	}
	for _, pod := range []*corev1.Pod{p, secondPod} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil || pod.DeletionTimestamp != nil {
			t.Fatal("first delete preceded all-device committed ACK", err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(second), second); err != nil {
		t.Fatal(err)
	}
	second.Status.State.Capture.Committed = true
	if err := c.Status().Update(ctx, second); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(second.Status.State.Capture)
	secondPod.Annotations[devicestate.CaptureGuardAnnotation] = string(raw)
	if err := c.Update(ctx, secondPod); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.reconcileLifecycle(ctx, l); err != nil {
		t.Fatal(err)
	}
	for _, pod := range []*corev1.Pod{p, secondPod} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), pod); err == nil {
			t.Fatal("collectively committed capture did not request deletion")
		}
	}
}

func TestLifecycleConsumesOnlyExactNativeReleasedObservation(t *testing.T) {
	r, l, _, c := lifecycleFixture(t, "Required")
	ctx := context.Background()
	// Test-only producer input: Task3 itself never fabricates these observations.
	l.Status.Lifecycle = &lab.LabLifecycleStatus{ObservedState: "Stopped", OperationID: "op", Revision: 1, LabUID: string(l.UID), ObservedGeneration: l.Generation, SnapshotComplete: true}
	l.Status.Resources = &lab.RuntimeAllocation{RuntimeState: "Released", OperationID: "op", Revision: 1}
	if err := c.Status().Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.reconcileLifecycle(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
		t.Fatal(err)
	}
	if l.Status.Lifecycle.ObservedState != "Stopped" {
		t.Fatal("matching native release was overwritten by API-only Unknown")
	}
	l.Status.Resources.OperationID = "older"
	if err := c.Status().Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	if exactStoppedRelease(l) {
		t.Fatal("old native release accepted for current stop")
	}
}
