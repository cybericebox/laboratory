//go:build linux

package nodeagent

import (
	"context"
	"errors"
	"testing"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodeagent/ofclient"
	"github.com/cybericebox/laboratory/internal/nstest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	if err := ovs.AddVethPortOwned(held, live.UID); err != nil {
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

// The old pod of a group is gone and its veth died with its netns: the port row is still in OVSDB, OpenFlow no longer lists the port. The
// retirement of that port must not wait for a port that cannot come back; the key goes to the replacement pod.
func TestNetnsPortGoneFromOpenFlowIsRetiredForTheReplacement(t *testing.T) {
	nstest.Require(t)
	ovs, flows, _, _ := nativeOwnerOVS(t)
	kube, old, replacement := ownerPods(t, names.ComponentGateway)
	key := groupPortKey(names.ComponentGateway, "g", 1)
	t.Cleanup(func() { _ = ovs.DelVethPort(key) })
	if err := ovs.AddVethPortOwned(key, old.UID); err != nil {
		t.Fatal(err)
	}
	if err := flows.AddT0Port(key, 22); err != nil {
		t.Fatal(err)
	}
	nstest.Run(t, "", "ip", "link", "del", key)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := flows.refreshPorts(); err != nil {
			t.Fatal(err)
		}
		if _, err := flows.client.PortNo(key); errors.Is(err, ofclient.ErrPortNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("OpenFlow still lists the port")
		}
		time.Sleep(50 * time.Millisecond)
	}
	r := &NetworkAttachReconciler{Client: kube, Reader: kube, NodeName: "node", OVS: ovs, Flows: flows}
	if err := r.ensurePodPortOwner(context.Background(), replacement, key); err != nil {
		t.Fatalf("a port that is gone from OpenFlow and whose pod is deleted counts as retired: %v", err)
	}
	owners, err := ovs.PortOwners()
	if err != nil {
		t.Fatal(err)
	}
	if _, present := owners[key]; present {
		t.Fatalf("the old pod's port row must be gone so the replacement can take the key: %v", owners)
	}
}
