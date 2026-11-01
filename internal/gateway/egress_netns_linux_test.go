//go:build linux

package gateway

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/cybericebox/laboratory/internal/egressproxy"
	"github.com/cybericebox/laboratory/internal/nstest"
)

// farServer is a small HTTP server of the "far" namespace: GET ?n=<bytes> answers that many bytes, POST reads its body; it names the Host it
// was asked for in X-Host (the name the proxy forwarded).
const farServer = `
import http.server, socketserver
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def do_GET(self):
        n = int(self.path.split("=")[-1]) if "=" in self.path else 5
        self.send_response(200); self.send_header("Content-Length", str(n)); self.send_header("X-Host", self.headers.get("Host", "")); self.end_headers()
        left = n
        while left > 0:
            k = min(left, 65536); self.wfile.write(b"x" * k); left -= k
    def do_POST(self):
        left = int(self.headers["Content-Length"])
        while left > 0:
            d = self.rfile.read(min(left, 65536)); left -= len(d)
        self.send_response(200); self.send_header("Content-Length", "2"); self.end_headers(); self.wfile.write(b"ok")
    def log_message(self, *a): pass
socketserver.ThreadingTCPServer.allow_reuse_address = True
socketserver.ThreadingTCPServer(("0.0.0.0", 80), H).serve_forever()
`

// labClient talks HTTP from the lab namespace to any address; it prints "status x-host seconds".
const labClient = `
import http.client, sys, time
ip, host, method, n = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
t = time.time()
try:
    c = http.client.HTTPConnection(ip, 80, timeout=40)
    if method == "POST":
        c.request("POST", "/", body=b"y" * n, headers={"Host": host})
    else:
        c.request("GET", "/?n=%d" % n, headers={"Host": host})
    r = c.getresponse(); body = r.read()
    print(r.status, r.getheader("X-Host") or "-", "%.2f" % (time.time() - t), len(body))
except Exception as e:
    print("error", type(e).__name__, "%.2f" % (time.time() - t), 0)
`

const labDNS = `
import socket, struct, sys
name, typ, server = sys.argv[1], int(sys.argv[2]), sys.argv[3]
pkt = struct.pack(">HHHHHH", 0x1234, 0x0100, 1, 0, 0, 0) + b"".join(bytes([len(p)]) + p.encode() for p in name.split(".")) + b"\0" + struct.pack(">HH", typ, 1)
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(2)
try:
    s.sendto(pkt, (server, 53)); d, _ = s.recvfrom(4096)
    an = struct.unpack(">H", d[6:8])[0]
    print(d[3] & 15, an, socket.inet_ntoa(d[-4:]) if an else "-")
except Exception as e:
    print("error", type(e).__name__)
`

func labGo(t *testing.T, script string, args ...string) string {
	t.Helper()
	out, err := nstest.Try("lab", append([]string{"python3", "-c", script}, args...)...)
	if err != nil {
		t.Fatalf("lab client: %v %s", err, out)
	}
	return strings.TrimSpace(out)
}

