//go:build linux

package vpn

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/nstest"
	"github.com/cybericebox/laboratory/internal/vpn/flowacct"
)

// The nft forwarding of the VPN pod with the real nftables, in network namespaces (as TestNetnsForwardPlanGatesBothDirectionsAndPreservesCounts
// for the iptables relation chains): permitted pairs work in both directions without connection tracking, a revoked pair stops at the next
// packet, counters per pair survive a restart of the process and a retirement, another participant is not touched.
func TestNetnsNFTForwardGatesBothDirectionsWithoutConntrack(t *testing.T) {
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
	targetB := nstest.Listen(t, "b", "udp", "0.0.0.0:7903")
	m, err := NewIPTablesManager("wg0")
	if err != nil {
		t.Fatal(err)
	}
	m.UseNFT(nativeNFT{})
	if err := m.SetupForwardPolicy(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Cleanup)
	// closed gate: nothing passes before a plan
	if nstest.Reach(t, "pa", "udp", "10.8.1.2:7900", targetA) {
		t.Fatal("the gate must be closed before the first plan")
	}
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
	if nstest.Reach(t, "pa", "udp", "10.8.2.2:7903", targetB) || nstest.Reach(t, "b", "udp", "10.8.0.2:7901", targetP) {
		t.Fatal("a lab without a pair was reachable")
	}
	nstest.Run(t, "b", "ip", "addr", "add", "10.8.1.99/24", "dev", "b0")
	nstest.Run(t, "b", "ip", "route", "replace", "10.8.0.2/32", "via", "10.8.2.1", "src", "10.8.1.99")
	if nstest.Reach(t, "b", "udp", "10.8.0.2:7901", targetP) {
		t.Fatal("spoofed source bypassed physical lab binding")
	}
	// no connection tracking for the pair
	if out, _ := nstest.Try("", "conntrack", "-L", "-s", "10.8.0.2"); strings.Contains(out, "10.8.1.2") {
		t.Fatalf("the traffic of the pair was tracked:\n%s", out)
	}
	counters, err := m.ReadPairCounters(context.Background())
	if err != nil || counters.Partial || len(counters.Rows) != 1 {
		t.Fatalf("counter snapshot: %+v %v", counters, err)
	}
	c := counters.Rows[0]
	if c.PacketsOut != 1 || c.PacketsIn != 1 || c.Attempts != 0 || c.LabInitiatedAttempts != 0 {
		t.Fatalf("wrong directional counts (UDP is not an attempt): %+v", c)
	}
	if _, err := m.ApplyForwardPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if after, err := m.ReadPairCounters(context.Background()); err != nil || len(after.Rows) != 1 || after.Rows[0] != c {
		t.Fatalf("unchanged plan lost counters: %+v %v", after, err)
	}
	tcpA := nstest.Listen(t, "a", "tcp", "0.0.0.0:7902")
	tcpP := nstest.Listen(t, "pa", "tcp", "0.0.0.0:7904")
	if !nstest.Reach(t, "pa", "tcp", "10.8.1.2:7902", tcpA) || !nstest.Reach(t, "a", "tcp", "10.8.0.2:7904", tcpP) {
		t.Fatal("TCP did not establish in both directions")
	}
	time.Sleep(100 * time.Millisecond)
	tcpSnapshot, err := m.ReadPairCounters(context.Background())
	if err != nil || len(tcpSnapshot.Rows) != 1 {
		t.Fatalf("TCP counters %+v %v", tcpSnapshot, err)
	}
	tcp := tcpSnapshot.Rows[0]
	if tcp.Attempts != 1 || tcp.LabInitiatedAttempts != 1 || tcp.PacketsOut <= c.PacketsOut || tcp.PacketsIn <= c.PacketsIn || tcp.BytesOut == 0 || tcp.BytesIn == 0 {
		t.Fatalf("TCP attempts (SYN without ACK, per direction) or totals are wrong: %+v", tcp)
	}
	// a restart of the process may keep the network namespace: the counters and their epoch stay
	restarted, err := NewIPTablesManager("wg0")
	if err != nil {
		t.Fatal(err)
	}
	restarted.UseNFT(nativeNFT{})
	if err := restarted.SetupForwardPolicy(); err != nil {
		t.Fatal(err)
	}
	if nstest.Reach(t, "pa", "udp", "10.8.1.2:7900", targetA) {
		t.Fatal("the restarted gate must be closed until the plan is applied")
	}
	restarted.RestoreCounterBindings(tcpSnapshot.Rows)
	if _, err := restarted.ApplyForwardPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	resumed, err := restarted.ReadPairCounters(context.Background())
	if err != nil || len(resumed.Rows) != 1 || resumed.Rows[0].Epoch != tcp.Epoch || resumed.Rows[0].PacketsOut < tcp.PacketsOut {
		t.Fatalf("retained namespace lost the epoch or the counts: %+v %v", resumed, err)
	}
	m = restarted
	t.Cleanup(m.Cleanup)
	// live sockets keep working while another pair is added; then the pair is retired: the next packet is dropped, the totals are kept
	nstest.Echo(t, "a", "tcp", "0.0.0.0:7910")
	nstest.Echo(t, "pa", "tcp", "0.0.0.0:7912")
	dialogs := []*nstest.Dialog{nstest.Persistent(t, "pa", "tcp", "10.8.1.2:7910"), nstest.Persistent(t, "a", "tcp", "10.8.0.2:7912")}
	for _, d := range dialogs {
		if !d.Exchange(t) {
			t.Fatal("live permitted socket failed")
		}
	}
	prior, err := m.ReadPairCounters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	extra, err := forwardBinding(AccessRule{ClientName: "p1", LabName: "b", SourceCIDR: "10.8.0.2/32", DestinationCIDR: "10.8.2.0/24", LabInterface: "lab2", Action: AccessAllow})
	if err != nil {
		t.Fatal(err)
	}
	additional := ForwardPlan{Allows: append(append([]ForwardRule(nil), plan.Allows...), extra), Decisions: plan.Decisions}
	if _, err := m.ApplyForwardPlan(context.Background(), additional); err != nil {
		t.Fatal(err)
	}
	for _, d := range dialogs {
		if !d.Exchange(t) {
			t.Fatal("adding another pair broke an established pair")
		}
	}
	if !nstest.Reach(t, "pa", "udp", "10.8.2.2:7903", targetB) {
		t.Fatal("the added pair did not work")
	}
	underTraffic, err := m.ReadPairCounters(context.Background())
	if err != nil || len(underTraffic.Rows) != 2 {
		t.Fatalf("%+v %v", underTraffic, err)
	}
	for _, row := range underTraffic.Rows {
		if row.BindingID == prior.Rows[0].BindingID && (row.Epoch != prior.Rows[0].Epoch || row.PacketsOut <= prior.Rows[0].PacketsOut) {
			t.Fatal("unchanged live pair reset")
		}
	}
	collector := flowacct.New(nil, func() flowacct.Topology { return flowacct.Topology{} }, "boot", time.Second)
	m.BeforeRetire = collector.ObserveCounters
	m.AfterRetire = collector.ForgetBindings
	if _, err := m.ApplyForwardPlan(context.Background(), ForwardPlan{}); err != nil {
		t.Fatal(err)
	}
	retired := collector.Snapshot(time.Now())
	if len(retired.Ledger) != 2 || len(retired.KernelCheckpoints) != 0 {
		t.Fatalf("final retirement lost counts: %+v", retired)
	}
	for _, d := range dialogs {
		if d.Exchange(t) {
			t.Fatal("an established socket stayed open after the pair was revoked")
		}
	}
	if nstest.Reach(t, "a", "udp", "10.8.0.2:7901", targetP) || nstest.Reach(t, "pa", "udp", "10.8.1.2:7900", targetA) {
		t.Fatal("revoked pair remained open")
	}
	if list, err := (nativeNFT{}).List(context.Background()); err != nil || strings.Contains(string(list), "cibacct:") {
		t.Fatalf("a retired pair left accounting elements: %s %v", list, err)
	}
}
