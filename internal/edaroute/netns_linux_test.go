//go:build linux

package edaroute

import (
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/nstest"
)

func TestNetnsHelperProcess(*testing.T) { nstest.HelperProcess() }

// The pod is this process; the EDA interface is wired like Cilium wires it:
//
//	pod (this process)   eda 10.0.0.2/24  <-> ns proxy   10.0.0.1/24 and 10.0.1.5 (the proxy, one hop away)
//	                     eth1       192.168.0.2   <-> ns lab     192.168.0.1 (the lab's gateway, with the author's default route)
//
// Cilium leaves "default via 10.0.0.1 dev eda" and "10.0.0.1 dev eda scope link" in the main table.
func setupPod(t *testing.T) {
	t.Helper()
	nstest.Require(t)
	nstest.NS(t, "proxy")
	nstest.NS(t, "lab")
	t.Cleanup(func() {
		for _, c := range [][]string{
			{"ip", "link", "del", "eda"}, {"ip", "link", "del", "eth1"},
			{"ip", "rule", "del", "from", "10.0.0.2/32", "lookup", "100"}, {"ip", "-6", "rule", "del", "from", "fd00:1::2/128", "lookup", "100"},
		} {
			_, _ = nstest.Try("", c...)
		}
	})
	nstest.Veth(t, "", "eda", "10.0.0.2/24", "proxy", "p0", "10.0.0.1/24")
	nstest.Veth(t, "", "eth1", "192.168.0.2/24", "lab", "l0", "192.168.0.1/24")
	nstest.Run(t, "proxy", "ip", "addr", "add", "10.0.1.5/32", "dev", "lo")
	nstest.Run(t, "proxy", "ip", "route", "add", "10.0.0.2", "dev", "p0")
	nstest.Run(t, "proxy", "sysctl", "-w", "net.ipv4.conf.all.rp_filter=0")
	for _, k := range []string{"all", "default", "eda", "eth1"} {
		nstest.Run(t, "", "sysctl", "-w", "net.ipv4.conf."+k+".rp_filter=0")
	}
	nstest.Run(t, "", "ip", "route", "add", "10.0.0.1", "dev", "eda", "scope", "link")
	nstest.Run(t, "", "ip", "route", "replace", "default", "via", "10.0.0.1", "dev", "eda")
}

// proxyPings says whether the proxy, from its address 10.0.1.5, gets an answer from the pod.
func proxyPings(t *testing.T) bool {
	t.Helper()
	_, err := nstest.Try("proxy", "ping", "-c", "1", "-W", "1", "-I", "10.0.1.5", "10.0.0.2")
	return err == nil
}

func mainHoldsEDA(t *testing.T) string {
	t.Helper()
	var left []string
	for _, v := range []string{"-4", "-6"} {
		for _, l := range strings.Split(nstest.Run(t, "", "ip", v, "route", "show", "table", "main"), "\n") {
			if strings.Contains(l, "dev eda") {
				left = append(left, l)
			}
		}
	}
	return strings.Join(left, "; ")
}

const self = "/proc/self/ns/net"

// The bug, without the fix: the author gives a lab interface a default gateway, it replaces the default of the port in main, and
// the replies to the proxy leave through the lab.
func TestNetnsBugWithoutTheFix(t *testing.T) {
	setupPod(t)
	if !proxyPings(t) {
		t.Fatal("precondition: the stand must answer the proxy before the lab gets its gateway")
	}
	nstest.Run(t, "", "ip", "route", "replace", "default", "via", "192.168.0.1", "dev", "eth1")
	if proxyPings(t) {
		t.Fatal("the replies reach the proxy although main's default left the port: the test does not reproduce the bug")
	}
}

