package laboratory

import (
	"context"
	"strings"
	"testing"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// withdrawnRecord is the scheduling record a withdrawal leaves.
func withdrawnRecord() *lab.PodSchedule {
	now := metav1.NewTime(time.Now())
	retry := metav1.NewTime(now.Add(time.Minute))
	return &lab.PodSchedule{State: lab.PodQueued, QueuedAt: &now, Withdrawals: 1, RetryAfter: &retry, SchedulerMessage: "Unschedulable: 0/1 nodes are available: 1 Insufficient cpu."}
}

// Stopping a lab whose Deployment device was withdrawn: nothing is recreated, and the stop is not certified from the API's
// absence of a pod (the same honest Unknown as a device that was never sent; a node-agent's native report decides).
func TestStopAfterWithdrawalDeploymentDevice(t *testing.T) {
	r, l, d, c := lifecycleFixture(t, "Skip")
	ctx := context.Background()
	d.Status.Scheduling = withdrawnRecord()
	if err := c.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	dr := &DeviceReconciler{Client: c, Reader: c, Scheme: r.Scheme, Scheduled: true}
	for i := 0; i < 3; i++ {
		if _, err := dr.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(d)}); err != nil {
			t.Fatal(err)
		}
	}
	var deps appsv1.DeploymentList
	if err := c.List(ctx, &deps); err != nil || len(deps.Items) != 0 {
		t.Fatalf("a withdrawn device of a stopped lab got a workload: %d %v", len(deps.Items), err)
	}
	if _, _, err := r.reconcileLifecycle(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
		t.Fatal(err)
	}
	if l.Status.Lifecycle == nil || l.Status.Lifecycle.ObservedState != "Unknown" {
		t.Fatalf("stop after a withdrawal: %+v", l.Status.Lifecycle)
	}
}

// Stopping a lab whose snapshot-backed device was withdrawn after a pod ran: no pod is recreated, and the stop waits for native
// inventory (MissingNativeInventory) instead of certifying a release from the API's absence of the pod.
func TestStopAfterWithdrawalStateDevice(t *testing.T) {
	r, l, d, c := lifecycleFixture(t, "Skip")
	ctx := context.Background()
	d.Spec.State = &lab.DeviceStateSpec{Enabled: true, MaxLayers: 1}
	if err := c.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	stopped := metav1.NewTime(time.Now())
	d.Status.State = &lab.DeviceStateStatus{Incarnation: 2, StoppedAt: &stopped}
	d.Status.Scheduling = withdrawnRecord()
	if err := c.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	dr := &DeviceReconciler{Client: c, Reader: c, Scheme: r.Scheme, Scheduled: true}
	for i := 0; i < 3; i++ {
		if _, err := dr.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(d)}); err != nil {
			t.Fatal(err)
		}
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods); err != nil || len(pods.Items) != 0 {
		t.Fatalf("a withdrawn device of a stopped lab got a pod: %d %v", len(pods.Items), err)
	}
	if _, _, err := r.reconcileLifecycle(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
		t.Fatal(err)
	}
	if l.Status.Lifecycle == nil || l.Status.Lifecycle.ObservedState != "Unknown" || l.Status.Lifecycle.Reason != "MissingNativeInventory" {
		t.Fatalf("stop after a withdrawal: %+v", l.Status.Lifecycle)
	}
	// no inventory came: after a bounded wait the stop goes on without it, and the lab says so
	asked := metav1.NewTime(time.Now().Add(-withdrawnStopGrace - time.Second))
	l.Status.Lifecycle.RequestedAt = &asked
	if err := c.Status().Update(ctx, l); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.reconcileLifecycle(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(l), l); err != nil {
		t.Fatal(err)
	}
	if l.Status.Lifecycle.Reason == "MissingNativeInventory" {
		t.Fatalf("the stop still waits for the withdrawn device: %+v", l.Status.Lifecycle)
	}
	if c := meta.FindStatusCondition(l.Status.Conditions, conditionWithdrawnRuntimeUnverified); c == nil || c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "web") {
		t.Fatalf("condition = %+v", c)
	}
}

