//go:build linux

package gateway

import (
	"context"
	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nstest"
	"github.com/cybericebox/laboratory/pkg/dhcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strings"
	"testing"
)

func TestNetnsStoppedGatewayRemovesOnlyOwnedDHCPAddressAndNAT(t *testing.T) {
	nstest.Require(t)
	ctx := context.Background()
	for _, iface := range []string{"lab1", "lab2", "eth0"} {
		nstest.Run(t, "", "ip", "link", "add", iface, "type", "dummy")
		nstest.Run(t, "", "ip", "link", "set", iface, "up")
		defer func(dev string) { _, _ = nstest.Try("", "ip", "link", "del", dev) }(iface)
	}
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	_ = allocation.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&lab.LabGateway{}).Build()
	a := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "g", UID: "lab-a"}, Spec: lab.LabSpec{Internet: lab.LabNetworkSpec{Enabled: true, DHCPServer: &lab.DHCPServer{Enabled: true, Ranges: []lab.DHCPRange{{Start: 2, End: 4}}}}}}
	b := a.DeepCopy()
	b.Name = "b"
	b.UID = "lab-b"
	for i, l := range []*lab.Lab{a, b} {
		if err := c.Create(ctx, l); err != nil {
			t.Fatal(err)
		}
		if err := c.Create(ctx, &allocation.Pool{ObjectMeta: metav1.ObjectMeta{Name: "dhcp-inet-" + l.Name + "-0", Namespace: "g", OwnerReferences: []metav1.OwnerReference{{APIVersion: lab.SchemeGroupVersion.String(), Kind: "Lab", Name: l.Name, UID: l.UID}}}}); err != nil {
			t.Fatal(err)
		}
		if err := c.Create(ctx, &lab.LabGateway{ObjectMeta: metav1.ObjectMeta{Name: l.Name + "-gw", Namespace: "g", Finalizers: []string{names.FinalizerController, names.FinalizerGateway}}, Spec: lab.LabGatewaySpec{LabName: l.Name, NetworkIndex: uint(i + 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	ipt, err := NewIPTablesManager("eth0")
	if err != nil {
		t.Fatal(err)
	}
	if err := ipt.SetupFilter(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ipt.RemoveInput)
	dhcpMgr := dhcp.NewManager()
	defer dhcpMgr.Close()
	r := &LabGatewayReconciler{Client: c, IPT: ipt, DHCP: dhcpMgr, Cfg: &Config{Namespace: "g", InetBaseNetwork: "10.9.0.0/16"}}
	for _, name := range []string{"a-gw", "b-gw"} {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: name, Namespace: "g"}}); err != nil {
			t.Fatal(err)
		}
	}
	if !dhcpMgr.Healthy("a") || !dhcpMgr.Healthy("b") {
		t.Fatal("real DHCP socket control failed")
	}
	a.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Skip"}
	if err := c.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "a-gw", Namespace: "g"}}); err != nil {
			t.Fatal(err)
		}
	}
	if dhcpMgr.Healthy("a") || !dhcpMgr.Healthy("b") {
		t.Fatal("stopped DHCP survived or sibling socket was stopped")
	}
	if strings.Contains(nstest.Run(t, "", "ip", "-4", "addr", "show", "dev", "lab1"), "10.9.1.1/24") || !strings.Contains(nstest.Run(t, "", "ip", "-4", "addr", "show", "dev", "lab2"), "10.9.2.1/24") {
		t.Fatal("stopped route/address survived or sibling lost its address")
	}
	nat := nstest.Run(t, "", "iptables-save", "-t", "nat")
	if strings.Contains(nat, "10.9.1.0/24") || !strings.Contains(nat, "10.9.2.0/24") {
		t.Fatal("NAT cleanup crossed Lab ownership", nat)
	}
	filter := nstest.Run(t, "", "iptables-save", "-t", "filter")
	if !strings.Contains(filter, "-i lab1 -j DROP") {
		t.Fatal("stopped source gate missing")
	}
	var got lab.Lab
	if err := c.Get(ctx, client.ObjectKeyFromObject(a), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Internet.Enabled || !got.Spec.Internet.DHCPServer.Enabled {
		t.Fatal("stop discarded retained network configuration")
	}
}
