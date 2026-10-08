//go:build linux

package reconciler

import (
	"context"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nstest"
	"github.com/cybericebox/laboratory/internal/vpn"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
)

func TestNetnsHelperProcess(t *testing.T) { nstest.HelperProcess() }
func TestNetnsStoppedIntentRevokesEstablishedBothDirectionsAndPreservesSibling(t *testing.T) {
	nstest.Require(t)
	ctx := context.Background()
	for _, ns := range []string{"pa", "a", "b"} {
		nstest.NS(t, ns)
	}
	nstest.Veth(t, "", "wg0", "10.8.0.1/24", "pa", "p0", "10.8.0.2/24")
	nstest.Veth(t, "", "lab1", "10.8.1.1/24", "a", "a0", "10.8.1.2/24")
	nstest.Veth(t, "", "lab2", "10.8.2.1/24", "b", "b0", "10.8.2.2/24")
	t.Cleanup(func() {
		for _, dev := range []string{"wg0", "lab1", "lab2"} {
			_, _ = nstest.Try("", "ip", "link", "del", dev)
		}
	})
	for _, e := range [][2]string{{"pa", "10.8.0.1"}, {"a", "10.8.1.1"}, {"b", "10.8.2.1"}} {
		nstest.Run(t, e[0], "ip", "route", "add", "default", "via", e[1])
	}
	nstest.Run(t, "", "sysctl", "-w", "net.ipv4.ip_forward=1")
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	a := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g", UID: "lab-a", Generation: 1}, Spec: lab.LabSpec{VPN: lab.LabNetworkSpec{Enabled: true}}, Status: lab.LabStatus{Phase: lab.PhaseReady, VPN: lab.LabNetworkStatus{Ready: true, CIDR: "10.8.1.0/24"}}}
	b := a.DeepCopy()
	b.Name = "b"
	b.UID = "lab-b"
	b.Status.VPN.CIDR = "10.8.2.0/24"
	p := &lab.LabGroupClient{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "g"}, Status: lab.LabGroupClientStatus{AssignedIP: "10.8.0.2/32"}}
	policy := &lab.LabGroupAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: names.LabGroupAccessPolicyName, Namespace: "g", Generation: 1}, Spec: lab.LabGroupAccessPolicySpec{OperationID: "acl", Revision: 1, ExpectedGroupUID: "group-uid", Rules: []lab.LabGroupAccessRule{{Action: lab.LabGroupAccessAllow}}}}
	leg := &lab.LabVPN{ObjectMeta: metav1.ObjectMeta{Name: names.LabVPNObjectName("a"), Namespace: "g", OwnerReferences: []metav1.OwnerReference{{Kind: "Lab", UID: a.UID}}}, Spec: lab.LabVPNSpec{LabName: "a", NetworkIndex: 1}}
	second := leg.DeepCopy()
	second.Name = names.LabVPNObjectName("b")
	second.Spec.LabName = "b"
	second.Spec.NetworkIndex = 2
	second.OwnerReferences[0].UID = b.UID
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vpn-pod", Namespace: "g", UID: "vpn-uid", Labels: map[string]string{names.LabelComponent: names.ComponentVPN}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "vpn", ContainerID: "containerd://vpn", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(a, b, p, policy, leg, second, &lab.LabTrafficReport{}).WithObjects(a, b, p, policy, leg, second, pod).Build()
	ipt, err := vpn.NewIPTablesManager("wg0")
	if err != nil {
		t.Fatal(err)
	}
	if err := ipt.SetupForwardPolicy(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ipt.Cleanup)
	r := &AccessReconciler{Client: c, Reader: c, IPT: ipt, Conntrack: vpn.NewConntrackRevoker(), GroupUID: "group-uid", Runtime: lab.VPNRuntimeIdentity{BootID: "boot", PodName: pod.Name, PodUID: string(pod.UID), ContainerID: "containerd://vpn"}}
	if err := r.initializeCurrentBoot(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: "access", Namespace: "g"}}
	reconcileAccess(t, r, req)
	for _, ns := range []string{"a", "b"} {
		nstest.Echo(t, ns, "tcp", "0.0.0.0:7910")
	}
	nstest.Echo(t, "pa", "tcp", "0.0.0.0:7911")
	stopped := []*nstest.Dialog{nstest.Persistent(t, "pa", "tcp", "10.8.1.2:7910"), nstest.Persistent(t, "a", "tcp", "10.8.0.2:7911")}
	siblings := []*nstest.Dialog{nstest.Persistent(t, "pa", "tcp", "10.8.2.2:7910"), nstest.Persistent(t, "b", "tcp", "10.8.0.2:7911")}
	for _, d := range append(stopped, siblings...) {
		if !d.Exchange(t) {
			t.Fatal("native established control failed")
		}
	}
	start := time.Now()
	a.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop-a", Revision: 1, SnapshotMode: "Skip"}
	a.Generation = 2
	if err := c.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Backend policy remains the old allow-all throughout this stop.
	reconcileAccess(t, r, req)
	for _, d := range stopped {
		if d.Exchange(t) {
			t.Fatal("established stopped direction survived")
		}
	}
	for _, d := range siblings {
		if !d.Exchange(t) {
			t.Fatal("sibling established direction was revoked")
		}
	}
	var got lab.LabVPN
	_ = c.Get(ctx, client.ObjectKeyFromObject(leg), &got)
	if got.Status.AccessFence == nil || got.Status.AccessFence.OperationID != "stop-a" {
		t.Fatal("no native retirement fence", got.Status)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(policy), policy)
	if policy.Status.State != "Applied" || policy.Status.AppliedRevision != 1 || policy.Status.OperationID != "acl" || policy.Status.VPNBootID != "boot" {
		t.Fatal("wrong policy certificate", policy.Status)
	}
	t.Logf("desired stop to reconciliation and both established-direction rejection: %s", time.Since(start))
}
