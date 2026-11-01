//go:build linux

package gateway

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/egressproxy"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nstest"
	"github.com/cybericebox/laboratory/pkg/dhcp"
	"github.com/cybericebox/laboratory/pkg/netutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Labs opened by a pool of workers end up with exactly the rules, addresses and registrations that labs opened one by one get.
func TestNetnsLabsOpenedInParallelEqualLabsOpenedOneByOne(t *testing.T) {
	nstest.Require(t)
	const perGroup = 10
	scheme := runtime.NewScheme()
	_ = lab.AddToScheme(scheme)
	_ = allocation.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&lab.LabGateway{}, &lab.Lab{}).Build()
	m, err := NewIPTablesManager("eth0")
	if err != nil {
		t.Fatal(err)
	}
	m.Egress = EgressPorts{HTTP: 3180, TLS: 3443, DNS: 5353}
	t.Cleanup(m.RemoveInput)
	if err := m.SetupFilter(); err != nil {
		t.Fatal(err)
	}
	// other tests of the package leave their lab rules in this namespace: start from none
	nstest.Run(t, "", "iptables", "-t", "nat", "-F")
	nstest.Run(t, "", "iptables", "-F", sourceChain)
	nstest.Run(t, "", "iptables", "-F", "CICE_INPUT")
	dhcpMgr := dhcp.NewManager()
	defer dhcpMgr.Close()
	labs := egressproxy.NewLabMap()
	r := &LabGatewayReconciler{Client: c, IPT: m, DHCP: dhcpMgr, Labs: labs, Shaper: NewShaper(), Cfg: &Config{Namespace: "g", InetBaseNetwork: "10.9.0.0/16"}}

	for i := 1; i <= 2*perGroup; i++ {
		iface := fmt.Sprintf("lab%d", i)
		nstest.Run(t, "", "ip", "link", "add", iface, "type", "dummy")
		nstest.Run(t, "", "ip", "link", "set", iface, "up")
		t.Cleanup(func() { _, _ = nstest.Try("", "ip", "link", "del", iface) })
		name := fmt.Sprintf("a%d", i)
		l := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "g", UID: types.UID(name + "-uid")}, Spec: lab.LabSpec{Internet: lab.LabNetworkSpec{Enabled: true}}}
		gw := &lab.LabGateway{ObjectMeta: metav1.ObjectMeta{Name: name + "-gw", Namespace: "g", Finalizers: []string{names.FinalizerController, names.FinalizerGateway}}, Spec: lab.LabGatewaySpec{LabName: name, NetworkIndex: uint(i)}}
		if err := c.Create(context.Background(), l); err != nil {
			t.Fatal(err)
		}
		if err := c.Create(context.Background(), gw); err != nil {
			t.Fatal(err)
		}
	}
	reconcile := func(i int) {
		t.Helper()
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "g", Name: fmt.Sprintf("a%d-gw", i)}}); err != nil {
			t.Errorf("lab %d: %v", i, err)
		}
	}

	start := time.Now()
	for i := 1; i <= perGroup; i++ {
		reconcile(i)
	}
	sequential := time.Since(start)

	start = time.Now()
	var wg sync.WaitGroup
	jobs := make(chan int)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				reconcile(i)
			}
		}()
	}
	for i := perGroup + 1; i <= 2*perGroup; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	parallel := time.Since(start)
	t.Logf("%d labs one by one: %s; with 4 workers: %s", perGroup, sequential, parallel)

	save := nstest.Run(t, "", "iptables-save")
	// every rule that names a lab, with the lab number and the subnet taken out, appears once per lab (the labs opened one by one and the labs
	// opened by the pool alike)
	num := regexp.MustCompile(`lab\d+\b`)
	addr := regexp.MustCompile(`10\.9\.\d+\.`)
	count := map[string]int{}
	for _, line := range strings.Split(save, "\n") {
		if num.MatchString(line) && !strings.Contains(line, "lab+") {
			count[addr.ReplaceAllString(num.ReplaceAllString(line, "labN"), "10.9.X.")]++
		}
	}
	if len(count) < 8 {
		t.Fatalf("too few kinds of lab rules to compare: %v", count)
	}
	for rule, n := range count {
		if n != 2*perGroup {
			t.Errorf("%d times, want once per lab (%d): %s", n, 2*perGroup, rule)
		}
	}
	for k := 1; k <= 2*perGroup; k++ {
		if name, ok := labs.Lookup(mustAddr(t, sevenOf(t, k))); !ok || name != fmt.Sprintf("a%d", k) {
			t.Errorf("lab %d is not registered for the egress services: %q %v", k, name, ok)
		}
		if out := nstest.Run(t, "", "ip", "-o", "addr", "show", "dev", fmt.Sprintf("lab%d", k)); !strings.Contains(out, firstHostIP(subnetOf(t, k))+"/") {
			t.Errorf("lab %d has no gateway address:\n%s", k, out)
		}
	}
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func subnetOf(t *testing.T, k int) string {
	t.Helper()
	s, err := netutil.SubnetForIndex("10.9.0.0/16", 24, uint(k))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// sevenOf is some address in the subnet of lab index k.
func sevenOf(t *testing.T, k int) string {
	t.Helper()
	p := netip.MustParsePrefix(subnetOf(t, k))
	a := p.Addr().As4()
	a[3] = 7
	return netip.AddrFrom4(a).String()
}