// The fix, in the order of a real pod: cni-gate moves the routes in the CNI ADD, the author's gateway comes after.
func TestNetnsLabDefaultGatewayDoesNotTakeTheReplies(t *testing.T) {
	setupPod(t)
	res, err := Ensure(self, "eda", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Error("the first call must report a change")
	}
	if left := mainHoldsEDA(t); left != "" {
		t.Fatalf("main still holds routes through eda: %s", left)
	}
	nstest.Run(t, "", "ip", "route", "replace", "default", "via", "192.168.0.1", "dev", "eth1")
	if !proxyPings(t) {
		t.Fatal("the pod does not answer the proxy with the lab default gateway in main")
	}
	if got := nstest.Run(t, "", "ip", "route", "get", "10.0.0.1"); !strings.Contains(got, "dev eth1") {
		t.Errorf("a connection started by the pod to the proxy side must not use the port, route get: %s", got)
	}
	if got := nstest.Run(t, "", "ip", "route", "get", "8.8.8.8"); !strings.Contains(got, "dev eth1") {
		t.Errorf("the pod's own traffic leaves through the lab: %s", got)
	}

	// Idempotent.
	res, err = Ensure(self, "eda", res.Routes)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Error("the second call must change nothing")
	}
	if n := strings.Count(nstest.Run(t, "", "ip", "rule", "show"), "from 10.0.0.2 lookup 100"); n != 1 {
		t.Errorf("want exactly one rule, got %d", n)
	}
}

// Extended devices can flush the table and delete the rule; Ensure puts them back, from what it saw before.
func TestNetnsRestoresARemovedRuleAndFlushedTable(t *testing.T) {
	setupPod(t)
	res, err := Ensure(self, "eda", nil)
	if err != nil {
		t.Fatal(err)
	}
	nstest.Run(t, "", "ip", "route", "replace", "default", "via", "192.168.0.1", "dev", "eth1")
	nstest.Run(t, "", "ip", "rule", "del", "from", "10.0.0.2/32", "lookup", "100")
	nstest.Run(t, "", "ip", "route", "flush", "table", "100")
	if proxyPings(t) {
		t.Fatal("precondition: without the rule and the table the replies are lost")
	}
	res2, err := Ensure(self, "eda", res.Routes)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Changed || !proxyPings(t) {
		t.Fatalf("not restored (changed=%v)", res2.Changed)
	}

	// The author puts the default back into main through the port: it leaves main again.
	nstest.Run(t, "", "ip", "route", "add", "10.0.0.0/24", "dev", "eda", "scope", "link")
	nstest.Run(t, "", "ip", "route", "add", "default", "via", "10.0.0.1", "dev", "eda", "metric", "5")
	if _, err := Ensure(self, "eda", res2.Routes); err != nil {
		t.Fatal(err)
	}
	if left := mainHoldsEDA(t); left != "" {
		t.Fatalf("main holds routes through eda: %s", left)
	}
	if !proxyPings(t) {
		t.Fatal("publication broke after the routes were moved again")
	}
}

func TestNetnsPortGone(t *testing.T) {
	setupPod(t)
	nstest.Run(t, "", "ip", "link", "del", "eda")
	if _, err := Ensure(self, "eda", nil); err == nil || !strings.Contains(err.Error(), ErrNoPort.Error()) {
		t.Fatalf("want ErrNoPort, got %v", err)
	}
}

func TestNetnsIPv6(t *testing.T) {
	setupPod(t)
	nstest.Run(t, "", "sysctl", "-w", "net.ipv6.conf.eda.disable_ipv6=0")
	nstest.Run(t, "", "ip", "-6", "addr", "add", "fd00:1::2/64", "dev", "eda", "nodad")
	nstest.Run(t, "", "ip", "-6", "route", "add", "default", "via", "fd00:1::1", "dev", "eda")
	nstest.Run(t, "", "ip", "addr", "add", "fd00:2::2/64", "dev", "eth1", "nodad")
	nstest.Run(t, "", "ip", "-6", "route", "replace", "default", "via", "fd00:2::1", "dev", "eth1", "metric", "1")
	if _, err := Ensure(self, "eda", nil); err != nil {
		t.Fatal(err)
	}
	if left := mainHoldsEDA(t); left != "" {
		t.Fatalf("main holds routes through eda: %s", left)
	}
	if got := nstest.Run(t, "", "ip", "-6", "rule", "show"); !strings.Contains(got, "from fd00:1::2 lookup 100") {
		t.Fatalf("no IPv6 rule: %s", got)
	}
	if got := nstest.Run(t, "", "ip", "-6", "route", "show", "table", "100"); !strings.Contains(got, "default via fd00:1::1") {
		t.Fatalf("no IPv6 default in the table: %s", got)
	}
}