// A group stop ignores the Queued records of its stopped labs (withdrawn ones, and those of labs stopped before their turn):
// the scheduler never dispatches them, so they hold nothing. A Starting record, or a Queued record of a lab that is not stopped,
// still holds the stop back.
func TestGroupStopIgnoresQueuedRecordsOfStoppedLabs(t *testing.T) {
	never := metav1.NewTime(time.Now())
	for _, tc := range []struct {
		name    string
		lab     string
		record  *lab.PodSchedule
		stopped bool
	}{
		{"withdrawn", "child", withdrawnRecord(), true},
		{"stopped before its turn", "child", &lab.PodSchedule{State: lab.PodQueued, QueuedAt: &never}, true},
		{"starting", "child", &lab.PodSchedule{State: lab.PodStarting, DispatchedAt: &never}, false},
		{"queued, lab not stopped", "other", &lab.PodSchedule{State: lab.PodQueued, QueuedAt: &never}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, g, c := groupStopFixture(t, groupStoppedChild())
			// the fixture's client has no status subresource for Devices: the record goes in with the object
			d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: tc.lab + "-web", Namespace: "ns"}, Spec: lab.DeviceSpec{LabRef: tc.lab, Name: "web", Type: lab.DeviceTypeContainer},
				Status: lab.DeviceStatus{Scheduling: tc.record}}
			if err := c.Create(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			if stopped, err := r.allGroupLabsStopped(context.Background(), g); err != nil || stopped != tc.stopped {
				t.Fatalf("stopped = %v, want %v (%v)", stopped, tc.stopped, err)
			}
		})
	}
}

// Before a withdrawal deletes the pods of a device, the native identity the node-agent reported for them is kept in the
// device's runtime inventory, as a lab stop does, so a later stop waits for their release instead of finding no trace.
func TestWithdrawalKeepsNativeInventory(t *testing.T) {
	r, l, d, c := lifecycleFixture(t, "Skip")
	ctx := context.Background()
	d.Spec.State = &lab.DeviceStateSpec{Enabled: true, MaxLayers: 1}
	if err := c.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a-web-2", Namespace: "g", UID: "pod-2"}, Spec: corev1.PodSpec{NodeName: "node", Containers: []corev1.Container{{Name: "device", Image: "base"}}}}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	d.Status.State = &lab.DeviceStateStatus{Incarnation: 2}
	d.Status.Scheduling = withdrawnRecord()
	report := lifecycleNativeReport(l, d, pod)
	other := lifecycleNativeReport(l, d, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "an-older-pod"}, Spec: corev1.PodSpec{NodeName: "node"}})
	d.Status.RuntimeReports = []lab.OwnedRuntimeReport{report, other}
	if err := c.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	dr := &DeviceReconciler{Client: c, Reader: c, Scheme: r.Scheme, Scheduled: true}
	if err := dr.deleteWithdrawnPods(ctx, d, []corev1.Pod{*pod}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if len(d.Status.RuntimeInventory) != 1 || d.Status.RuntimeInventory[0].PodUID != "pod-2" {
		t.Fatalf("inventory = %+v", d.Status.RuntimeInventory)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), pod); err == nil && pod.DeletionTimestamp == nil {
		t.Fatal("the withdrawn pod is deleted")
	}
}

// The same for a Deployment device: before its Deployment is deleted, the identity the node-agent reported for its pod is kept.
func TestWithdrawalKeepsNativeInventoryOfDeploymentPods(t *testing.T) {
	r, l, d, c := lifecycleFixture(t, "Skip")
	ctx := context.Background()
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: workloadName(d), Namespace: "g"}, Spec: appsv1.DeploymentSpec{Replicas: ptrInt32(1)}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a-web-x", Namespace: "g", UID: "pod-x", Labels: map[string]string{names.LabelLab: "a", names.LabelDevice: "web"}},
		Spec: corev1.PodSpec{NodeName: "node", Containers: []corev1.Container{{Name: "device", Image: "base"}}}}
	for _, o := range []client.Object{dep, pod} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	d.Status.Scheduling = withdrawnRecord()
	d.Status.RuntimeReports = []lab.OwnedRuntimeReport{lifecycleNativeReport(l, d, pod)}
	if err := c.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	dr := &DeviceReconciler{Client: c, Reader: c, Scheme: r.Scheme, Scheduled: true}
	if err := dr.deleteWithdrawnWorkload(ctx, d, dep); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if len(d.Status.RuntimeInventory) != 1 || d.Status.RuntimeInventory[0].PodUID != "pod-x" {
		t.Fatalf("inventory = %+v", d.Status.RuntimeInventory)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(dep), dep); err == nil {
		t.Fatal("the withdrawn Deployment is deleted")
	}
}
