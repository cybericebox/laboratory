//go:build linux

package gateway

import (
	"context"
	"fmt"
	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nstest"
	"github.com/cybericebox/laboratory/pkg/dhcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

type countedFilter struct {
	netfilter
	calls int
}

func (f *countedFilter) AppendUnique(table, chain string, rules ...string) error {
	f.calls++
	return f.netfilter.AppendUnique(table, chain, rules...)
}
func (f *countedFilter) Exists(table, chain string, rules ...string) (bool, error) {
	f.calls++
	return f.netfilter.Exists(table, chain, rules...)
}
func (f *countedFilter) Insert(table, chain string, pos int, rules ...string) error {
	f.calls++
	return f.netfilter.Insert(table, chain, pos, rules...)
}
func (f *countedFilter) Delete(table, chain string, rules ...string) error {
	f.calls++
	return f.netfilter.Delete(table, chain, rules...)
}
func TestNetnsGatewayControlOneTenTwentyLabs(t *testing.T) {
	nstest.Require(t)
	for _, count := range []int{1, 10, 20} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d_labs_DHCP_%v", count, enabled), func(t *testing.T) {
				scheme := runtime.NewScheme()
				_ = lab.AddToScheme(scheme)
				_ = allocation.AddToScheme(scheme)
				c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&lab.LabGateway{}, &lab.Lab{}).Build()
				filter := newFake(t)
				ipt := manager(filter)
				if err := ipt.SetupFilter(); err != nil {
					t.Fatal(err)
				}
				tracked := &countedFilter{netfilter: filter}
				ipt.ipt = tracked
				dhcpMgr := dhcp.NewManager()
				defer dhcpMgr.Close()
				r := &LabGatewayReconciler{Client: c, IPT: ipt, DHCP: dhcpMgr, Cfg: &Config{Namespace: "g", InetBaseNetwork: "10.9.0.0/16"}}
				for i := 1; i <= count; i++ {
					iface := fmt.Sprintf("lab%d", i)
					nstest.Run(t, "", "ip", "link", "add", iface, "type", "dummy")
					defer nstest.Try("", "ip", "link", "del", iface)
					name := fmt.Sprintf("a%d", i)
					l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "g", UID: types.UID(name + "-uid")}, Spec: lab.LabSpec{Internet: lab.LabNetworkSpec{Enabled: true}}}
					if enabled {
						l.Spec.Internet.DHCPServer = &lab.DHCPServer{Enabled: true, DNS: "1.1.1.1", Ranges: []lab.DHCPRange{{Start: 2, End: 20}}}
					}
					gw := &lab.LabGateway{ObjectMeta: metav1.ObjectMeta{Name: name + "-gw", Namespace: "g", Finalizers: []string{names.FinalizerController, names.FinalizerGateway}}, Spec: lab.LabGatewaySpec{LabName: name, NetworkIndex: uint(i)}}
					if err := c.Create(context.Background(), l); err != nil {
						t.Fatal(err)
					}
					if err := c.Create(context.Background(), gw); err != nil {
						t.Fatal(err)
					}
					if enabled {
						pool := &allocation.Pool{ObjectMeta: metav1.ObjectMeta{Name: "dhcp-inet-" + name + "-0", Namespace: "g", OwnerReferences: []metav1.OwnerReference{{APIVersion: lab.SchemeGroupVersion.String(), Kind: "Lab", Name: name, UID: l.UID}}}}
						if err := c.Create(context.Background(), pool); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gw)}); err != nil {
						t.Fatal(err)
					}
				}
				baseline := tracked.calls
				for repeat := 0; repeat < 10; repeat++ {
					for i := 1; i <= count; i++ {
						if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "g", Name: fmt.Sprintf("a%d-gw", i)}}); err != nil {
							t.Fatal(err)
						}
					}
				}
				if tracked.calls != baseline {
					t.Fatalf("unchanged inputs performed %d filter operations", tracked.calls-baseline)
				}
				t.Logf("labs=%d DHCP=%v initialFilterOperations=%d repeatedFilterOperations=0", count, enabled, baseline)
			})
		}
	}
}
