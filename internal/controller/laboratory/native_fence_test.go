package laboratory

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cybericebox/laboratory/internal/names"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNativeStoppedChildFenceSurvivesGroupPauseAndRestart(t *testing.T) {
	now := metav1.Now()
	s := runtime.NewScheme()
	if e := lab.AddToScheme(s); e != nil {
		t.Fatal(e)
	}
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "group", UID: types.UID("child-uid"), Generation: 5}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "child-stop", Revision: 2, SnapshotMode: "Required"}}, Status: lab.LabStatus{Lifecycle: &lab.LabLifecycleStatus{LabUID: "child-uid", OperationID: "child-stop", Revision: 2, ObservedGeneration: 5, ObservedState: "Stopped", SnapshotComplete: true, AccessFenced: true, AccessFencedAt: &now, AccessFenceVPNBootID: "original-vpn-boot"}, Resources: &lab.RuntimeAllocation{OperationID: "child-stop", Revision: 2, RuntimeState: "Released", ObservedAt: &now, ReleasedAt: &now}}}
	l.Spec.VPN.Enabled = true
	r := &LabReconciler{Client: fake.NewClientBuilder().WithScheme(s).Build()}
	for _, groupState := range []string{"Stopped", "Running"} {
		t.Run(groupState, func(t *testing.T) {
			ok, at, boot, e := r.currentAccessFence(context.Background(), l)
			if e != nil || !ok || at == nil || boot != "original-vpn-boot" {
				t.Fatalf("physically stopped child required running VPN: %v %v %s", ok, e, boot)
			}
		})
	}
	l.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Running", OperationID: "new-child-start", Revision: 3}
	l.Generation = 6
	ok, _, _, e := r.currentAccessFence(context.Background(), l)
	if e != nil || ok {
		t.Fatalf("new child start reused old fence: %v %v", ok, e)
	}
}

// A stop in a suspended group (no VPN pod, none wanted) has nothing to fence; a stop with a running VPN waits for the real fence; a start
// never takes the shortcut.
func TestStopFenceNeedsNoVPNPodWhenThereIsNoneAndNoneIsWanted(t *testing.T) {
	zero, one := int32(0), int32(1)
	deploy := func(replicas int32) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: names.ComponentVPN, Namespace: "group"}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}}
	}
	for _, tc := range []struct {
		name    string
		intent  string
		objects []client.Object
		fenced  bool
	}{
		{"stop, deployment at zero", "Stopped", []client.Object{deploy(zero)}, true},
		{"stop, no deployment (not provisioned yet): waits", "Stopped", nil, false},
		{"stop, vpn running", "Stopped", []client.Object{deploy(one)}, false},
		{"stop, a pod is left", "Stopped", []client.Object{deploy(zero), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vpn-x", Namespace: "group", Labels: map[string]string{names.LabelComponent: names.ComponentVPN}}}}, false},
		{"start never takes the shortcut", "Running", []client.Object{deploy(zero)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := runtime.NewScheme()
			_ = lab.AddToScheme(s)
			_ = corev1.AddToScheme(s)
			_ = appsv1.AddToScheme(s)
			l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "group", UID: "child-uid", Generation: 5}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: tc.intent, OperationID: "op", Revision: 2, SnapshotMode: "Skip"}}}
			l.Spec.VPN.Enabled = true
			r := &LabReconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(tc.objects...).Build()}
			ok, at, boot, err := r.currentAccessFence(context.Background(), l)
			if err != nil || ok != tc.fenced {
				t.Fatalf("fenced = %v (%v), want %v", ok, err, tc.fenced)
			}
			if ok && (at == nil || boot == "") {
				t.Fatalf("a fence carries its time and boot: %v %q", at, boot)
			}
			// the time stays the one of the first call, so the lifecycle status does not change on every pass
			if ok {
				l.Status.Lifecycle = &lab.LabLifecycleStatus{OperationID: "op", Revision: 2, AccessFenced: true, AccessFencedAt: at, AccessFenceVPNBootID: boot}
				_, again, _, _ := r.currentAccessFence(context.Background(), l)
				if !again.Equal(at) {
					t.Fatalf("the fence time moved: %v -> %v", at, again)
				}
			}
		})
	}
}
