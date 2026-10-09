package laboratory

import (
	"context"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func groupStoppedChild() *lab.Lab {
	now := metav1.Now()
	return &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "ns", UID: "child-uid", Generation: 3}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "child-op", Revision: 2, SnapshotMode: "Skip"}}, Status: lab.LabStatus{Lifecycle: &lab.LabLifecycleStatus{LabUID: "child-uid", OperationID: "child-op", Revision: 2, ObservedGeneration: 3, ObservedState: "Stopped", StoppedAt: &now}, Resources: &lab.RuntimeAllocation{OperationID: "child-op", Revision: 2, RuntimeState: "Released", ObservedAt: &now, ReleasedAt: &now}}}
}
func groupStopFixture(t *testing.T, child *lab.Lab) (*LabGroupReconciler, *lab.LabGroup, client.Client) {
	t.Helper()
	g := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g", UID: "group-uid", Generation: 1}, Spec: lab.LabGroupSpec{Lifecycle: &lab.GroupLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 1, RequireAllLabsStopped: true}}, Status: lab.LabGroupStatus{Namespace: "ns"}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: "ns", UID: "dep-uid"}, Spec: appsv1.DeploymentSpec{Replicas: ptrInt32(1)}}
	c := fake.NewClientBuilder().WithScheme(retentionScheme(t)).WithStatusSubresource(g, child, dep).WithObjects(g, child, dep).Build()
	return &LabGroupReconciler{Client: c, Reader: c}, g, c
}
func TestGroupLifecycleWaitsExactStoppedReleaseAndPendingStarts(t *testing.T) {
	for _, state := range []string{"Running", "Snapshotting", "Stopping", "StopFailed", "Unknown"} {
		t.Run(state, func(t *testing.T) {
			child := groupStoppedChild()
			child.Status.Lifecycle.ObservedState = state
			r, g, c := groupStopFixture(t, child)
			if _, _, err := r.reconcileGroupLifecycle(context.Background(), g); err != nil {
				t.Fatal(err)
			}
			var dep appsv1.Deployment
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "vpn"}, &dep); err != nil {
				t.Fatal(err)
			}
			if *dep.Spec.Replicas != 1 {
				t.Fatal("scaled active child service")
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(g), g); err != nil {
				t.Fatal(err)
			}
			if g.Status.Lifecycle.Reason != "WaitingForLabs" {
				t.Fatalf("reason %s", g.Status.Lifecycle.Reason)
			}
		})
	}
	for _, mut := range []func(*lab.Lab){func(l *lab.Lab) { l.Status.Resources.RuntimeState = "Unknown" }, func(l *lab.Lab) { l.Status.Lifecycle.Revision-- }, func(l *lab.Lab) { l.Status.Lifecycle.LabUID = "replaced" }, func(l *lab.Lab) { l.Status.Scheduling = &lab.SchedulingStatus{Pending: 1} }} {
		child := groupStoppedChild()
		mut(child)
		r, g, _ := groupStopFixture(t, child)
		if stopped, err := r.allGroupLabsStopped(context.Background(), g); err != nil || stopped {
			t.Fatalf("stopped=%v err=%v", stopped, err)
		}
	}
	child := groupStoppedChild()
	r, g, _ := groupStopFixture(t, child)
	if stopped, err := r.allGroupLabsStopped(context.Background(), g); err != nil || !stopped {
		t.Fatalf("matching release not accepted: %v %v", stopped, err)
	}
}
func TestGroupLifecycleAPIPodAbsenceCannotRelease(t *testing.T) {
	r, g, c := groupStopFixture(t, groupStoppedChild())
	if _, _, err := r.reconcileGroupLifecycle(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(g), g); err != nil {
		t.Fatal(err)
	}
	if g.Status.Lifecycle.ObservedState != "Unknown" || g.Status.Lifecycle.Reason != "WaitingForNativeInventory" {
		t.Fatalf("API absence invented release: %+v", g.Status.Lifecycle)
	}
}
func TestGroupLifecycleLegacySuspendedAndStartNeverResurrectChild(t *testing.T) {
	child := groupStoppedChild()
	child.Spec.Lifecycle.Terminal = true
	r, g, c := groupStopFixture(t, child)
	g.Spec.Lifecycle = nil
	g.Spec.Suspended = true
	if err := c.Update(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if handled, _, err := r.reconcileGroupLifecycle(context.Background(), g); err != nil || handled {
		t.Fatalf("legacy suspension became full stop: %v %v", handled, err)
	}
	g.Spec.Lifecycle = &lab.GroupLifecycleSpec{DesiredState: "Running", OperationID: "start", Revision: 2}
	r.Scheduled = true
	if err := c.Update(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if handled, _, err := r.reconcileGroupLifecycle(context.Background(), g); err != nil || handled {
			t.Fatalf("start blocked normal service reconcile: %v %v", handled, err)
		}
	}
	var after lab.Lab
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(child), &after); err != nil {
		t.Fatal(err)
	}
	if !after.Spec.Lifecycle.IsStopped() || !after.Spec.Lifecycle.Terminal {
		t.Fatal("group start changed terminal child")
	}
}
