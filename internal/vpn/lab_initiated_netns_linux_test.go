//go:build linux

package vpn

import (
	"context"
	"testing"

	"github.com/cybericebox/laboratory/internal/nstest"
)

// By default a lab cannot start a connection towards a client, and the replies to what the client started still pass (TCP, UDP, ICMP). The
// switch of the lab changes this at the next packet, without a restart and without losing the counters of the pair. Run for both firewalls.
func TestNetnsLabCannotStartConnectionsToClientsUnlessAllowed(t *testing.T) {
	for name, useNFT := range map[string]bool{"iptables": false, "nft": true} {
		t.Run(name, func(t *testing.T) { labInitiatedScenario(t, useNFT) })
	}
}

func labInitiatedScenario(t *testing.T, useNFT bool) {
	nstest.Require(t)
	for _, ns := range []string{"pa", "a"} {
		nstest.NS(t, ns)
	}
	nstest.Veth(t, "", "wg0", "10.8.0.1/24", "pa", "p0", "10.8.0.2/24")
	nstest.Veth(t, "", "lab1", "10.8.1.1/24", "a", "a0", "10.8.1.2/24")
	t.Cleanup(func() {
		for _, dev := range []string{"wg0", "lab1"} {
			_, _ = nstest.Try("", "ip", "link", "del", dev)
		}
	})
	for _, e := range [][2]string{{"pa", "10.8.0.1"}, {"a", "10.8.1.1"}} {
		nstest.Run(t, e[0], "ip", "route", "add", "default", "via", e[1])
	}
	nstest.Run(t, "", "sysctl", "-w", "net.ipv4.ip_forward=1")
	_, _ = nstest.Try("", "conntrack", "-F")
	udpLab := nstest.Listen(t, "a", "udp", "0.0.0.0:7910")
	tcpLab := nstest.Listen(t, "a", "tcp", "0.0.0.0:7911")
	udpClient := nstest.Listen(t, "pa", "udp", "0.0.0.0:7912")
	tcpClient := nstest.Listen(t, "pa", "tcp", "0.0.0.0:7913")

	m, err := NewIPTablesManager("wg0")
	if err != nil {
		t.Fatal(err)
	}
	if useNFT {
		m.UseNFT(nativeNFT{})
	}
	if err := m.SetupForwardPolicy(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Cleanup)
	apply := func(labInitiated bool) {
		t.Helper()
		plan, err := CompileForwardPlan([]ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.2/32"}},
			map[string]LabAccessSnapshot{"a": {VPNCIDR: "10.8.1.0/24", Ready: true, Interface: "lab1", LabInitiated: labInitiated}}, []AccessPolicyRule{{Action: AccessAllow}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.ApplyForwardPlan(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	clientToLab := func() {
		t.Helper()
		// the client starts the connection: the replies of the lab must pass (a TCP handshake needs the SYN-ACK)
		if !nstest.Reach(t, "pa", "tcp", "10.8.1.2:7911", tcpLab) || !nstest.Reach(t, "pa", "udp", "10.8.1.2:7910", udpLab) || !nstest.Ping(t, "pa", "10.8.1.2") {
			t.Fatal("a connection the client started, with its replies, did not work")
		}
	}
	labToClient := func(want bool) {
		t.Helper()
		got := nstest.Reach(t, "a", "tcp", "10.8.0.2:7913", tcpClient) && nstest.Reach(t, "a", "udp", "10.8.0.2:7912", udpClient)
		if want && !got {
			t.Fatal("the lab may start connections but they did not pass")
		}
		if !want {
			if nstest.Reach(t, "a", "tcp", "10.8.0.2:7913", tcpClient) {
				t.Fatal("the lab started a TCP connection to the client")
			}
			if nstest.Reach(t, "a", "udp", "10.8.0.2:7912", udpClient) {
				t.Fatal("the lab sent a new UDP flow to the client")
			}
			if nstest.Ping(t, "a", "10.8.0.2") {
				t.Fatal("the lab pinged the client")
			}
		}
	}

	apply(false) // the default
	clientToLab()
	labToClient(false)

	first, err := m.ReadPairCounters(context.Background())
	if err != nil || len(first.Rows) != 1 {
		t.Fatalf("counters: %+v %v", first, err)
	}

	apply(true) // the lab may start connections
	clientToLab()
	labToClient(true)
	if !nstest.Ping(t, "a", "10.8.0.2") {
		t.Fatal("with the switch on the lab can ping the client")
	}
	open, err := m.ReadPairCounters(context.Background())
	if err != nil || len(open.Rows) != 1 || open.Rows[0].Epoch != first.Rows[0].Epoch {
		t.Fatalf("the switch must not change the pair's counters: %+v %v", open, err)
	}

	apply(false) // and off again: the next packet of a lab-started flow is refused
	_, _ = nstest.Try("", "conntrack", "-F")
	clientToLab()
	labToClient(false)
}
