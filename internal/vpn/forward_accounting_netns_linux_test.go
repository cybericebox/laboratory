//go:build linux

package vpn

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ti-mo/conntrack"
	"golang.org/x/sys/unix"

	"github.com/cybericebox/laboratory/internal/nstest"
	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
)

func TestNetnsForwardPlanGatesBothDirectionsAndPreservesCounts(t *testing.T) {
	nstest.Require(t)
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
	targetA := nstest.Listen(t, "a", "udp", "0.0.0.0:7900")
	targetP := nstest.Listen(t, "pa", "udp", "0.0.0.0:7901")
	m, err := NewIPTablesManager("wg0")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetupForwardPolicy(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Cleanup)
	plan, err := CompileForwardPlan([]ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.2/32"}}, map[string]LabAccessSnapshot{"a": {VPNCIDR: "10.8.1.0/24", Ready: true, Interface: "lab1"}}, []AccessPolicyRule{{Action: AccessAllow}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplyForwardPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if !nstest.Reach(t, "pa", "udp", "10.8.1.2:7900", targetA) || !nstest.Reach(t, "a", "udp", "10.8.0.2:7901", targetP) {
		t.Fatal("permitted pair did not work in both directions")
	}
	if nstest.Reach(t, "b", "udp", "10.8.0.2:7901", targetP) {
		t.Fatal("unassigned lab initiated traffic to the client")
	}
	nstest.Run(t, "b", "ip", "addr", "add", "10.8.1.99/24", "dev", "b0")
	nstest.Run(t, "b", "ip", "route", "replace", "10.8.0.2/32", "via", "10.8.2.1", "src", "10.8.1.99")
	if nstest.Reach(t, "b", "udp", "10.8.0.2:7901", targetP) {
		t.Fatal("spoofed source bypassed physical lab binding")
	}
	counters, err := m.ReadPairCounters(context.Background())
	if err != nil || counters.Partial || len(counters.Rows) != 1 {
		t.Fatalf("counter snapshot: %+v %v", counters, err)
	}
	c := counters.Rows[0]
	if c.PacketsOut != 1 || c.PacketsIn != 1 || c.Attempts != 1 || c.LabInitiatedAttempts != 1 {
		t.Fatalf("wrong directional/first-flow counts: %+v", c)
	}
	if _, err := m.ApplyForwardPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	after, err := m.ReadPairCounters(context.Background())
	if err != nil || len(after.Rows) != 1 || after.Rows[0] != c {
		t.Fatalf("unchanged plan lost counters: %+v %v", after, err)
	}
	tcpA := nstest.Listen(t, "a", "tcp", "0.0.0.0:7902")
	if !nstest.Reach(t, "pa", "tcp", "10.8.1.2:7902", tcpA) {
		t.Fatal("client TCP did not establish")
	}
	time.Sleep(100 * time.Millisecond)
	tcpSnapshot, err := m.ReadPairCounters(context.Background())
	if err != nil || len(tcpSnapshot.Rows) != 1 {
		t.Fatalf("TCP counters %+v %v", tcpSnapshot, err)
	}
	tcp := tcpSnapshot.Rows[0]
	if tcp.Attempts != 2 || tcp.LabInitiatedAttempts != 1 || tcp.PacketsOut <= c.PacketsOut || tcp.PacketsIn <= c.PacketsIn {
		t.Fatalf("TCP replies counted as lab initiatives or were missed: %+v", tcp)
	}
	// A process restart may preserve the network namespace. Bind private
	// checkpoint identities before enabling the gate and keep its epoch/counts.
	restarted, err := NewIPTablesManager("wg0")
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.SetupForwardPolicy(); err != nil {
		t.Fatal(err)
	}
	restarted.RestoreCounterBindings(tcpSnapshot.Rows)
	if _, err := restarted.ApplyForwardPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	resumed, err := restarted.ReadPairCounters(context.Background())
	if err != nil || len(resumed.Rows) != 1 || resumed.Rows[0] != tcp {
		t.Fatalf("retained namespace lost native epoch: %+v %v", resumed, err)
	}
	m = restarted
	t.Cleanup(m.Cleanup)
	// Keep established sockets open across updates and revoke them in both
	// original orientations. New dial attempts alone cannot prove this gate.
	nstest.Echo(t, "a", "tcp", "0.0.0.0:7910")
	nstest.Echo(t, "a", "udp", "0.0.0.0:7911")
	nstest.Echo(t, "pa", "tcp", "0.0.0.0:7912")
	nstest.Echo(t, "pa", "udp", "0.0.0.0:7913")
	dialogs := []*nstest.Dialog{nstest.Persistent(t, "pa", "tcp", "10.8.1.2:7910"), nstest.Persistent(t, "pa", "udp", "10.8.1.2:7911"), nstest.Persistent(t, "a", "tcp", "10.8.0.2:7912"), nstest.Persistent(t, "a", "udp", "10.8.0.2:7913")}
	for _, d := range dialogs {
		if !d.Exchange(t) {
			t.Fatal("live permitted socket failed")
		}
	}
	prior, err := m.ReadPairCounters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	additional := plan
	additional.Allows = append(append([]ForwardRule(nil), plan.Allows...), ForwardRule{ClientName: "p1", LabName: "b", ClientCIDR: "10.8.0.2/32", LabCIDR: "10.8.2.0/24", LabInterface: "lab2"})
	extra, err := forwardBinding(AccessRule{ClientName: "p1", LabName: "b", SourceCIDR: "10.8.0.2/32", DestinationCIDR: "10.8.2.0/24", LabInterface: "lab2", Action: AccessAllow})
	if err != nil {
		t.Fatal(err)
	}
	additional.Allows[1] = extra
	if _, err := m.ApplyForwardPlan(context.Background(), additional); err != nil {
		t.Fatal(err)
	}
	for _, d := range dialogs {
		if !d.Exchange(t) {
			t.Fatal("other pair update broke established traffic")
		}
	}
	underTraffic, err := m.ReadPairCounters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range underTraffic.Rows {
		if row.BindingID == prior.Rows[0].BindingID {
			found = true
			if row.Epoch != prior.Rows[0].Epoch || row.PacketsOut <= prior.Rows[0].PacketsOut || row.PacketsIn <= prior.Rows[0].PacketsIn {
				t.Fatal("unchanged live pair reset")
			}
		}
	}
	if !found {
		t.Fatal("unchanged binding disappeared")
	}
	collector := flowacct.New(nil, func() flowacct.Topology { return flowacct.Topology{} }, "boot", time.Second)
	m.BeforeRetire = collector.ObserveCounters
	m.AfterRetire = collector.ForgetBindings
	if _, err := m.ApplyForwardPlan(context.Background(), ForwardPlan{}); err != nil {
		t.Fatal(err)
	}
	retired := collector.Snapshot(time.Now())
	if len(retired.Ledger) != 1 || retired.Ledger[0].PacketsOut < int64(tcp.PacketsOut) || retired.Ledger[0].LabInitiatedAttempts != 3 || len(retired.KernelCheckpoints) != 0 {
		t.Fatalf("final retirement lost counts: %+v", retired)
	}
	for _, d := range dialogs {
		if d.Exchange(t) {
			t.Fatal("established socket bypassed revoked gate")
		}
	}
	revoker := NewConntrackRevoker()
	defer revoker.Close()
	removed, err := revoker.Revoke([]string{"10.8.1.0/24", "10.8.2.0/24"}, nil, []string{"10.8.0.2/32"})
	if err != nil || removed < 4 {
		t.Fatalf("real original/reply conntrack removal %d %v", removed, err)
	}
	if nstest.Reach(t, "a", "udp", "10.8.0.2:7901", targetP) || nstest.Reach(t, "pa", "udp", "10.8.1.2:7900", targetA) {
		t.Fatal("revoked pair remained open")
	}
}

