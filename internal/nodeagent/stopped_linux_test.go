//go:build linux

package nodeagent

import (
	"context"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nstest"
	nodev1 "github.com/cybericebox/laboratory/pkg/rpc/node/v1"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestStoppedLabCNIPreventsStalePodSetup(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g"}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Skip"}}}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "g", UID: "pod", Labels: map[string]string{names.LabelLab: "a", names.LabelDevice: "web"}, Annotations: map[string]string{names.AnnotationDefaultNetwork: names.DefaultEth0}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(l, p).Build()
	s := NewNodeAgentServer(nil, nil)
	s.SetK8sClient(c)
	if _, err := s.SetupNetworks(context.Background(), &nodev1.SetupNetworksRequest{Namespace: "g", Name: "pod", PodUid: "pod"}); err == nil {
		t.Fatal("stale CNI add authorized a stopped Lab")
	}
}

func TestNetnsStoppedGroupAttachmentDeletesOnlyOwnedKernelVethAndOVSDBPort(t *testing.T) {
	nstest.Require(t)
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	a := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g"}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Skip"}}}
	b := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "g"}}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: "g", UID: "vpn", Labels: map[string]string{names.LabelComponent: names.ComponentVPN}}, Spec: corev1.PodSpec{NodeName: "node"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	leg := &lab.LabVPN{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g"}, Spec: lab.LabVPNSpec{LabName: "a", NetworkIndex: 1}}
	sibling := leg.DeepCopy()
	sibling.Name = "b"
	sibling.Spec.LabName = "b"
	sibling.Spec.NetworkIndex = 2
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(a, b, p, leg, sibling).Build()
	ovs := testOVSDB(t)
	first, second := names.VPNHostPortKey("g", 1), names.VPNHostPortKey("g", 2)
	for _, key := range []string{first, second} {
		if err := ovs.AddVethPort(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ovs.DelVethPort(key) })
	}
	r := &NetworkAttachReconciler{Client: c, Reader: c, NodeName: "node", OVS: ovs, CRISock: "/nonexistent-owned-fixture-cri.sock"}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)}); err != nil {
		t.Fatal(err)
	}
	if _, err := netlink.LinkByName(first); err == nil {
		t.Fatal("stopped group's actual kernel veth remained")
	}
	if _, err := netlink.LinkByName(second); err != nil {
		t.Fatal("sibling kernel veth was detached", err)
	}
	ports, err := ovs.PortKeys()
	if err != nil {
		t.Fatal(err)
	}
	if ports[first] || !ports[second] {
		t.Fatal("OVSDB removal crossed Lab ownership", ports)
	}
}
