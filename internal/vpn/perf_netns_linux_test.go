//go:build linux

package vpn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/nstest"
)

// A measurement, not a test: CICE_PERF=1 runs the iptables relation chains with connection tracking and the nft sets without it, at
// 10 participants x 50 labs (500 permitted pairs), through the same pod: the throughput of one TCP flow, the connections per second a
// scan opens, and the CPU the host spent (all CPUs, user + system + softirq) while it ran. The numbers are printed (go test -v).
func TestPerfNetnsIptablesAgainstNFT(t *testing.T) {
	if os.Getenv("CICE_PERF") == "" {
		t.Skip("set CICE_PERF=1")
	}
	nstest.Require(t)
	for _, backend := range []string{"iptables", "nft"} {
		t.Run(backend, func(t *testing.T) { perfRun(t, backend) })
	}
}

func busyJiffies(t *testing.T) (busy, total uint64) {
	t.Helper()
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(strings.SplitN(string(raw), "\n", 2)[0])[1:]
	for i, v := range f {
		n, _ := strconv.ParseUint(v, 10, 64)
		total += n
		if i != 3 && i != 4 { // idle, iowait
			busy += n
		}
	}
	return busy, total
}

func perfRun(t *testing.T, backend string) {
	for _, ns := range []string{"pa", "a"} {
		nstest.NS(t, ns)
	}
	nstest.Veth(t, "", "wg0", "10.8.0.1/24", "pa", "p0", "10.8.0.2/24")
	nstest.Veth(t, "", "lab1", "10.8.1.1/24", "a", "a0", "10.8.1.2/24")
	t.Cleanup(func() {
		for _, dev := range []string{"wg0", "lab1"} {
			_, _ = nstest.Try("", "ip", "link", "del", dev)
		}
		_, _ = nstest.Try("", "conntrack", "-F")
	})
	nstest.Run(t, "pa", "ip", "route", "add", "default", "via", "10.8.0.1")
	nstest.Run(t, "a", "ip", "route", "add", "default", "via", "10.8.1.1")
	nstest.Run(t, "", "sysctl", "-w", "net.ipv4.ip_forward=1")
	m, err := NewIPTablesManager("wg0")
	if err != nil {
		t.Fatal(err)
	}
	if backend == "nft" {
		m.UseNFT(nativeNFT{})
	}
	if err := m.SetupForwardPolicy(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Cleanup)
	var plan ForwardPlan
	// the pair that carries the traffic (client 0, lab 1) is the LAST of the 500: the iptables chain walks its rules in order, so this is its
	// worst case; with CICE_PERF_FIRST=1 it is the first (its best case)
	for c := 9; c >= 0; c-- {
		for l := 50; l >= 1; l-- {
			iface := fmt.Sprintf("lab%d", l)
			r, err := forwardBinding(AccessRule{ClientName: fmt.Sprintf("p%d", c), LabName: fmt.Sprintf("l%d", l), SourceCIDR: fmt.Sprintf("10.8.0.%d/32", c+2), DestinationCIDR: fmt.Sprintf("10.8.%d.0/24", l), LabInterface: iface, Action: AccessAllow})
			if err != nil {
				t.Fatal(err)
			}
			plan.Allows = append(plan.Allows, r)
		}
	}
	if os.Getenv("CICE_PERF_FIRST") != "" {
		for i, j := 0, len(plan.Allows)-1; i < j; i, j = i+1, j-1 {
			plan.Allows[i], plan.Allows[j] = plan.Allows[j], plan.Allows[i]
		}
	}
	start := time.Now()
	if _, err := m.ApplyForwardPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	apply := time.Since(start)
	// one real pair among the 500: p0 -> lab1
	listen := nstest.Run(t, "a", "sh", "-c", "iperf3 -s -D -1 -p 5201 >/dev/null 2>&1; echo started")
	_ = listen
	time.Sleep(500 * time.Millisecond)
	busy0, total0 := busyJiffies(t)
	out, err := nstest.Try("pa", "iperf3", "-c", "10.8.1.2", "-t", "5", "-J")
	if err != nil {
		t.Fatalf("iperf3: %v\n%s", err, out)
	}
	busy1, total1 := busyJiffies(t)
	var res struct {
		End struct {
			SumReceived struct {
				BitsPerSecond float64 `json:"bits_per_second"`
			} `json:"sum_received"`
		} `json:"end"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("iperf3 output: %v", err)
	}
	// small packets: UDP of 64 bytes as fast as the client sends them (the per-packet cost of the gate)
	time.Sleep(300 * time.Millisecond)
	nstest.Run(t, "a", "sh", "-c", "iperf3 -s -D -1 -p 5202 >/dev/null 2>&1; echo started")
	time.Sleep(500 * time.Millisecond)
	busyU0, totalU0 := busyJiffies(t)
	uout, err := nstest.Try("pa", "iperf3", "-c", "10.8.1.2", "-p", "5202", "-u", "-l", "64", "-b", "0", "-t", "5", "-J")
	if err != nil {
		t.Fatalf("iperf3 udp: %v\n%s", err, uout)
	}
	busyU1, totalU1 := busyJiffies(t)
	var ures struct {
		End struct {
			Sum struct {
				Packets int64   `json:"packets"`
				Seconds float64 `json:"seconds"`
				Lost    int64   `json:"lost_packets"`
			} `json:"sum"`
		} `json:"end"`
	}
	_ = json.Unmarshal([]byte(uout), &ures)
	// a scan: parallel short connections to a closed-after-accept service
	script := `import socket,threading,time
def srv():
    s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("0.0.0.0",7000)); s.listen(4096)
    while True:
        c,_=s.accept(); c.close()
threading.Thread(target=srv,daemon=True).start()
time.sleep(30)`
	nstest.Run(t, "a", "sh", "-c", "python3 -c '"+script+"' >/dev/null 2>&1 & sleep 1; echo ok")
	scan := `import socket,threading,time
n=[0]; stop=time.time()+5
def w():
    while time.time()<stop:
        try:
            s=socket.create_connection(("10.8.1.2",7000),timeout=1); s.close(); n[0]+=1
        except Exception: pass
ts=[threading.Thread(target=w) for _ in range(16)]
[t.start() for t in ts]; [t.join() for t in ts]
print(n[0]/5.0)`
	busy2, total2 := busyJiffies(t)
	rate, err := nstest.Try("pa", "python3", "-c", scan)
	if err != nil {
		t.Fatalf("scan: %v %s", err, rate)
	}
	busy3, total3 := busyJiffies(t)
	entries, _ := os.ReadFile("/proc/sys/net/netfilter/nf_conntrack_count")
	t.Logf("RESULT-UDP64 %-8s %.0f pps received (%d sent, %d lost), cpu busy %.0f%%", backend, float64(ures.End.Sum.Packets-ures.End.Sum.Lost)/ures.End.Sum.Seconds, ures.End.Sum.Packets, ures.End.Sum.Lost, 100*float64(busyU1-busyU0)/float64(totalU1-totalU0))
	t.Logf("RESULT %-8s pairs=500 apply=%s iperf=%.2f Gbit/s (cpu busy during iperf %.0f%%) scan=%s conn/s (cpu busy during scan %.0f%%) conntrack-entries=%s",
		backend, apply.Round(time.Millisecond), res.End.SumReceived.BitsPerSecond/1e9, 100*float64(busy1-busy0)/float64(total1-total0),
		strings.TrimSpace(rate), 100*float64(busy3-busy2)/float64(total3-total2), strings.TrimSpace(string(entries)))
}
