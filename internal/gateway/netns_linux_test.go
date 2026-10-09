//go:build linux

package gateway

import (
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/nstest"
)

func TestNetnsHelperProcess(*testing.T) { nstest.HelperProcess() }

// The internet gateway pod with the real iptables, in network namespaces:
//
//	pod (this process)   eth0 203.0.113.2  uplink    <-> ns net 203.0.113.1 (the outside)
//	                     lab1 10.9.0.1     the lab    <-> ns lab 10.9.0.2 (a lab device, default route through the pod)
func TestNetnsGatewayAnswersNothingOnTheLabSide(t *testing.T) {
	nstest.Require(t)
	nstest.NS(t, "net")
	nstest.NS(t, "lab")
	nstest.NS(t, "lab2")
	nstest.Veth(t, "", "eth0", "203.0.113.2/24", "net", "n0", "203.0.113.1/24")
	nstest.Veth(t, "", "lab1", "10.9.0.1/24", "lab", "l0", "10.9.0.2/24")
	nstest.Veth(t, "", "lab2", "10.9.1.1/24", "lab2", "m0", "10.9.1.2/24")
	nstest.Run(t, "lab", "ip", "route", "add", "default", "via", "10.9.0.1")
	nstest.Run(t, "lab2", "ip", "route", "add", "default", "via", "10.9.1.1")
	nstest.Run(t, "net", "ip", "route", "add", "default", "via", "203.0.113.2")
	nstest.Run(t, "", "sysctl", "-w", "net.ipv4.ip_forward=1")

	tcpPod := nstest.Listen(t, "", "tcp", "0.0.0.0:7000")
	udpPod := nstest.Listen(t, "", "udp", "0.0.0.0:7001")
	tcpNet := nstest.Listen(t, "net", "tcp", "0.0.0.0:7400")
	if !nstest.Ping(t, "lab", "10.9.0.1") || !nstest.Reach(t, "lab", "tcp", "10.9.0.1:7000", tcpPod) {
		t.Fatal("control: the lab does not reach the open pod; the topology is wrong")
	}

	m, err := NewIPTablesManager("eth0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.RemoveInput)
	if err := m.SetupFilter(); err != nil {
		t.Fatal(err)
	}
	if err := m.AddAntiSpoof("lab1", "10.9.0.0/24"); err != nil {
		t.Fatal(err)
	}
	if err := m.AddMasquerade("10.9.0.0/24"); err != nil {
		t.Fatal(err)
	}
	for _, l := range [][2]string{{"lab1", "10.9.0.1"}, {"lab2", "10.9.1.1"}} {
		if err := m.AllowPing(l[0], l[1]); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("the lab gets no answer from the pod, except the ping of its own interface address", func(t *testing.T) {
		if !nstest.Ping(t, "lab", "10.9.0.1") || !nstest.Ping(t, "lab2", "10.9.1.1") {
			t.Errorf("a lab cannot ping the gateway's address on its own interface")
		}
		for _, c := range []struct{ ns, dst string }{{"lab", "10.9.1.1"}, {"lab2", "10.9.0.1"}, {"lab", "203.0.113.2"}} {
			if nstest.Ping(t, c.ns, c.dst) {
				t.Errorf("ping %s from %s is answered", c.dst, c.ns)
			}
		}
		for _, dst := range []string{"10.9.0.1", "203.0.113.2"} {
			if nstest.Reach(t, "lab", "tcp", dst+":7000", tcpPod) {
				t.Errorf("tcp %s:7000 from the lab is open", dst)
			}
			if nstest.Reach(t, "lab", "udp", dst+":7001", udpPod) {
				t.Errorf("udp %s:7001 from the lab is open", dst)
			}
		}
	})

	t.Run("forwarding to the outside is unchanged", func(t *testing.T) {
		if !nstest.Reach(t, "lab", "tcp", "203.0.113.1:7400", tcpNet) {
			t.Errorf("a lab device does not reach the outside through the gateway")
		}
		if !nstest.Ping(t, "lab", "203.0.113.1") {
			t.Errorf("ping to the outside through the gateway fails")
		}
	})

	t.Run("source leg stays closed while its guard is replaced", func(t *testing.T) {
		sink := nstest.Listen(t, "net", "udp", "0.0.0.0:7450")
		if err := m.AddMasquerade("10.9.1.0/24"); err != nil {
			t.Fatal(err)
		}
		if err := m.BlockLab("lab1"); err != nil {
			t.Fatal(err)
		}
		m.DelAntiSpoof("lab1", "10.9.0.0/24")
		if nstest.Reach(t, "lab", "udp", "203.0.113.1:7450", sink) {
			t.Fatal("leg escaped while guard absent")
		}
		nstest.Run(t, "lab", "ip", "addr", "add", "10.9.1.77/24", "dev", "l0")
		nstest.Run(t, "lab", "ip", "route", "replace", "203.0.113.1/32", "via", "10.9.0.1", "src", "10.9.1.77")
		if nstest.Reach(t, "lab", "udp", "203.0.113.1:7450", sink) {
			t.Fatal("spoofed another live NAT binding during replacement")
		}
		if err := m.AddAntiSpoof("lab1", "10.9.0.0/24"); err != nil {
			t.Fatal(err)
		}
		if err := m.UnblockLab("lab1"); err != nil {
			t.Fatal(err)
		}
		if nstest.Reach(t, "lab", "udp", "203.0.113.1:7450", sink) {
			t.Fatal("replacement source guard did not block spoof")
		}
		nstest.Run(t, "lab", "ip", "route", "del", "203.0.113.1/32")
		nstest.Run(t, "lab", "ip", "addr", "del", "10.9.1.77/24", "dev", "l0")
		if !nstest.Reach(t, "lab", "udp", "203.0.113.1:7450", sink) {
			t.Fatal("secured replacement did not reopen legitimate traffic")
		}
	})

	t.Run("the pod network still reaches the pod", func(t *testing.T) {
		if !nstest.Ping(t, "net", "203.0.113.2") || !nstest.Reach(t, "net", "tcp", "203.0.113.2:7000", tcpPod) {
			t.Errorf("the uplink does not reach the pod")
		}
	})

	t.Run("DHCP is open on the interface that runs it only", func(t *testing.T) {
		dhcp := nstest.Listen(t, "", "udp", "0.0.0.0:67")
		if nstest.Reach(t, "lab", "udp", "10.9.0.1:67", dhcp) {
			t.Errorf("DHCP is open before it is asked for")
		}
		if err := m.AllowDHCP("lab1"); err != nil {
			t.Fatal(err)
		}
		if !nstest.Reach(t, "lab", "udp", "10.9.0.1:67", dhcp) {
			t.Errorf("DHCP does not reach the server on lab1")
		}
		m.DenyDHCP("lab1")
		if nstest.Reach(t, "lab", "udp", "10.9.0.1:67", dhcp) {
			t.Errorf("DHCP is still open after DenyDHCP")
		}
	})

	t.Run("a second setup changes nothing and removal leaves no policy", func(t *testing.T) {
		before := nstest.Run(t, "", "iptables", "-S", "INPUT")
		if err := m.SetupFilter(); err != nil {
			t.Fatal(err)
		}
		if after := nstest.Run(t, "", "iptables", "-S", "INPUT"); before != after {
			t.Errorf("a second setup changed INPUT:\n--- before\n%s\n--- after\n%s", before, after)
		}
		m.RemoveInput()
		if out := nstest.Run(t, "", "iptables", "-S", "INPUT"); strings.Contains(out, "CICE_INPUT") {
			t.Errorf("the policy is left after RemoveInput:\n%s", out)
		}
	})
}