// The egress of a lab with the real iptables, tc and sockets, in network namespaces:
//
//	pod (this process)  eth0 10.60.0.2   <-> ns net 10.60.0.1 (a router inside the cloud, an internal address)
//	                                          ns net 198.51.100.2 <-> ns far 198.51.100.1 (the internet: an HTTP server)
//	                    lab1 10.9.0.1    <-> ns lab 10.9.0.2 (a lab device, default route through the pod)
//
// The web proxy and the DNS answerer of the gateway run in this process, the redirects, the filter and the shaping are the real ones.
func TestNetnsGatewayEgress(t *testing.T) {
	nstest.Require(t)
	for _, ns := range []string{"net", "far", "lab"} {
		nstest.NS(t, ns)
	}
	nstest.Veth(t, "", "eth0", "10.60.0.2/24", "net", "n0", "10.60.0.1/24")
	nstest.Veth(t, "net", "f0", "198.51.100.2/24", "far", "g0", "198.51.100.1/24")
	nstest.Veth(t, "", "lab1", "10.9.0.1/24", "lab", "l0", "10.9.0.2/24")
	t.Cleanup(func() {
		for _, dev := range []string{"eth0", "lab1"} {
			_, _ = nstest.Try("", "ip", "link", "del", dev)
		}
	})
	nstest.Run(t, "lab", "ip", "route", "add", "default", "via", "10.9.0.1")
	nstest.Run(t, "far", "ip", "route", "add", "default", "via", "198.51.100.2")
	nstest.Run(t, "", "ip", "route", "add", "default", "via", "10.60.0.1")
	nstest.Run(t, "net", "sysctl", "-w", "net.ipv4.ip_forward=1")
	nstest.Run(t, "", "sysctl", "-w", "net.ipv4.ip_forward=1")
	_, _ = nstest.Try("", "conntrack", "-F")

	srv := exec.Command("ip", "netns", "exec", "far", "python3", "-c", farServer)
	srv.Stderr = os.Stderr
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Process.Kill(); _ = srv.Wait() })
	tlsFar := nstest.Listen(t, "far", "tcp", "198.51.100.1:443")
	sshFar := nstest.Listen(t, "far", "tcp", "198.51.100.1:22")
	udpFar := nstest.Listen(t, "far", "udp", "198.51.100.1:9999")
	for i := 0; i < 50; i++ { // the server listens
		if _, err := net.DialTimeout("tcp", "198.51.100.1:80", 100*time.Millisecond); err == nil || i == 49 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// the gateway: filter, redirects, NAT, the proxy and the DNS answerer
	m, err := NewIPTablesManager("eth0")
	if err != nil {
		t.Fatal(err)
	}
	m.Egress = EgressPorts{HTTP: 3180, TLS: 3443, DNS: 5353}
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
	if err := m.AddEgressServices("lab1"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.DelEgressServices("lab1") })

	labs := egressproxy.NewLabMap()
	_ = labs.Set("10.9.0.0/24", "lab-a")
	resolver, err := egressproxy.NewResolver([]string{"10.60.0.1"}) // nothing answers there: names the gateway was not told fail fast
	if err != nil {
		t.Fatal(err)
	}
	resolver.SetStatic("allowed.test", netip.MustParseAddr("198.51.100.1"))
	stats := egressproxy.NewCollector("gw")
	deny := egressproxy.NewDenylist(egressproxy.DefaultTunnelSuffixes)
	proxy := &egressproxy.Proxy{Labs: labs, Resolver: resolver, Deny: deny, Stats: stats, Log: logr.Discard(), HostNewPerSecond: 1000}
	dns := &egressproxy.DNS{Labs: labs, Resolver: resolver, Deny: deny, Stats: stats, Log: logr.Discard(), QueriesPerSecond: 1000}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	listen := func(port int) net.Listener {
		l, err := egressproxy.Listen(port)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	go func() { _ = proxy.Serve(ctx, listen(3180), false) }()
	go func() { _ = proxy.Serve(ctx, listen(3443), true) }()
	pc, err := net.ListenPacket("udp4", ":5353")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = dns.ServeUDP(ctx, pc) }()
	go func() { _ = dns.ServeTCP(ctx, listen(5353)) }()

	t.Run("a name that is allowed is served, whatever address the lab dialled", func(t *testing.T) {
		// 1.2.3.4 is nobody's address: the lab chose it (an /etc/hosts entry, a DNS server of its own); the proxy uses the name
		for _, ip := range []string{"1.2.3.4", "198.51.100.1", "8.8.8.8"} {
			if out := labGo(t, labClient, ip, "allowed.test", "GET", "5"); !strings.HasPrefix(out, "200 allowed.test") {
				t.Errorf("GET to %s with Host allowed.test: %q", ip, out)
			}
		}
	})

	t.Run("a name the gateway cannot resolve, a bare IP and a tunnel service are refused", func(t *testing.T) {
		// 198.51.100.1 is the real server, and the lab dialled it: still refused, the Host decides
		for host, ip := range map[string]string{"other.test": "198.51.100.1", "198.51.100.1": "198.51.100.1", "abc.ngrok.io": "198.51.100.1"} {
			if out := labGo(t, labClient, ip, host, "GET", "5"); !strings.HasPrefix(out, "error") && !strings.HasPrefix(out, "403") {
				t.Errorf("Host %s was served: %q", host, out)
			}
		}
	})

	t.Run("a TLS flight is forwarded by its server name only", func(t *testing.T) {
		tls := `
import socket, ssl, sys
ctx = ssl.create_default_context(); ctx.check_hostname = False; ctx.verify_mode = ssl.CERT_NONE
name = sys.argv[1]
try:
    s = socket.create_connection(("9.9.9.9", 443), timeout=3)
    ctx.wrap_socket(s, server_hostname=name or None)
except Exception as e:
    pass
`
		_, _ = nstest.Try("lab", "python3", "-c", tls, "allowed.test")
		if !tlsFar.Hit() {
			t.Error("a ClientHello for allowed.test did not reach the server")
		}
		_, _ = nstest.Try("lab", "python3", "-c", tls, "")
		if tlsFar.Hit() {
			t.Error("a ClientHello with no server name reached the server")
		}
		_, _ = nstest.Try("lab", "python3", "-c", tls, "198.51.100.1")
		if tlsFar.Hit() {
			t.Error("a ClientHello for a bare IP reached the server")
		}
	})

	t.Run("nothing but the web ports and ICMP echo leaves", func(t *testing.T) {
		if nstest.Reach(t, "lab", "tcp", "198.51.100.1:22", sshFar) {
			t.Error("SSH left the lab")
		}
		if nstest.Reach(t, "lab", "udp", "198.51.100.1:9999", udpFar) {
			t.Error("a UDP datagram (QUIC, VPN, ...) left the lab")
		}
		if !nstest.Ping(t, "lab", "198.51.100.1") {
			t.Error("a ping does not leave the lab")
		}
	})

	t.Run("every DNS query is answered by the gateway", func(t *testing.T) {
		for _, server := range []string{"8.8.8.8", "10.9.0.1", "198.51.100.1"} {
			if out := labGo(t, labDNS, "allowed.test", "1", server); out != "0 1 198.51.100.1" {
				t.Errorf("A allowed.test asked of %s: %q", server, out)
			}
		}
		if out := labGo(t, labDNS, "allowed.test", "65", "8.8.8.8"); out != "0 0 -" { // HTTPS record: no ECH keys for the browser
			t.Errorf("HTTPS record: %q", out)
		}
		if out := labGo(t, labDNS, "allowed.test", "28", "8.8.8.8"); out != "0 0 -" {
			t.Errorf("AAAA: %q", out)
		}
		if out := labGo(t, labDNS, "x.ngrok.io", "1", "8.8.8.8"); !strings.HasPrefix(out, "3 ") {
			t.Errorf("tunnel service: %q", out)
		}
	})

	t.Run("ping is limited, and the hops inside the cloud stay hidden", func(t *testing.T) {
		time.Sleep(1200 * time.Millisecond)
		out, _ := nstest.Try("lab", "ping", "-c", "30", "-i", "0.05", "-W", "1", "198.51.100.1")
		var got int
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, " received") {
				for _, f := range strings.Split(l, ",") {
					if strings.Contains(f, "received") {
						got, _ = strconv.Atoi(strings.Fields(f)[0])
					}
				}
			}
		}
		if got < 3 || got > 15 {
			t.Errorf("30 pings in 1.5 s against 5 a second: %d answered\n%s", got, out)
		}
		time.Sleep(1200 * time.Millisecond)
		tr, err := nstest.Try("lab", "traceroute", "-I", "-n", "-q", "1", "-w", "1", "-m", "5", "198.51.100.1")
		if err != nil {
			t.Fatalf("traceroute: %v %s", err, tr)
		}
		if !strings.Contains(tr, "10.9.0.1") || !strings.Contains(tr, "198.51.100.1") {
			t.Errorf("the gateway and the destination must show:\n%s", tr)
		}
		if strings.Contains(tr, "10.60.0.1") || strings.Contains(tr, "10.60.0.2") {
			t.Errorf("an internal hop is visible:\n%s", tr)
		}
	})

	t.Run("upload is held to its ceiling, download is not, and the ACKs of a download are not counted", func(t *testing.T) {
		sh := NewShaper()
		if err := sh.SetLimits(50_000, 2_048); err != nil { // 50 Mbit/s down, 2 Mbit/s up
			t.Fatal(err)
		}
		if err := sh.Track("lab1"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sh.Forget("lab1") })
		// the download of 6 MB takes about a second at 50 Mbit/s
		dl := strings.Fields(labGo(t, labClient, "1.2.3.4", "allowed.test", "GET", "6000000"))
		if len(dl) < 4 || dl[0] != "200" {
			t.Fatalf("download: %v", dl)
		}
		if secs, _ := strconv.ParseFloat(dl[2], 64); secs > 4 {
			t.Errorf("6 MB download took %.1f s, ceiling 50 Mbit/s", secs)
		}
		// the upload of 3 MB: the first quarter of a MB goes in the burst of the police, the other 2.75 MB take 11 s at 2 Mbit/s
		ul := strings.Fields(labGo(t, labClient, "1.2.3.4", "allowed.test", "POST", "3000000"))
		if len(ul) < 4 || ul[0] != "200" {
			t.Fatalf("upload: %v", ul)
		}
		secs, _ := strconv.ParseFloat(ul[2], 64)
		if secs < 8 || secs > 17 {
			t.Errorf("3 MB upload took %.1f s; at 2 Mbit/s with a 256 kB burst it should take about 11", secs)
		}
		// with an upload ceiling of 64 kbit/s the pure ACKs of a download would be starved if they were counted
		if err := sh.SetLimits(50_000, 64); err != nil {
			t.Fatal(err)
		}
		dl = strings.Fields(labGo(t, labClient, "1.2.3.4", "allowed.test", "GET", "4000000"))
		if len(dl) < 4 || dl[0] != "200" {
			t.Fatalf("download with a tiny upload ceiling: %v", dl)
		}
		if secs, _ := strconv.ParseFloat(dl[2], 64); secs > 4 {
			t.Errorf("4 MB download with the upload at 64 kbit/s took %.1f s: the ACKs were counted", secs)
		}
	})

	t.Run("only an ACK without payload is exempt from the upload ceiling, and it has a ceiling of its own", func(t *testing.T) {
		sh := NewShaper()
		if err := sh.SetLimits(50_000, 2_048); err != nil {
			t.Fatal(err)
		}
		if err := sh.Track("lab1"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sh.Forget("lab1") })
		// TCP packets of a lab with only the ACK flag, with and without payload, sent raw (nobody answers; they are counted where they enter)
		raw := `
import socket, struct, sys
n, payload, doff = int(sys.argv[1]), int(sys.argv[2]), int(sys.argv[3])
opts = b"\x01\x01\x08\x0a" + b"\0" * 8 if doff == 8 else b""
tcp = struct.pack(">HHIIBBHHH", 40000, 80, 1, 1, doff << 4, 0x10, 65535, 0, 0) + opts + b"z" * payload
ip = struct.pack(">BBHHHBBH4s4s", 0x45, 0, 20 + len(tcp), 1, 0, 64, 6, 0, socket.inet_aton("10.9.0.2"), socket.inet_aton("198.51.100.1"))
s = socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_RAW)
for _ in range(n):
    try: s.sendto(ip + tcp, ("198.51.100.1", 0))
    except OSError: pass
`
		stats := regexp.MustCompile(`Sent (\d+) bytes (\d+) pkt \(dropped (\d+),`)
		// per police of the ingress, in the order of the filters: ACK of 40 bytes, ACK of 52 bytes, the ceiling
		counters := func() (pkts, dropped [3]int, text string) {
			text = nstest.Run(t, "", "tc", "-s", "filter", "show", "dev", "lab1", "parent", "ffff:")
			m := stats.FindAllStringSubmatch(text, -1)
			if len(m) != 3 {
				t.Fatalf("expected the three polices of the ingress, got %d:\n%s", len(m), text)
			}
			for i := range m {
				pkts[i], _ = strconv.Atoi(m[i][2])
				dropped[i], _ = strconv.Atoi(m[i][3])
			}
			return
		}
		send := func(n, payload, doff int) {
			if _, err := nstest.Try("lab", "python3", "-c", raw, strconv.Itoa(n), strconv.Itoa(payload), strconv.Itoa(doff)); err != nil {
				t.Fatal(err)
			}
		}
		// 2 MB of ACKs with 60 bytes of payload each: all of them meet the ceiling (a 1 MB burst passes, the rest is dropped), none the exempt polices
		p0, d0, _ := counters()
		send(20000, 60, 5)
		p1, d1, text := counters()
		if p1[2]-p0[2] < 15000 || d1[2]-d0[2] < 5000 {
			t.Errorf("ACKs with a payload must be counted by the ceiling: %d seen, %d dropped\n%s", p1[2]-p0[2], d1[2]-d0[2], text)
		}
		if p1[0] != p0[0] || p1[1] != p0[1] {
			t.Errorf("ACKs with a payload were taken for payload-less ones: %v -> %v", p0, p1)
		}
		// 30000 ACKs without payload, 40 and 52 bytes: they are the exempt ones, and they meet a ceiling of their own
		p2, d2, _ := counters()
		send(30000, 0, 5)
		send(30000, 0, 8)
		p3, d3, text := counters()
		if p3[0]-p2[0] < 20000 || p3[1]-p2[1] < 20000 || p3[2] != p2[2] {
			t.Errorf("payload-less ACKs must go to the exempt polices only: %v -> %v\n%s", p2, p3, text)
		}
		if d3[0]-d2[0] < 1000 || d3[1]-d2[1] < 1000 {
			t.Errorf("the exempt ACKs have a ceiling of their own: dropped %v -> %v", d2, d3)
		}
	})

	fmt.Fprintln(os.Stderr, "egress netns test done")
}
