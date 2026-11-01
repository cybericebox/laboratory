//go:build linux

package nodeagent

import (
	"context"
	"errors"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nstest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A leg that cannot be wired (its port is held by a live pod) must not hold back the other legs of the group pod: they are wired, and the
// first error comes back so that the pass is retried and the leg heals by itself.
func TestNetnsWireLegsGoOnAfterAFailedLeg(t *testing.T) {
	nstest.Require(t)
	ovs := testOVSDB(t)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = lab.AddToScheme(scheme)
	live := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "live", Namespace: "g", UID: "live-uid"}}
	fresh := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: "g", UID: "fresh-uid", Labels: map[string]string{names.LabelComponent: names.ComponentGateway}}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live, fresh).Build()

	held, free := names.GWHostPortKey("g", 1), names.GWHostPortKey("g", 2)
	if err := ovs.AddVethPortOwned(held, types.UID(live.UID)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ovs.DelVethPort(held); _ = ovs.DelVethPort(free) })

	r := &NetworkAttachReconciler{Client: kube, Reader: kube, NodeName: "node", OVS: ovs}
	atts := []NetAttachment{{Name: held}, {Name: free}}
	_, err := r.wireLegs(context.Background(), fresh, names.ComponentGateway, atts, "/nonexistent/netns")
	if !errors.Is(err, ErrPortOwnerChanged) {
		t.Fatalf("the first error must come back so the pass is retried: %v", err)
	}
	owners, err := ovs.PortOwners()
	if err != nil {
		t.Fatal(err)
	}
	if owners[free] != fresh.UID {
		t.Fatalf("the leg after the failed one must still be wired: owners %v", owners)
	}
	if owners[held] != live.UID {
		t.Fatalf("the held port must stay with its live owner: owners %v", owners)
	}
}
