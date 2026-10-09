package laboratory

import (
	"context"
	"testing"

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
