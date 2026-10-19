package nodeagent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/profiles"
)

func startingNode(label bool, tun string, taints ...corev1.Taint) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{Taints: taints}}
	if label {
		n.Labels = map[string]string{names.LabelNodeAgentReady: "true"}
	}
	if tun != "" {
		n.Status.Allocatable = corev1.ResourceList{corev1.ResourceName(profiles.TUNResource): resource.MustParse(tun)}
	}
	return n
}

var (
	startingTaint = corev1.Taint{Key: StartingTaintKey, Value: "true", Effect: corev1.TaintEffectNoSchedule}
	otherTaint    = corev1.Taint{Key: "dedicated", Value: "lab", Effect: corev1.TaintEffectNoSchedule}
)

func taintsOf(t *testing.T, c client.Client) []corev1.Taint {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "n1"}, &n); err != nil {
		t.Fatal(err)
	}
	return n.Spec.Taints
}

func TestStartupTaintRemovedOnlyWhenAllConditionsHold(t *testing.T) {
	for name, tc := range map[string]struct {
		node    *corev1.Node
		removed bool
	}{
		"no label":       {startingNode(false, "4", startingTaint, otherTaint), false},
		"no tun":         {startingNode(true, "", startingTaint, otherTaint), false},
		"tun is zero":    {startingNode(true, "0", startingTaint, otherTaint), false},
		"everything set": {startingNode(true, "4", startingTaint, otherTaint), true},
	} {
		c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(tc.node).Build()
		r := &StartupTaintRemover{Client: c, NodeName: "n1", TunSlots: 4}
		done, err := r.Reconcile(context.Background())
		taints := taintsOf(t, c)
		if err != nil || done != tc.removed {
			t.Fatalf("%s: done=%v err=%v", name, done, err)
		}
		if tc.removed && (len(taints) != 1 || taints[0].Key != "dedicated") {
			t.Fatalf("%s: other taints must stay, got %v", name, taints)
		}
		if !tc.removed && len(taints) != 2 {
			t.Fatalf("%s: taint removed too early: %v", name, taints)
		}
	}
}

func TestStartupTaintLastTaintIsRemovedAndPluginOffNeedsNoTun(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(startingNode(true, "", startingTaint)).Build()
	r := &StartupTaintRemover{Client: c, NodeName: "n1", TunSlots: 0}
	if done, err := r.Reconcile(context.Background()); err != nil || !done || len(taintsOf(t, c)) != 0 {
		t.Fatalf("done=%v err=%v taints=%v", done, err, taintsOf(t, c))
	}
}

func TestStartupTaintAbsentIsANoOp(t *testing.T) {
	patched := false
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(startingNode(true, "4", otherTaint)).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, o ...client.PatchOption) error {
			patched = true
			return cl.Patch(ctx, obj, p, o...)
		}}).Build()
	r := &StartupTaintRemover{Client: c, NodeName: "n1", TunSlots: 4}
	if done, err := r.Reconcile(context.Background()); err != nil || !done || patched || len(taintsOf(t, c)) != 1 {
		t.Fatalf("done=%v err=%v patched=%v", done, err, patched)
	}
}

func TestStartupTaintRetriesOnConflict(t *testing.T) {
	calls := 0
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(startingNode(true, "4", startingTaint)).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, o ...client.PatchOption) error {
			calls++
			if calls < 3 {
				return errors.NewConflict(schema.GroupResource{Resource: "nodes"}, "n1", nil)
			}
			return cl.Patch(ctx, obj, p, o...)
		}}).Build()
	r := &StartupTaintRemover{Client: c, NodeName: "n1", TunSlots: 4}
	if done, err := r.Reconcile(context.Background()); err != nil || !done || calls != 3 || len(taintsOf(t, c)) != 0 {
		t.Fatalf("done=%v err=%v calls=%d", done, err, calls)
	}
}