func TestNetnsSendUDPHelper(t *testing.T) {
	if os.Getenv("CICE_METER_SEND") != "1" {
		return
	}
	args := os.Args
	count, err := strconv.Atoi(args[len(args)-1])
	if err != nil {
		os.Exit(2)
	}
	c, err := net.Dial("udp", args[len(args)-2])
	if err != nil {
		os.Exit(3)
	}
	defer c.Close()
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Write([]byte("meter")); err != nil {
				os.Exit(4)
			}
		}()
	}
	wg.Wait()
	os.Exit(0)
}

func TestNetnsMeterCountsEveryPacketButOneParallelUDPInitiative(t *testing.T) {
	nstest.Require(t)
	for _, ns := range []string{"p", "l"} {
		nstest.NS(t, ns)
	}
	nstest.Veth(t, "", "wg0", "10.8.0.1/24", "p", "p0", "10.8.0.2/24")
	nstest.Veth(t, "", "lab1", "10.8.1.1/24", "l", "l0", "10.8.1.2/24")
	t.Cleanup(func() {
		for _, dev := range []string{"wg0", "lab1"} {
			_, _ = nstest.Try("", "ip", "link", "del", dev)
		}
	})
	nstest.Run(t, "p", "ip", "route", "add", "default", "via", "10.8.0.1")
	nstest.Run(t, "l", "ip", "route", "add", "default", "via", "10.8.1.1")
	nstest.Run(t, "", "sysctl", "-w", "net.ipv4.ip_forward=1")
	_ = nstest.Listen(t, "l", "udp", "0.0.0.0:7980")
	m, err := NewIPTablesManager("wg0")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetupForwardPolicy(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Cleanup)
	plan, err := CompileForwardPlan([]ClientAccessSnapshot{{Name: "p1", AssignedIP: "10.8.0.2/32"}}, map[string]LabAccessSnapshot{"a": {VPNCIDR: "10.8.1.0/24", Ready: true, Interface: "lab1"}}, []AccessPolicyRule{{Action: AccessAllow}})
	if err != nil {
		t.Fatal(err)
	}
	nstest.Run(t, "", "iptables", "-t", "mangle", "-A", "PREROUTING", "-i", "wg0", "-p", "udp", "--dport", "7980", "-j", "CONNMARK", "--set-xmark", "0x42/0xff")
	if _, err := m.ApplyForwardPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ip", "netns", "exec", "p", os.Args[0], "-test.run=^TestNetnsSendUDPHelper$", "--", "10.8.1.2:7980", "300")
	cmd.Env = append(os.Environ(), "CICE_METER_SEND=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("send: %s %v", out, err)
	}
	time.Sleep(100 * time.Millisecond)
	snapshot, err := m.ReadPairCounters(context.Background())
	if err != nil || len(snapshot.Rows) != 1 {
		t.Fatalf("snapshot %+v %v", snapshot, err)
	}
	r := snapshot.Rows[0]
	if r.PacketsOut != 300 || r.BytesOut != 9900 || r.Attempts != 1 || r.LabInitiatedAttempts != 0 {
		t.Fatalf("300 packets in one UDP flow: %+v", r)
	}
	revoker := NewConntrackRevoker()
	defer revoker.Close()
	ct, err := conntrack.Dial(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ct.Close()
	raw, err := ct.Dump(nil)
	if err != nil {
		t.Fatal(err)
	}
	marked := false
	for _, f := range raw {
		if f.TupleOrig.Proto.DestinationPort == 7980 {
			marked = true
			if f.Mark&0xff != 0x42 {
				t.Fatal("count mark overwrote unrelated bits")
			}
		}
	}
	if !marked {
		t.Fatal("UDP conntrack control missing")
	}
	removed, err := revoker.Revoke([]string{"10.8.1.0/24"}, nil, []string{"10.8.0.2/32"})
	if err != nil || removed < 1 {
		t.Fatalf("ephemeral flow removal %d %v", removed, err)
	}
	vanished, err := m.ReadPairCounters(context.Background())
	if err != nil || vanished.Rows[0] != r {
		t.Fatalf("vanished flow counter lost %+v %v", vanished, err)
	}
	nstest.Run(t, "l", "iptables", "-A", "INPUT", "-p", "tcp", "--dport", "7981", "-j", "DROP")
	syn := exec.Command("ip", "netns", "exec", "p", os.Args[0], "-test.run=^TestNetnsRepeatedSYNHelper$", "--", "10.8.0.2", "10.8.1.2")
	syn.Env = append(os.Environ(), "CICE_SYN_SEND=1")
	if out, err := syn.CombinedOutput(); err != nil {
		t.Fatalf("SYN helper %v %s", err, out)
	}
	final, err := m.ReadPairCounters(context.Background())
	if err != nil || len(final.Rows) != 1 || final.Rows[0].Attempts != 2 || final.Rows[0].PacketsOut != 310 {
		t.Fatalf("repeated SYN counted multiple initiatives %+v %v", final, err)
	}
	if err := m.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterClose, err := m.ReadPairCounters(context.Background())
	if err != nil || afterClose.Rows[0] != final.Rows[0] {
		t.Fatal("quiescence lost final counters")
	}
	if _, err := m.ApplyForwardPlan(context.Background(), plan); err == nil {
		t.Fatal("reconcile reopened shutdown gate")
	}

}

func TestNetnsRepeatedSYNHelper(t *testing.T) {
	if os.Getenv("CICE_SYN_SEND") != "1" {
		return
	}
	args := os.Args
	src := net.ParseIP(args[len(args)-2]).To4()
	dst := net.ParseIP(args[len(args)-1]).To4()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_RAW, unix.IPPROTO_TCP)
	if err != nil {
		os.Exit(2)
	}
	defer unix.Close(fd)
	var source, target [4]byte
	copy(source[:], src)
	copy(target[:], dst)
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: source}); err != nil {
		os.Exit(2)
	}
	tcp := make([]byte, 20)
	binary.BigEndian.PutUint16(tcp, 49000)
	binary.BigEndian.PutUint16(tcp[2:], 7981)
	binary.BigEndian.PutUint32(tcp[4:], 123)
	tcp[12] = 0x50
	tcp[13] = 2
	binary.BigEndian.PutUint16(tcp[14:], 32768)
	pseudo := append(append(append([]byte{}, src...), dst...), 0, 6, 0, 20)
	pseudo = append(pseudo, tcp...)
	sum := uint32(0)
	for i := 0; i < len(pseudo); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(pseudo[i:]))
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(tcp[16:], ^uint16(sum))
	for i := 0; i < 10; i++ {
		if err := unix.Sendto(fd, tcp, 0, &unix.SockaddrInet4{Addr: target}); err != nil {
			os.Exit(3)
		}
	}
	os.Exit(0)
}
