//go:build linux

package vpn

import (
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cybericebox/laboratory/internal/nstest"
	"github.com/cybericebox/laboratory/internal/podinput"
)

func TestNetnsHelperProcess(*testing.T) { nstest.HelperProcess() }

// The VPN pod with the real iptables, in network namespaces:
//
//	pod (this process)   eth0 10.244.0.2  uplink          <-> ns up   10.244.0.1
//	                     lab1 10.8.100.1  the lab          <-> ns lab  10.8.100.2 (a lab device)
//	                     wg0  10.8.0.1    the tunnel       <-> ns wgsw, a switch with the participants pa 10.8.0.2 and pb 10.8.0.3
//
// A participant is a /32 that reaches everything through the pod, like a WireGuard peer: pa to pb is routed by the pod (and must
// be dropped), not switched.
func TestNetnsVPNPodIsATransparentGateway(t *testing.T) {
	nstest.Require(t)
	for _, ns := range []string{"up", "lab", "lab2", "wgsw", "pa", "pb"} {
		nstest.NS(t, ns)
	}
	nstest.Veth(t, "", "eth0", "10.244.0.2/24", "up", "u0", "10.244.0.1/24")
	nstest.Veth(t, "", "lab1", "10.8.100.1/24", "lab", "l0", "10.8.100.2/24")
	nstest.Veth(t, "", "lab2", "10.8.101.1/24", "lab2", "m0", "10.8.101.2/24")
	nstest.Veth(t, "", "wg0", "10.8.0.1/24", "wgsw", "vw", "")
	nstest.Veth(t, "wgsw", "va", "", "pa", "pa0", "")
	nstest.Veth(t, "wgsw", "vb", "", "pb", "pb0", "")
	nstest.Run(t, "wgsw", "ip", "link", "add", "brw", "type", "bridge")
	nstest.Run(t, "wgsw", "ip", "link", "set", "brw", "up")
	for _, dev := range []string{"vw", "va", "vb"} {
		nstest.Run(t, "wgsw", "ip", "link", "set", dev, "master", "brw")
	}
	for _, p := range []struct{ ns, dev, ip string }{{"pa", "pa0", "10.8.0.2"}, {"pb", "pb0", "10.8.0.3"}} {
		nstest.Run(t, p.ns, "ip", "addr", "add", p.ip+"/32", "dev", p.dev)
		nstest.Run(t, p.ns, "ip", "route", "add", "10.8.0.1", "dev", p.dev)
		nstest.Run(t, p.ns, "ip", "route", "add", "10.8.0.0/24", "via", "10.8.0.1")
		nstest.Run(t, p.ns, "ip", "route", "add", "10.8.100.0/24", "via", "10.8.0.1")
		nstest.Run(t, p.ns, "ip", "route", "add", "10.244.0.0/24", "via", "10.8.0.1")
	}
	nstest.Run(t, "lab", "ip", "route", "add", "10.8.0.0/24", "via", "10.8.100.1")
	nstest.Run(t, "lab", "ip", "route", "add", "10.244.0.0/24", "via", "10.8.100.1")
	nstest.Run(t, "lab", "ip", "route", "add", "10.8.101.0/24", "via", "10.8.100.1")
	nstest.Run(t, "lab2", "ip", "route", "add", "10.8.100.0/24", "via", "10.8.101.1")
	nstest.Run(t, "lab2", "ip", "route", "add", "10.8.0.0/24", "via", "10.8.101.1")
	nstest.Run(t, "", "sysctl", "-w", "net.ipv4.ip_forward=1")

	// Targets on the pod itself, on every kind of address it has.
	tcpPod := nstest.Listen(t, "", "tcp", "0.0.0.0:7000")
	udpPod := nstest.Listen(t, "", "udp", "0.0.0.0:7001")
	// Devices behind the pod.
	tcpLab := nstest.Listen(t, "lab", "tcp", "0.0.0.0:7300")
	tcpA := nstest.Listen(t, "pa", "tcp", "0.0.0.0:7200")
	tcpB := nstest.Listen(t, "pb", "tcp", "0.0.0.0:7100")
	udpB := nstest.Listen(t, "pb", "udp", "0.0.0.0:7101")

	// Control: before the policy the pod answers on all its addresses, so the checks below really measure the rules.
	if !nstest.Ping(t, "lab", "10.8.100.1") || !nstest.Reach(t, "lab", "tcp", "10.8.100.1:7000", tcpPod) {
		t.Fatal("control: the lab does not reach the open pod; the topology is wrong")
	}

	m, err := NewIPTablesManager("wg0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Cleanup)
	ipv6, err := m.ProtectInput("eth0", "10.8.0.1", ProbePort)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing is forwarded from the first moment: the policy is DROP before any FORWARD rule exists.
	if out := nstest.Run(t, "", "iptables", "-S", "FORWARD"); !strings.Contains(out, "-P FORWARD DROP") {
		t.Fatalf("FORWARD policy is not DROP right after ProtectInput:\n%s", out)
	}
	if b, err := os.ReadFile(podinput.ConntrackHelpers); err == nil && strings.TrimSpace(string(b)) != "0" {
		t.Fatalf("conntrack helpers are on (%q): a RELATED connection could be opened by a payload", b)
	}
	if _, err := m.GuardWireGuardPort("eth0", 51820); err != nil {
		t.Fatal(err)
	}
	if err := m.SetupForwardPolicy(); err != nil {
		t.Fatal(err)
	}
	if err := m.AllowLabToClients("lab1", "10.8.100.0/24"); err != nil {
		t.Fatal(err)
	}
	for _, l := range [][2]string{{"lab1", "10.8.100.1"}, {"lab2", "10.8.101.1"}} {
		if err := m.AllowPing(l[0], l[1]); err != nil {
			t.Fatal(err)
		}
	}
	_, wgNet, _ := net.ParseCIDR("10.8.0.0/24")
	probe, err := startProbe(wgNet, ProbePort, "help@example.org", "wg0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(probe.Close)
	if out := nstest.Run(t, "", "iptables", "-S", "INPUT"); !strings.Contains(out, "-j CICE_INPUT") {
		t.Fatalf("INPUT does not jump to the policy:\n%s", out)
	}
	if ipv6 {
		if out := nstest.Run(t, "", "ip6tables", "-S", "INPUT"); !strings.Contains(out, "-j CICE_INPUT") {
			t.Fatalf("ip6tables INPUT does not jump to the policy:\n%s", out)
		}
	}

	t.Run("lab to the pod itself is blocked, except the ping of its own interface address", func(t *testing.T) {
		if !nstest.Ping(t, "lab", "10.8.100.1") || !nstest.Ping(t, "lab2", "10.8.101.1") {
			t.Errorf("a lab cannot ping the pod's address on its own interface")
		}
		for _, c := range []struct{ ns, dst, why string }{
			{"lab", "10.8.101.1", "the address of another lab's interface"},
			{"lab2", "10.8.100.1", "the address of another lab's interface"},
			{"lab", "10.8.0.1", "the tunnel address"},
			{"lab", "10.244.0.2", "the uplink address"},
		} {
			if nstest.Ping(t, c.ns, c.dst) {
				t.Errorf("ping %s from %s (%s) is answered", c.dst, c.ns, c.why)
			}
		}
		for _, dst := range []string{"10.8.100.1", "10.8.0.1", "10.244.0.2"} {
			if nstest.Reach(t, "lab", "tcp", dst+":7000", tcpPod) {
				t.Errorf("tcp %s:7000 from the lab is open", dst)
			}
			if nstest.Reach(t, "lab", "udp", dst+":7001", udpPod) {
				t.Errorf("udp %s:7001 from the lab is open", dst)
			}
			if nstest.Dial(t, "lab", "tcp", dst+":8088") {
				t.Errorf("the status page at %s:8088 answers the lab", dst)
			}
		}
	})

	t.Run("the ping of the gateway is rate-limited", func(t *testing.T) {
		out, _ := nstest.Try("lab", "ping", "-c", "100", "-i", "0.01", "-W", "1", "10.8.100.1")
		sum := regexp.MustCompile(`(\d+) received`).FindStringSubmatch(out)
		if sum == nil {
			t.Fatalf("no ping summary:\n%s", out)
		}
		if n, _ := strconv.Atoi(sum[1]); n < 10 || n > 60 {
			t.Errorf("%d of 100 fast echo requests answered, want a few (burst 20 plus 10 per second)", n)
		}
	})

	t.Run("participants reach the status page and nothing else of the pod", func(t *testing.T) {
		if !nstest.Dial(t, "pa", "tcp", "10.8.0.1:8088") {
			t.Errorf("the status page does not answer a participant")
		}
		if nstest.Dial(t, "pa", "tcp", "10.8.100.1:8088") || nstest.Dial(t, "pa", "tcp", "10.244.0.2:8088") {
			t.Errorf("the status page answers on another address of the pod")
		}
		for _, dst := range []string{"10.8.0.1", "10.8.100.1", "10.244.0.2"} {
			if nstest.Ping(t, "pa", dst) {
				t.Errorf("ping %s from a participant is answered", dst)
			}
			if nstest.Reach(t, "pa", "tcp", dst+":7000", tcpPod) {
				t.Errorf("tcp %s:7000 from a participant is open", dst)
			}
			if nstest.Reach(t, "pa", "udp", dst+":7001", udpPod) {
				t.Errorf("udp %s:7001 from a participant is open", dst)
			}
		}
	})

	t.Run("the uplink and the pod's own connections stay open", func(t *testing.T) {
		if !nstest.Ping(t, "up", "10.244.0.2") || !nstest.Reach(t, "up", "tcp", "10.244.0.2:7000", tcpPod) ||
			!nstest.Reach(t, "up", "udp", "10.244.0.2:7001", udpPod) {
			t.Errorf("the pod network does not reach the pod")
		}
		// The pod connects out to a device of the lab: the replies come back through INPUT as ESTABLISHED.
		if !nstest.Dial(t, "", "tcp", "10.8.100.2:7300") || !tcpLab.Hit() {
			t.Errorf("the pod cannot connect to a lab device (replies are dropped)")
		}
	})

	t.Run("participant to participant through the pod is dropped", func(t *testing.T) {
		if nstest.Reach(t, "pa", "tcp", "10.8.0.3:7100", tcpB) {
			t.Errorf("tcp from a participant to another one went through")
		}
		if nstest.Reach(t, "pa", "udp", "10.8.0.3:7101", udpB) {
			t.Errorf("udp from a participant to another one went through")
		}
		if nstest.Ping(t, "pa", "10.8.0.3") {
			t.Errorf("ping from a participant to another one is answered")
		}
	})

	t.Run("lab to participants works both ways", func(t *testing.T) {
		// A device of the lab starts the connection; the participant's answer comes back by conntrack.
		if !nstest.Reach(t, "lab", "tcp", "10.8.0.2:7200", tcpA) {
			t.Errorf("a lab device does not reach a participant")
		}
		if !nstest.Reach(t, "lab", "tcp", "10.8.0.3:7100", tcpB) {
			t.Errorf("a lab device does not reach another participant")
		}
		if !nstest.Ping(t, "lab", "10.8.0.2") {
			t.Errorf("ping from the lab to a participant fails")
		}
		// The participant starts it: only with an access rule.
		if nstest.Reach(t, "pa", "tcp", "10.8.100.2:7300", tcpLab) {
			t.Errorf("a participant reached the lab without an access rule")
		}
		rule := AccessRule{ClientName: "a", LabName: "l", SourceCIDR: "10.8.0.2/32", DestinationCIDR: "10.8.100.0/24", Action: AccessAllow}
		if err := m.ReplaceAccessRules([]AccessRule{rule}); err != nil {
			t.Fatal(err)
		}
		if !nstest.Reach(t, "pa", "tcp", "10.8.100.2:7300", tcpLab) || !nstest.Ping(t, "pa", "10.8.100.2") {
			t.Errorf("a participant with access does not reach the lab")
		}
		if nstest.Reach(t, "pb", "tcp", "10.8.100.2:7300", tcpLab) {
			t.Errorf("a participant without access reached the lab")
		}
	})

	t.Run("a lab never sends as another lab", func(t *testing.T) {
		udpA := nstest.Listen(t, "pa", "udp", "0.0.0.0:7201")
		nstest.Run(t, "lab", "ip", "addr", "add", "10.8.99.5/24", "dev", "l0")
		nstest.Run(t, "lab", "ip", "route", "replace", "10.8.0.2/32", "via", "10.8.100.1", "src", "10.8.100.2")
		if !nstest.Reach(t, "lab", "udp", "10.8.0.2:7201", udpA) {
			t.Fatalf("control: a datagram from the lab's own address does not arrive")
		}
		nstest.Run(t, "lab", "ip", "route", "replace", "10.8.0.2/32", "via", "10.8.100.1", "src", "10.8.99.5")
		if nstest.Reach(t, "lab", "udp", "10.8.0.2:7201", udpA) {
			t.Errorf("a datagram with a source outside the lab's subnet went through")
		}
	})

	t.Run("DHCP is open on the interface that runs it only", func(t *testing.T) {
		dhcp := nstest.Listen(t, "", "udp", "0.0.0.0:67")
		if nstest.Reach(t, "lab", "udp", "10.8.100.1:67", dhcp) {
			t.Errorf("DHCP is open before it is asked for")
		}
		if err := m.AllowDHCP("lab1"); err != nil {
			t.Fatal(err)
		}
		if !nstest.Reach(t, "lab", "udp", "10.8.100.1:67", dhcp) {
			t.Errorf("DHCP does not reach the server on lab1")
		}
		if nstest.Reach(t, "pa", "udp", "10.8.0.1:67", dhcp) {
			t.Errorf("DHCP is open to the tunnel")
		}
		m.DenyDHCP("lab1")
		if nstest.Reach(t, "lab", "udp", "10.8.100.1:67", dhcp) {
			t.Errorf("DHCP is still open after DenyDHCP")
		}
	})

	t.Run("a second setup changes nothing and cleanup leaves nothing", func(t *testing.T) {
		if err := m.ReplaceAccessRules(nil); err != nil {
			t.Fatal(err)
		}
		before := nstest.Run(t, "", "iptables", "-S")
		if _, err := m.ProtectInput("eth0", "10.8.0.1", ProbePort); err != nil {
			t.Fatal(err)
		}
		if _, err := m.GuardWireGuardPort("eth0", 51820); err != nil {
			t.Fatal(err)
		}
		if err := m.SetupForwardPolicy(); err != nil {
			t.Fatal(err)
		}
		if err := m.AllowLabToClients("lab1", "10.8.100.0/24"); err != nil {
			t.Fatal(err)
		}
		after := nstest.Run(t, "", "iptables", "-S")
		if before != after {
			t.Errorf("a second setup changed the rules:\n--- before\n%s\n--- after\n%s", before, after)
		}
		m.Cleanup()
		left := nstest.Run(t, "", "iptables", "-S")
		for _, line := range strings.Split(strings.TrimSpace(left), "\n") {
			if strings.HasPrefix(line, "-A") || strings.HasPrefix(line, "-N") {
				t.Errorf("a rule is left after Cleanup: %s", line)
			}
		}
	})
}
