//go:build linux

package nodeagent

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
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
