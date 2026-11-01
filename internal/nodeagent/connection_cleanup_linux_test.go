//go:build linux

package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/devices"
)

// A cleanup that cannot finish says so: a NotFound (swallowed by the controller wrapper) becomes a retry with the condition CleanupBlocked
// and the error in its message, so a Connection that keeps its finalizer is never silent.
func TestConnectionCleanupBlockedIsVisible(t *testing.T) {
	s := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	conn := &laboratoryv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", Generation: 1, DeletionTimestamp: &now, Finalizers: []string{"cybericebox.com/ovs-cleanup"}},
		Spec: laboratoryv1alpha1.ConnectionSpec{LabRef: "gone-lab"}}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(conn).WithObjects(conn).Build()
	r := &ConnectionReconciler{Client: c, Reader: c, NodeName: "node"}
	res, err := r.reconcileDelete(context.Background(), conn)
	if err != nil || res.RequeueAfter <= 0 {
		t.Fatalf("a NotFound in the cleanup must become a visible retry: %+v %v", res, err)
	}
	var got laboratoryv1alpha1.Connection
	if err := c.Get(context.Background(), types.NamespacedName{Name: "c", Namespace: "ns"}, &got); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionWired)
	if cond == nil || cond.Reason != WaitCleanupBlocked || !strings.Contains(cond.Message, "gone-lab") {
		t.Fatalf("condition %+v", cond)
	}
}

// A switch connected to the VPN has no patch pair: the VPN is not a Device, and looking it up as one (a NotFound) kept the finalizer of every
// such Connection. A switch whose own Device is gone is not retired here either: its cleanup took its ports and flows.
func TestRetireSwitchEndpointsDoesNotLookUpTheVirtualPeerAsADevice(t *testing.T) {
	s := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	sw := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: devices.Name("l", "sw-1"), Namespace: "ns"},
		Spec: laboratoryv1alpha1.DeviceSpec{LabRef: "l", Name: "sw-1", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sw).Build()
	r := &ConnectionReconciler{Client: c, Reader: c, NodeName: "node"}
	conn := &laboratoryv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}, Spec: laboratoryv1alpha1.ConnectionSpec{LabRef: "l",
		Endpoints: []laboratoryv1alpha1.EndpointSpec{{Device: "vpn", Interface: "eth0"}, {Device: "sw-1", Interface: "g0"}}}}
	if err := r.retireSwitchEndpoints(context.Background(), conn); err != nil {
		t.Fatalf("a switch connected to the VPN must retire cleanly: %v", err)
	}
	gone := conn.DeepCopy()
	gone.Spec.Endpoints[1].Device = "sw-deleted"
	if err := r.retireSwitchEndpoints(context.Background(), gone); err != nil {
		t.Fatalf("an endpoint device that is gone has nothing left to retire: %v", err)
	}
}

// A port that still belongs to the replaced pod is waited for on a short timer, not failed into the exponential back-off of the controller
// (which kept the labs of a rolled gateway cut off for tens of seconds after their legs were wired).
func TestPortOwnerPendingIsAWaitNotAnError(t *testing.T) {
	for _, err := range []error{ErrPortOwnerChanged, ErrPortOwnerUnknown, fmt.Errorf("wrapped: %w", ErrPortOwnerChanged)} {
		if !portOwnerPending(err) {
			t.Errorf("%v must be waited for", err)
		}
	}
	for _, err := range []error{nil, errors.New("ovs is down")} {
		if portOwnerPending(err) {
			t.Errorf("%v must stay an error", err)
		}
	}
	if waitMessage(WaitPortOwner) == WaitPortOwner || wiredRetryOnWait > 5*time.Second {
		t.Error("the wait needs a message and a short timer")
	}
}
