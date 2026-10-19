package laboratory

import (
	"context"
	"testing"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	if l.Status.Lifecycle == nil || l.Status.Lifecycle.ObservedState != "Unknown" {
		t.Fatalf("stop after a withdrawal: %+v", l.Status.Lifecycle)
	}
}

// A group stop is not admitted while a device of the group has a withdrawn (Queued) record, as for any queued admission.
func TestGroupStopWaitsForWithdrawnDevice(t *testing.T) {
	child := groupStoppedChild()
	r, g, c := groupStopFixture(t, child)
	if stopped, err := r.allGroupLabsStopped(context.Background(), g); err != nil || !stopped {
		t.Fatalf("stopped child not accepted: %v %v", stopped, err)
	}
	// the fixture's client has no status subresource for Devices: the record goes in with the object
	d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "child-web", Namespace: "ns"}, Spec: lab.DeviceSpec{LabRef: "child", Name: "web", Type: lab.DeviceTypeContainer},
		Status: lab.DeviceStatus{Scheduling: withdrawnRecord()}}
	if err := c.Create(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if stopped, err := r.allGroupLabsStopped(context.Background(), g); err != nil || stopped {
		t.Fatalf("group stop admitted with a withdrawn device: %v %v", stopped, err)
	}
}
