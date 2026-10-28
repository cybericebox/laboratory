//go:build linux

package fastpath

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// The programs run on hand-built frames with BPF_PROG_TEST_RUN, and once live between network namespaces:
//
//	ns bpfcl  cl0 192.0.2.2 (the client)  <->  ext0 192.0.2.1 (the external device; entry 192.0.2.1:31820)
//	ns bpfsv  sv0 10.0.0.2  (the server)  <->  srv0 10.0.0.1  (towards the server; the demux socket's source 10.0.0.1:51820)
//
// The test process is the node, in a network namespace of its own (`unshare -n`, make test-bpf). Both neighbours are
// permanent, so the route lookups of the programs succeed without ARP.

const (
	tcxNext     = ^uint32(0) // TCX_NEXT (-1)
	tcxPass     = 0
	tcxDrop     = 2
	tcxRedirect = 7
	xdpDrop     = 1
	xdpPass     = 2

	replyMark = 0x57470000
	metaMagic = 0x57474d31
	metaLen   = 20

	serverIndex uint32 = 0x0a0b0c0d // receiver index of client -> server packets
	clientIndex uint32 = 0x01020304 // receiver index of server -> client packets
)

var (
	client = netip.MustParseAddrPort("192.0.2.2:45000")
	entry  = netip.MustParseAddrPort("192.0.2.1:31820")
	probe  = netip.MustParseAddrPort("10.0.0.1:51820")
	server = netip.MustParseAddrPort("10.0.0.2:51820")

	extMAC    = net.HardwareAddr{2, 0, 0, 0, 0, 1}
	clientMAC = net.HardwareAddr{2, 0, 0, 0, 0, 2}
	srvMAC    = net.HardwareAddr{2, 0, 0, 0, 0, 0x11}
	serverMAC = net.HardwareAddr{2, 0, 0, 0, 0, 0x12}
)

type topo struct{ ext, srv int }

func requireBPF(t *testing.T) {
	t.Helper()
	if os.Getenv("CICE_BPF_TESTS") != "1" {
		t.Skip("set CICE_BPF_TESTS=1 (make test-bpf) to run the eBPF program tests")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("needs ip")
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ifaces {
		if i.Name != "lo" && i.Flags&net.FlagUp != 0 {
			t.Fatalf("the test would change a real network namespace (found %s up): run it with `unshare -n`", i.Name)
		}
	}
}

func ip(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
		t.Fatalf("ip %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func setup(t *testing.T) topo {
	t.Helper()
	requireBPF(t)
	ip(t, "link", "set", "lo", "up")
	for _, ns := range []string{"bpfcl", "bpfsv"} {
		ip(t, "netns", "add", ns)
		t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
		ip(t, "-n", ns, "link", "set", "lo", "up")
	}
	for _, c := range []struct {
		dev, cidr, ns, peer, peerCIDR, neigh string
		mac, peerMAC                         net.HardwareAddr
	}{
		{"ext0", "192.0.2.1/24", "bpfcl", "cl0", "192.0.2.2/24", "192.0.2.2", extMAC, clientMAC},
		{"srv0", "10.0.0.1/24", "bpfsv", "sv0", "10.0.0.2/24", "10.0.0.2", srvMAC, serverMAC},
	} {
		ip(t, "link", "add", c.dev, "address", c.mac.String(), "type", "veth", "peer", "name", c.peer, "address", c.peerMAC.String(), "netns", c.ns)
		t.Cleanup(func() { _ = exec.Command("ip", "link", "del", c.dev).Run() })
		ip(t, "addr", "add", c.cidr, "dev", c.dev)
		ip(t, "link", "set", c.dev, "up")
		ip(t, "-n", c.ns, "addr", "add", c.peerCIDR, "dev", c.peer)
		ip(t, "-n", c.ns, "link", "set", c.peer, "up")
		ip(t, "neigh", "replace", c.neigh, "lladdr", c.peerMAC.String(), "dev", c.dev, "nud", "permanent")
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	ext, err := net.InterfaceByName("ext0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := net.InterfaceByName("srv0")
	if err != nil {
		t.Fatal(err)
	}
	return topo{ext: ext.Index, srv: srv.Index}
}

func load(t *testing.T, cfg WgFastpathWgCfg) *WgFastpathObjects {
	t.Helper()
	spec, err := LoadWgFastpath()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range spec.Maps {
		m.Pinning = ebpf.PinNone // the loader pins; the tests need fresh maps
	}
	var objs WgFastpathObjects
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		var ve *ebpf.VerifierError
		if errors.As(err, &ve) {
			t.Fatalf("%+v", ve)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = objs.Close() })
	if err := objs.WgCfg.Put(uint32(0), cfg); err != nil {
		t.Fatal(err)
	}
	if err := objs.WgEntries.Put(addr(entry), uint8(1)); err != nil {
		t.Fatal(err)
	}
	return &objs
}

func defaultCfg(tp topo) WgFastpathWgCfg {
	return WgFastpathWgCfg{TtlNs: uint64(3 * time.Minute), ExtIfindex: uint32(tp.ext)}
}

func be32(a netip.Addr) uint32 {
	b := a.As4()
	return binary.NativeEndian.Uint32(b[:])
}

func be16(p uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], p)
	return binary.NativeEndian.Uint16(b[:])
}

func addr(a netip.AddrPort) WgFastpathWgAddr {
	return WgFastpathWgAddr{Ip: be32(a.Addr()), Port: be16(a.Port())}
}

func monoNow(t *testing.T) uint64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		t.Fatal(err)
	}
	return uint64(ts.Nano())
}

// fwd is one direction of the session client <-> server as the demux writes it: both directions name the client -> server
// index as their session (the owner of the bucket and last_seen), and a reply must be sent to the probe source.
func fwd(t *testing.T, exp, src, dst netip.AddrPort, toClient bool, replyIf int) WgFastpathWgFwd {
	f := WgFastpathWgFwd{
		ExpIp: be32(exp.Addr()), ExpPort: be16(exp.Port()),
		NewSrcIp: be32(src.Addr()), NewSrcPort: be16(src.Port()),
		DstIp: be32(dst.Addr()), DstPort: be16(dst.Port()),
		Session: serverIndex,
	}
	if toClient {
		f.Flags = 1
		f.ReplyIfindex = uint32(replyIf)
		f.ViaIp, f.ViaPort = be32(probe.Addr()), be16(probe.Port())
	} else {
		f.LastSeenNs = monoNow(t)
	}
	return f
}

func owner(t *testing.T, objs *WgFastpathObjects) WgFastpathWgFwd {
	t.Helper()
	var f WgFastpathWgFwd
	if err := objs.WgSessions.Lookup(WgFastpathWgKey{Index: serverIndex}, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// putSessions writes both directions of the session client <-> server, as the demux does when a handshake completes.
func putSessions(t *testing.T, objs *WgFastpathObjects, tp topo) {
	t.Helper()
	if err := objs.WgSessions.Put(WgFastpathWgKey{Index: serverIndex}, fwd(t, client, probe, server, false, 0)); err != nil {
		t.Fatal(err)
	}
	if err := objs.WgSessions.Put(WgFastpathWgKey{Index: clientIndex}, fwd(t, server, entry, client, true, tp.srv)); err != nil {
		t.Fatal(err)
	}
}

func transport(index uint32) []byte {
	b := make([]byte, 16+48)
	b[0] = 4
	binary.LittleEndian.PutUint32(b[4:], index)
	for i := 8; i < len(b); i++ {
		b[i] = byte(i * 7)
	}
	return b
}

func initiation(sender uint32) []byte {
	b := make([]byte, 148)
	b[0] = 1
	binary.LittleEndian.PutUint32(b[4:], sender)
	for i := 8; i < len(b); i++ {
		b[i] = byte(i * 3)
	}
	return b
}

type frame struct {
	src, dst  netip.AddrPort
	payload   []byte
	noUDPSum  bool
	fragOff   uint16
	ipOptions bool
	proto     byte
}

func onesSum(b []byte, sum uint32) uint32 {
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	return sum
}

func fold(s uint32) uint16 {
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return uint16(s)
}

func pseudoSum(src, dst netip.Addr, udpLen int) uint32 {
	s, d := src.As4(), dst.As4()
	p := append(append(s[:], d[:]...), 0, unix.IPPROTO_UDP, byte(udpLen>>8), byte(udpLen))
	return onesSum(p, 0)
}

func (f frame) bytes() []byte {
	var opts []byte
	if f.ipOptions {
		opts = []byte{1, 1, 1, 0} // NOP NOP NOP EOL
	}
	proto := f.proto
	if proto == 0 {
		proto = unix.IPPROTO_UDP
	}
	udp := make([]byte, 8+len(f.payload))
	binary.BigEndian.PutUint16(udp[0:], f.src.Port())
	binary.BigEndian.PutUint16(udp[2:], f.dst.Port())
	binary.BigEndian.PutUint16(udp[4:], uint16(len(udp)))
	copy(udp[8:], f.payload)
	if !f.noUDPSum {
		sum := ^fold(onesSum(udp, pseudoSum(f.src.Addr(), f.dst.Addr(), len(udp))))
		if sum == 0 {
			sum = 0xffff
		}
		binary.BigEndian.PutUint16(udp[6:], sum)
	}
	iph := make([]byte, 20+len(opts))
	iph[0] = 0x40 | byte(len(iph)/4)
	binary.BigEndian.PutUint16(iph[2:], uint16(len(iph)+len(udp)))
	binary.BigEndian.PutUint16(iph[4:], 0x1234)
	binary.BigEndian.PutUint16(iph[6:], f.fragOff)
	iph[8] = 64
	iph[9] = proto
	s, d := f.src.Addr().As4(), f.dst.Addr().As4()
	copy(iph[12:], s[:])
	copy(iph[16:], d[:])
	copy(iph[20:], opts)
	binary.BigEndian.PutUint16(iph[10:], ^fold(onesSum(iph, 0)))
	eth := append(append(append([]byte{}, extMAC...), clientMAC...), 0x08, 0x00)
	return append(append(eth, iph...), udp...)
}

func toServerFrame() frame { return frame{src: client, dst: entry, payload: transport(serverIndex)} }
func toClientFrame() frame { return frame{src: server, dst: probe, payload: transport(clientIndex)} }

// skbCtx is struct __sk_buff, the context of a tc program run (only mark, ingress_ifindex and ifindex are set here).
type skbCtx struct {
	Len, PktType, Mark, QueueMapping, Protocol, VlanPresent, VlanTci, VlanProto, Priority, IngressIfindex, Ifindex, TcIndex uint32
	Cb                                                                                                                      [5]uint32
	Hash, TcClassid, Data, DataEnd, NapiID, Family, RemoteIP4, LocalIP4                                                     uint32
	RemoteIP6, LocalIP6                                                                                                     [4]uint32
	RemotePort, LocalPort, DataMeta                                                                                         uint32
	FlowKeys, Tstamp                                                                                                        uint64
	WireLen, GsoSegs                                                                                                        uint32
	Sk                                                                                                                      uint64
	GsoSize                                                                                                                 uint32
	TstampType                                                                                                              uint8
	_                                                                                                                       [3]uint8
	Hwtstamp                                                                                                                uint64
}

// xdpCtx is struct xdp_md.
type xdpCtx struct{ Data, DataEnd, DataMeta, IngressIfindex, RxQueueIndex, EgressIfindex uint32 }

type tcRun struct {
	ret  uint32
	out  []byte
	mark uint32
}

func runTC(t *testing.T, prog *ebpf.Program, in []byte, ctx skbCtx) tcRun {
	t.Helper()
	var ctxOut skbCtx
	opts := &ebpf.RunOptions{Data: in, DataOut: make([]byte, len(in)+256), Context: ctx, ContextOut: &ctxOut}
	ret, err := prog.Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	return tcRun{ret: ret, out: opts.DataOut, mark: ctxOut.Mark}
}

type xdpRun struct {
	ret       uint32
	meta, out []byte
}

func runXDP(t *testing.T, prog *ebpf.Program, in []byte) xdpRun {
	t.Helper()
	var ctxOut xdpCtx
	opts := &ebpf.RunOptions{Data: in, DataOut: make([]byte, len(in)+256), Context: xdpCtx{DataEnd: uint32(len(in))}, ContextOut: &ctxOut}
	ret, err := prog.Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	return xdpRun{ret: ret, meta: opts.DataOut[:ctxOut.Data], out: opts.DataOut[ctxOut.Data:]}
}

func ingress(tp topo, mark uint32) skbCtx {
	return skbCtx{Mark: mark, IngressIfindex: uint32(tp.ext), Ifindex: uint32(tp.ext)}
}

func replyCtx(ifindex int) skbCtx {
	return skbCtx{IngressIfindex: uint32(ifindex), Ifindex: uint32(ifindex)}
}

func stat(t *testing.T, objs *WgFastpathObjects, s WgFastpathWgStat) uint64 {
	t.Helper()
	var per []uint64
	if err := objs.WgStats.Lookup(uint32(s), &per); err != nil {
		t.Fatal(err)
	}
	var n uint64
	for _, v := range per {
		n += v
	}
	return n
}

func wantStats(t *testing.T, objs *WgFastpathObjects, want map[WgFastpathWgStat]uint64) {
	t.Helper()
	for s := WgFastpathWgStat(0); s < WgFastpathWgStatWG_STAT_MAX; s++ {
		if got := stat(t, objs, s); got != want[s] {
			t.Errorf("counter %d = %d, want %d", s, got, want[s])
		}
	}
}

// checkRewrite asserts a frame left with the new tuple and MACs, every other byte as it came, and valid checksums.
func checkRewrite(t *testing.T, in, out []byte, src, dst netip.AddrPort, smac, dmac net.HardwareAddr) {
	t.Helper()
	if len(out) != len(in) {
		t.Fatalf("frame length %d, want %d", len(out), len(in))
	}
	if !bytes.Equal(out[0:6], dmac) || !bytes.Equal(out[6:12], smac) {
		t.Errorf("MACs dst %x src %x, want %x %x", out[0:6], out[6:12], []byte(dmac), []byte(smac))
	}
	s, d := src.Addr().As4(), dst.Addr().As4()
	if !bytes.Equal(out[26:30], s[:]) || !bytes.Equal(out[30:34], d[:]) {
		t.Errorf("IP %v -> %v, want %v -> %v", net.IP(out[26:30]), net.IP(out[30:34]), src.Addr(), dst.Addr())
	}
	if sp, dp := binary.BigEndian.Uint16(out[34:]), binary.BigEndian.Uint16(out[36:]); sp != src.Port() || dp != dst.Port() {
		t.Errorf("UDP ports %d -> %d, want %d -> %d", sp, dp, src.Port(), dst.Port())
	}
	// Everything but the addresses, ports and checksums is unchanged; the WireGuard payload above all.
	for _, r := range [][2]int{{12, 24}, {38, 40}, {42, len(in)}} {
		if !bytes.Equal(out[r[0]:r[1]], in[r[0]:r[1]]) {
			t.Errorf("bytes %d..%d changed: %x, want %x", r[0], r[1], out[r[0]:r[1]], in[r[0]:r[1]])
		}
	}
	if fold(onesSum(out[14:34], 0)) != 0xffff {
		t.Errorf("IP checksum %#04x invalid", binary.BigEndian.Uint16(out[24:]))
	}
	inSum, outSum := binary.BigEndian.Uint16(in[40:]), binary.BigEndian.Uint16(out[40:])
	switch {
	case inSum == 0 && outSum != 0:
		t.Errorf("a zero UDP checksum became %#04x", outSum)
	case inSum != 0 && fold(onesSum(out[34:], pseudoSum(src.Addr(), dst.Addr(), len(out)-34))) != 0xffff:
		t.Errorf("UDP checksum %#04x invalid", outSum)
	}
}

func TestForwardsKnownIndex(t *testing.T) {
	for _, zero := range []bool{false, true} {
		t.Run(map[bool]string{false: "udp checksum", true: "no udp checksum"}[zero], func(t *testing.T) {
			tp := setup(t)
			objs := load(t, defaultCfg(tp))
			putSessions(t, objs, tp)
			var before WgFastpathWgFwd
			if err := objs.WgSessions.Lookup(WgFastpathWgKey{Index: serverIndex}, &before); err != nil {
				t.Fatal(err)
			}

			req := toServerFrame()
			req.noUDPSum = zero
			in := req.bytes()
			r := runTC(t, objs.WgTcIngress, in, ingress(tp, 0x12340e00))
			if r.ret != tcxRedirect {
				t.Fatalf("to server: verdict %d, want TCX_REDIRECT", int32(r.ret))
			}
			checkRewrite(t, in, r.out, probe, server, srvMAC, serverMAC)
			// Cilium's host magic in its nibble, the rest of the mark kept: the packet enters Cilium as the demux socket's would.
			if r.mark != 0x12340c00 {
				t.Errorf("to server: mark %#x, want 0x12340c00", r.mark)
			}

			rep := toClientFrame()
			rep.noUDPSum = zero
			in = rep.bytes()
			r = runTC(t, objs.WgTcReply, in, replyCtx(tp.srv))
			if r.ret != tcxRedirect {
				t.Fatalf("to client: verdict %d, want TCX_REDIRECT", int32(r.ret))
			}
			checkRewrite(t, in, r.out, entry, client, extMAC, clientMAC)
			if r.mark != replyMark {
				t.Errorf("to client: mark %#x, want %#x", r.mark, replyMark)
			}

			wantStats(t, objs, map[WgFastpathWgStat]uint64{
				WgFastpathWgStatWG_STAT_FORWARDED_TO_SERVER: 1,
				WgFastpathWgStatWG_STAT_FORWARDED_TO_CLIENT: 1,
			})
			var after WgFastpathWgFwd
			if err := objs.WgSessions.Lookup(WgFastpathWgKey{Index: serverIndex}, &after); err != nil {
				t.Fatal(err)
			}
			if after.LastSeenNs <= before.LastSeenNs {
				t.Errorf("last_seen %d not moved past %d", after.LastSeenNs, before.LastSeenNs)
			}
		})
	}
}

func checkUntouched(t *testing.T, name string, r tcRun, in []byte) {
	t.Helper()
	if r.ret != tcxNext {
		t.Errorf("%s: verdict %d, want TCX_NEXT", name, int32(r.ret))
	}
	if !bytes.Equal(r.out, in) {
		t.Errorf("%s: frame changed", name)
	}
}

func TestPassesUnknownIndex(t *testing.T) {
	tp := setup(t)
	objs := load(t, defaultCfg(tp))
	putSessions(t, objs, tp)

	req := toServerFrame()
	req.payload = transport(0xdeadbeef)
	in := req.bytes()
	checkUntouched(t, "to server", runTC(t, objs.WgTcIngress, in, ingress(tp, 0)), in)
	// The index of the other direction is not one of this direction.
	req.payload = transport(clientIndex)
	in = req.bytes()
	checkUntouched(t, "to server, a client index", runTC(t, objs.WgTcIngress, in, ingress(tp, 0)), in)
	rep := toClientFrame()
	rep.payload = transport(serverIndex)
	in = rep.bytes()
	checkUntouched(t, "to client, a server index", runTC(t, objs.WgTcReply, in, replyCtx(tp.srv)), in)

	wantStats(t, objs, map[WgFastpathWgStat]uint64{WgFastpathWgStatWG_STAT_PASSED_MISS: 3})
}

func TestPassesWrongSource(t *testing.T) {
	tp := setup(t)
	objs := load(t, defaultCfg(tp))
	putSessions(t, objs, tp)

	// A roamed client (new port) and a stranger: the slow path decides (roam at most once per roamInterval).
	for _, src := range []string{"192.0.2.2:45001", "192.0.2.9:45000"} {
		req := toServerFrame()
		req.src = netip.MustParseAddrPort(src)
		in := req.bytes()
		checkUntouched(t, "to server from "+src, runTC(t, objs.WgTcIngress, in, ingress(tp, 0)), in)
	}
	rep := toClientFrame()
	rep.src = netip.MustParseAddrPort("10.0.0.3:51820")
	in := rep.bytes()
	checkUntouched(t, "to client from another pod", runTC(t, objs.WgTcReply, in, replyCtx(tp.srv)), in)
	// The right server, but arriving on another device than the session's.
	in = toClientFrame().bytes()
	checkUntouched(t, "to client on another device", runTC(t, objs.WgTcReply, in, replyCtx(tp.ext)), in)

	wantStats(t, objs, map[WgFastpathWgStat]uint64{WgFastpathWgStatWG_STAT_PASSED_SOURCE: 4})
}

func TestPassesStale(t *testing.T) {
	tp := setup(t)
	cfg := defaultCfg(tp)
	cfg.TtlNs = 1
	objs := load(t, cfg)
	putSessions(t, objs, tp)
	time.Sleep(time.Millisecond)

	in := toServerFrame().bytes()
	checkUntouched(t, "to server", runTC(t, objs.WgTcIngress, in, ingress(tp, 0)), in)
	in = toClientFrame().bytes()
	checkUntouched(t, "to client", runTC(t, objs.WgTcReply, in, replyCtx(tp.srv)), in)
	x := runXDP(t, objs.WgXdp, toServerFrame().bytes())
	if x.ret != xdpPass || len(x.meta) != 0 || !bytes.Equal(x.out, toServerFrame().bytes()) {
		t.Errorf("xdp: verdict %d, meta %d bytes; want XDP_PASS, untouched", x.ret, len(x.meta))
	}

	// XDP leaves the counting to tc, which sees the frame next.
	wantStats(t, objs, map[WgFastpathWgStat]uint64{WgFastpathWgStatWG_STAT_PASSED_STALE: 2})
}

func TestPassesWithoutRoute(t *testing.T) {
	tp := setup(t)
	objs := load(t, defaultCfg(tp))
	nowhere := netip.MustParseAddrPort("203.0.113.9:51820")
	if err := objs.WgSessions.Put(WgFastpathWgKey{Index: serverIndex}, fwd(t, client, probe, nowhere, false, 0)); err != nil {
		t.Fatal(err)
	}
	in := toServerFrame().bytes()
	checkUntouched(t, "to server", runTC(t, objs.WgTcIngress, in, ingress(tp, 0)), in)
	wantStats(t, objs, map[WgFastpathWgStat]uint64{WgFastpathWgStatWG_STAT_PASSED_ROUTE: 1})
}

func TestDropsOverRate(t *testing.T) {
	tp := setup(t)
	cfg := defaultCfg(tp)
	cfg.RatePerS, cfg.Burst = 1, 3

	t.Run("tc", func(t *testing.T) {
		objs := load(t, cfg)
		putSessions(t, objs, tp)
		in := toServerFrame().bytes()
		for i, want := range []uint32{tcxRedirect, tcxRedirect, tcxRedirect, tcxDrop} {
			if r := runTC(t, objs.WgTcIngress, in, ingress(tp, 0)); r.ret != want {
				t.Fatalf("packet %d: verdict %d, want %d", i+1, int32(r.ret), int32(want))
			}
		}
		wantStats(t, objs, map[WgFastpathWgStat]uint64{
			WgFastpathWgStatWG_STAT_FORWARDED_TO_SERVER: 3,
			WgFastpathWgStatWG_STAT_DROPPED_RATE:        1,
		})
	})
	t.Run("xdp", func(t *testing.T) {
		objs := load(t, cfg)
		putSessions(t, objs, tp)
		in := toServerFrame().bytes()
		for i, want := range []uint32{xdpPass, xdpPass, xdpPass, xdpDrop} {
			if x := runXDP(t, objs.WgXdp, in); x.ret != want || (want == xdpPass) != (len(x.meta) == metaLen) {
				t.Fatalf("packet %d: verdict %d with %d bytes of meta, want %d", i+1, x.ret, len(x.meta), want)
			}
		}
		wantStats(t, objs, map[WgFastpathWgStat]uint64{WgFastpathWgStatWG_STAT_DROPPED_RATE: 1})
	})
}

// The byte bucket of a session: 92-byte packets (20 IP + 8 UDP + 64 WireGuard), a burst of 200 bytes and a rate too small to refill
// within the test pass two packets and drop the third, both in tc and in XDP.
func TestDropsOverByteRate(t *testing.T) {
	tp := setup(t)
	cfg := defaultCfg(tp)
	cfg.ByteRate, cfg.ByteBurst = 1, 200

	t.Run("tc", func(t *testing.T) {
		objs := load(t, cfg)
		putSessions(t, objs, tp)
		in := toServerFrame().bytes()
		for i, want := range []uint32{tcxRedirect, tcxRedirect, tcxDrop} {
			if r := runTC(t, objs.WgTcIngress, in, ingress(tp, 0)); r.ret != want {
				t.Fatalf("packet %d: verdict %d, want %d", i+1, int32(r.ret), int32(want))
			}
		}
		wantStats(t, objs, map[WgFastpathWgStat]uint64{
			WgFastpathWgStatWG_STAT_FORWARDED_TO_SERVER: 2,
			WgFastpathWgStatWG_STAT_DROPPED_RATE:        1,
		})
	})
	t.Run("xdp", func(t *testing.T) {
		objs := load(t, cfg)
		putSessions(t, objs, tp)
		in := toServerFrame().bytes()
		for i, want := range []uint32{xdpPass, xdpPass, xdpDrop} {
			if x := runXDP(t, objs.WgXdp, in); x.ret != want {
				t.Fatalf("packet %d: verdict %d, want %d", i+1, x.ret, want)
			}
		}
	})
}

// The lengths in the headers are the sender's word. A packet whose IP or UDP length is not the length it really has (trailing bytes
// the limiters would not see, a header that claims less) is not forwarded and is charged nothing; the slow path gets it and counts
// the bytes it really reads.
func TestLengthMismatchIsNotForwardedOrCharged(t *testing.T) {
	tp := setup(t)
	cfg := defaultCfg(tp)
	cfg.ByteRate, cfg.ByteBurst = 1, 1000
	cfg.ClassWindowNs, cfg.ClassWindowBytes = uint64(time.Hour), 1000
	good := toServerFrame().bytes()
	trailing := append(append([]byte{}, good...), make([]byte, 600)...)
	ipShort := append([]byte{}, good...)
	binary.BigEndian.PutUint16(ipShort[16:], uint16(len(good)-14-10)) // IP total length: 10 bytes less than the packet has
	udpShort := append([]byte{}, good...)
	binary.BigEndian.PutUint16(udpShort[14+20+4:], uint16(len(good)-14-20-10)) // UDP length: 10 bytes less
	udpLong := append([]byte{}, good...)
	binary.BigEndian.PutUint16(udpLong[14+20+4:], uint16(len(good)-14-20+10))
	for name, in := range map[string][]byte{"trailing bytes": trailing, "short IP length": ipShort, "short UDP length": udpShort, "long UDP length": udpLong} {
		t.Run(name, func(t *testing.T) {
			objs := load(t, cfg)
			putSessions(t, objs, tp)
			checkUntouched(t, "tc", runTC(t, objs.WgTcIngress, in, ingress(tp, 0)), in)
			if x := runXDP(t, objs.WgXdp, in); x.ret != xdpPass || len(x.meta) != 0 {
				t.Fatalf("xdp: verdict %d with %d bytes of meta, the frame must pass untouched", x.ret, len(x.meta))
			}
			if o := owner(t, objs); o.ByteTokens != 0 || o.ByteRefillNs != 0 {
				t.Fatalf("a refused packet was charged to the session: %+v", o)
			}
			var c WgFastpathWgClass
			if err := objs.WgClass.Lookup(uint32(0), &c); err != nil || c.Used != 0 {
				t.Fatalf("a refused packet was charged to the class: used %d (%v)", c.Used, err)
			}
			wantStats(t, objs, map[WgFastpathWgStat]uint64{})
		})
	}
	// the same packet with honest lengths goes, and is charged its real length (92 bytes)
	objs := load(t, cfg)
	putSessions(t, objs, tp)
	if r := runTC(t, objs.WgTcIngress, good, ingress(tp, 0)); r.ret != tcxRedirect {
		t.Fatalf("the honest packet: verdict %d", int32(r.ret))
	}
	var c WgFastpathWgClass
	if err := objs.WgClass.Lookup(uint32(0), &c); err != nil || c.Used != uint64(len(good)-14) {
		t.Fatalf("charged %d, the packet has %d bytes after the Ethernet header (%v)", c.Used, len(good)-14, err)
	}
}

// A packet over the byte bucket takes nothing from the packet bucket (and the other way round), and both directions of a session
// draw from the same byte bucket.
func TestByteBucketIsSharedByBothDirections(t *testing.T) {
	tp := setup(t)
	cfg := defaultCfg(tp)
	cfg.ByteRate, cfg.ByteBurst = 1, 200
	objs := load(t, cfg)
	putSessions(t, objs, tp)
	if r := runTC(t, objs.WgTcIngress, toServerFrame().bytes(), ingress(tp, 0)); r.ret != tcxRedirect {
		t.Fatalf("to server: verdict %d", int32(r.ret))
	}
	if r := runTC(t, objs.WgTcReply, toClientFrame().bytes(), replyCtx(tp.srv)); r.ret != tcxRedirect {
		t.Fatalf("to client: verdict %d", int32(r.ret))
	}
	if r := runTC(t, objs.WgTcReply, toClientFrame().bytes(), replyCtx(tp.srv)); r.ret != tcxDrop {
		t.Fatalf("the third packet of the session, in either direction, is over its 200 bytes: verdict %d", int32(r.ret))
	}
}

// The node's class window: all sessions together, 200 bytes in a window that does not end during the test.
func TestDropsOverTheClassWindow(t *testing.T) {
	tp := setup(t)
	cfg := defaultCfg(tp)
	cfg.ClassWindowNs, cfg.ClassWindowBytes = uint64(time.Hour), 200
	objs := load(t, cfg)
	putSessions(t, objs, tp)
	in := toServerFrame().bytes()
	for i, want := range []uint32{tcxRedirect, tcxRedirect, tcxDrop} {
		if r := runTC(t, objs.WgTcIngress, in, ingress(tp, 0)); r.ret != want {
			t.Fatalf("packet %d: verdict %d, want %d", i+1, int32(r.ret), int32(want))
		}
	}
	var c WgFastpathWgClass
	if err := objs.WgClass.Lookup(uint32(0), &c); err != nil || c.Used != 184 {
		t.Fatalf("a refused packet gives its bytes back: used %d (%v)", c.Used, err)
	}
}

// Loaded with byte_limiter = 0 (the fallback), the byte settings do nothing and the packet bucket is the only limit.
func TestByteLimiterOffIgnoresTheByteSettings(t *testing.T) {
	tp := setup(t)
	cfg := defaultCfg(tp)
	cfg.ByteRate, cfg.ByteBurst = 1, 1
	cfg.ClassWindowNs, cfg.ClassWindowBytes = uint64(time.Hour), 1
	spec, err := LoadWgFastpath()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range spec.Maps {
		m.Pinning = ebpf.PinNone
	}
	if err := spec.Variables[WgFastpathVarByteLimiter].Set(uint32(0)); err != nil {
		t.Fatal(err)
	}
	var objs WgFastpathObjects
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = objs.Close() })
	if err := objs.WgCfg.Put(uint32(0), cfg); err != nil {
		t.Fatal(err)
	}
	if err := objs.WgEntries.Put(addr(entry), uint8(1)); err != nil {
		t.Fatal(err)
	}
	putSessions(t, &objs, tp)
	in := toServerFrame().bytes()
	for i := 0; i < 5; i++ {
		if r := runTC(t, objs.WgTcIngress, in, ingress(tp, 0)); r.ret != tcxRedirect {
			t.Fatalf("packet %d: verdict %d, the limiter is off", i+1, int32(r.ret))
		}
	}
}

func TestRecordsEntryOfHandshake(t *testing.T) {
	tp := setup(t)
	objs := load(t, defaultCfg(tp))

	in := frame{src: client, dst: entry, payload: initiation(0x55)}.bytes()
	checkUntouched(t, "initiation", runTC(t, objs.WgTcIngress, in, ingress(tp, 0)), in)
	var got WgFastpathWgAddr
	if err := objs.WgEntrySeen.Lookup(addr(client), &got); err != nil {
		t.Fatalf("no entry recorded for the client: %v", err)
	}
	if got != addr(entry) {
		t.Errorf("recorded %+v, want %+v", got, addr(entry))
	}

	// Not to an entry address, or not a whole initiation: nothing recorded.
	other := netip.MustParseAddrPort("192.0.2.3:46000")
	for _, f := range []frame{
		{src: other, dst: netip.MustParseAddrPort("192.0.2.1:31821"), payload: initiation(0x56)},
		{src: other, dst: entry, payload: initiation(0x57)[:100]},
	} {
		in := f.bytes()
		checkUntouched(t, "other initiation", runTC(t, objs.WgTcIngress, in, ingress(tp, 0)), in)
	}
	if err := objs.WgEntrySeen.Lookup(addr(other), &got); !errors.Is(err, ebpf.ErrKeyNotExist) {
		t.Errorf("recorded %+v for a packet that is not an initiation to an entry (err %v)", got, err)
	}
	wantStats(t, objs, nil)
}

func ignoredFrames() map[string]frame {
	nonEntry := toServerFrame()
	nonEntry.dst = netip.MustParseAddrPort("192.0.2.1:31821")
	moreFragments := toServerFrame()
	moreFragments.fragOff = 0x2000
	laterFragment := toServerFrame()
	laterFragment.fragOff = 0x0010
	options := toServerFrame()
	options.ipOptions = true
	short := toServerFrame()
	short.payload = short.payload[:31]
	notUDP := toServerFrame()
	notUDP.proto = unix.IPPROTO_TCP
	return map[string]frame{
		"not to an entry": nonEntry, "more fragments": moreFragments, "a later fragment": laterFragment,
		"ip options": options, "shorter than a transport message": short, "not udp": notUDP,
	}
}

func TestIgnoresNonEntryAndFragments(t *testing.T) {
	tp := setup(t)
	objs := load(t, defaultCfg(tp))
	putSessions(t, objs, tp)

	for name, f := range ignoredFrames() {
		in := f.bytes()
		checkUntouched(t, name, runTC(t, objs.WgTcIngress, in, ingress(tp, 0)), in)
		if x := runXDP(t, objs.WgXdp, in); x.ret != xdpPass || len(x.meta) != 0 || !bytes.Equal(x.out, in) {
			t.Errorf("%s: xdp verdict %d, meta %d bytes; want XDP_PASS, untouched", name, x.ret, len(x.meta))
		}
	}
	// Not ours at all: no counter moves.
	wantStats(t, objs, nil)
}

func TestXDPSameAsTC(t *testing.T) {
	tp := setup(t)
	objs := load(t, defaultCfg(tp))
	putSessions(t, objs, tp)

	wrongSource := toServerFrame()
	wrongSource.src = netip.MustParseAddrPort("192.0.2.2:45001")
	unknown := toServerFrame()
	unknown.payload = transport(0xdeadbeef)
	noSum := toServerFrame()
	noSum.noUDPSum = true
	cases := map[string]struct {
		f       frame
		forward bool
	}{
		"forwarded":             {toServerFrame(), true},
		"forwarded, no udp sum": {noSum, true},
		"unknown index":         {unknown, false},
		"wrong source":          {wrongSource, false},
	}
	for name, f := range ignoredFrames() {
		cases[name] = struct {
			f       frame
			forward bool
		}{f, false}
	}
	for name, c := range cases {
		in := c.f.bytes()
		r := runTC(t, objs.WgTcIngress, in, ingress(tp, 0))
		x := runXDP(t, objs.WgXdp, in)
		if !c.forward {
			checkUntouched(t, name, r, in)
			if x.ret != xdpPass || len(x.meta) != 0 || !bytes.Equal(x.out, in) {
				t.Errorf("%s: xdp verdict %d, meta %d bytes; want XDP_PASS, untouched", name, x.ret, len(x.meta))
			}
			continue
		}
		if r.ret != tcxRedirect || x.ret != xdpPass {
			t.Fatalf("%s: tc verdict %d, xdp verdict %d; want TCX_REDIRECT, XDP_PASS", name, int32(r.ret), x.ret)
		}
		// The same IP and UDP headers and payload; XDP keeps the MACs (tc sets them after the route lookup).
		if !bytes.Equal(x.out[14:], r.out[14:]) {
			t.Errorf("%s: xdp rewrote %x, tc %x", name, x.out[14:42], r.out[14:42])
		}
		if !bytes.Equal(x.out[:14], in[:14]) {
			t.Errorf("%s: xdp changed the Ethernet header", name)
		}
		// The metadata for tc: magic, the original tuple and UDP checksum.
		if len(x.meta) != metaLen {
			t.Fatalf("%s: %d bytes of meta, want %d", name, len(x.meta), metaLen)
		}
		m := x.meta
		if binary.NativeEndian.Uint32(m[0:]) != metaMagic || !bytes.Equal(m[4:8], in[26:30]) || !bytes.Equal(m[8:12], in[30:34]) ||
			!bytes.Equal(m[12:16], in[34:38]) || !bytes.Equal(m[16:18], in[40:42]) {
			t.Errorf("%s: meta %x does not hold the magic and the original tuple of %x", name, m, in[26:42])
		}
	}
}

func TestEgressGuard(t *testing.T) {
	tp := setup(t)
	objs := load(t, defaultCfg(tp))

	ours := frame{src: entry, dst: client, payload: transport(clientIndex)}.bytes()
	slow := frame{src: netip.MustParseAddrPort("192.0.2.1:51820"), dst: client, payload: transport(clientIndex)}.bytes()
	for _, c := range []struct {
		name string
		in   []byte
		mark uint32
		want uint32
	}{
		{"our reply", ours, replyMark, tcxPass},
		{"our mark, source not an entry", slow, replyMark, tcxNext},
		{"an entry source without our mark", ours, 0, tcxNext},
		{"an entry source with Cilium's host mark", ours, 0xc00, tcxNext},
	} {
		if r := runTC(t, objs.WgTcEgressGuard, c.in, skbCtx{Mark: c.mark, Ifindex: uint32(tp.ext)}); r.ret != c.want {
			t.Errorf("%s: verdict %d, want %d", c.name, int32(r.ret), int32(c.want))
		}
	}
}

// udpIn opens a socket in a named network namespace.
func udpIn(t *testing.T, ns string, at netip.AddrPort) *net.UDPConn {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = netns.Set(orig); _ = orig.Close() }()
	h, err := netns.GetFromName(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	if err := netns.Set(h); err != nil {
		t.Fatal(err)
	}
	c, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(at))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func receive(t *testing.T, c *net.UDPConn, from netip.AddrPort, want []byte) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, src, err := c.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatalf("nothing arrived at %v: %v", c.LocalAddr(), err)
	}
	if src != from || !bytes.Equal(buf[:n], want) {
		t.Fatalf("got %d bytes from %v, want %d bytes from %v", n, src, len(want), from)
	}
}

// TestForwardsLive sends real packets through the attached programs: client -> entry arrives at the server from the demux
// socket's source, and the server's reply to that source arrives at the client from the entry address. In xdp mode the
// request is rewritten by XDP (generic, as on kind's veth) and forwarded by tc through the metadata.
func TestForwardsLive(t *testing.T) {
	for _, mode := range []string{"tc", "xdp"} {
		t.Run(mode, func(t *testing.T) {
			tp := setup(t)
			objs := load(t, defaultCfg(tp))
			putSessions(t, objs, tp)
			attachAll(t, objs, tp, mode == "xdp")
			cl := udpIn(t, "bpfcl", client)
			sv := udpIn(t, "bpfsv", server)

			if _, err := cl.WriteToUDPAddrPort(transport(serverIndex), entry); err != nil {
				t.Fatal(err)
			}
			receive(t, sv, probe, transport(serverIndex))
			if _, err := sv.WriteToUDPAddrPort(transport(clientIndex), probe); err != nil {
				t.Fatal(err)
			}
			receive(t, cl, entry, transport(clientIndex))

			wantStats(t, objs, map[WgFastpathWgStat]uint64{
				WgFastpathWgStatWG_STAT_FORWARDED_TO_SERVER: 1,
				WgFastpathWgStatWG_STAT_FORWARDED_TO_CLIENT: 1,
			})
		})
	}
}

// TestSharesOneBucketPerSession: both directions draw from the session's one bucket, as in user space, and a reply keeps
// the session fresh just like a request.
func TestSharesOneBucketPerSession(t *testing.T) {
	tp := setup(t)
	cfg := defaultCfg(tp)
	cfg.RatePerS, cfg.Burst = 1, 3
	objs := load(t, cfg)
	putSessions(t, objs, tp)
	before := owner(t, objs)

	req, rep := toServerFrame().bytes(), toClientFrame().bytes()
	if r := runTC(t, objs.WgTcReply, rep, replyCtx(tp.srv)); r.ret != tcxRedirect {
		t.Fatalf("reply 1: verdict %d, want TCX_REDIRECT", int32(r.ret))
	}
	if got := owner(t, objs).LastSeenNs; got <= before.LastSeenNs {
		t.Errorf("a reply did not move the session's last_seen (%d, was %d)", got, before.LastSeenNs)
	}
	if r := runTC(t, objs.WgTcIngress, req, ingress(tp, 0)); r.ret != tcxRedirect {
		t.Fatalf("request 1: verdict %d, want TCX_REDIRECT", int32(r.ret))
	}
	if r := runTC(t, objs.WgTcReply, rep, replyCtx(tp.srv)); r.ret != tcxRedirect {
		t.Fatalf("reply 2: verdict %d, want TCX_REDIRECT", int32(r.ret))
	}
	// Three packets of the session so far: the bucket (burst 3) is empty for either direction.
	if r := runTC(t, objs.WgTcIngress, req, ingress(tp, 0)); r.ret != tcxDrop {
		t.Fatalf("request 2: verdict %d, want TCX_DROP", int32(r.ret))
	}
	if r := runTC(t, objs.WgTcReply, rep, replyCtx(tp.srv)); r.ret != tcxDrop {
		t.Fatalf("reply 3: verdict %d, want TCX_DROP", int32(r.ret))
	}
	wantStats(t, objs, map[WgFastpathWgStat]uint64{
		WgFastpathWgStatWG_STAT_FORWARDED_TO_SERVER: 1,
		WgFastpathWgStatWG_STAT_FORWARDED_TO_CLIENT: 2,
		WgFastpathWgStatWG_STAT_DROPPED_RATE:        2,
	})
	var other WgFastpathWgFwd
	if err := objs.WgSessions.Lookup(WgFastpathWgKey{Index: clientIndex}, &other); err != nil {
		t.Fatal(err)
	}
	if other.TokensX1000 != 0 || other.RefillNs != 0 || other.LastSeenNs != 0 {
		t.Errorf("the server -> client entry keeps a bucket of its own: %+v", other)
	}
}

// TestRepliesNeedTheirSession: a reply is fast only with a reply device, to the demux socket's address, and with the
// session's owner (the client -> server direction) present.
func TestRepliesNeedTheirSession(t *testing.T) {
	tp := setup(t)
	in := toClientFrame().bytes()

	t.Run("no reply device", func(t *testing.T) {
		objs := load(t, defaultCfg(tp))
		putSessions(t, objs, tp)
		if err := objs.WgSessions.Put(WgFastpathWgKey{Index: clientIndex}, fwd(t, server, entry, client, true, 0)); err != nil {
			t.Fatal(err)
		}
		checkUntouched(t, "reply_ifindex 0", runTC(t, objs.WgTcReply, in, replyCtx(tp.srv)), in)
		wantStats(t, objs, map[WgFastpathWgStat]uint64{WgFastpathWgStatWG_STAT_PASSED_MISS: 1})
	})
	t.Run("not to the demux socket", func(t *testing.T) {
		objs := load(t, defaultCfg(tp))
		putSessions(t, objs, tp)
		for _, dst := range []string{"10.0.0.1:51821", "10.0.0.9:51820"} {
			f := toClientFrame()
			f.dst = netip.MustParseAddrPort(dst)
			other := f.bytes()
			checkUntouched(t, "reply to "+dst, runTC(t, objs.WgTcReply, other, replyCtx(tp.srv)), other)
		}
		wantStats(t, objs, map[WgFastpathWgStat]uint64{WgFastpathWgStatWG_STAT_PASSED_MISS: 2})
	})
	t.Run("owner of another session", func(t *testing.T) {
		// The reply entry names an index that now owns another, self-consistent session (a stale entry after index reuse):
		// the reply must not draw from that session's bucket or keep it fresh.
		cfg := defaultCfg(tp)
		cfg.RatePerS, cfg.Burst = 1, 3
		objs := load(t, cfg)
		otherClient := netip.MustParseAddrPort("192.0.2.7:47000")
		otherServer := netip.MustParseAddrPort("10.0.0.7:51820")
		if err := objs.WgSessions.Put(WgFastpathWgKey{Index: serverIndex}, fwd(t, otherClient, probe, otherServer, false, 0)); err != nil {
			t.Fatal(err)
		}
		if err := objs.WgSessions.Put(WgFastpathWgKey{Index: clientIndex}, fwd(t, server, entry, client, true, tp.srv)); err != nil {
			t.Fatal(err)
		}
		before := owner(t, objs)
		checkUntouched(t, "reply charged to another session", runTC(t, objs.WgTcReply, in, replyCtx(tp.srv)), in)
		if after := owner(t, objs); after != before {
			t.Errorf("the other session's owner changed: %+v, was %+v", after, before)
		}
		wantStats(t, objs, map[WgFastpathWgStat]uint64{WgFastpathWgStatWG_STAT_PASSED_MISS: 1})
	})
	t.Run("no owner", func(t *testing.T) {
		objs := load(t, defaultCfg(tp))
		if err := objs.WgSessions.Put(WgFastpathWgKey{Index: clientIndex}, fwd(t, server, entry, client, true, tp.srv)); err != nil {
			t.Fatal(err)
		}
		checkUntouched(t, "reply without its session's owner", runTC(t, objs.WgTcReply, in, replyCtx(tp.srv)), in)
		wantStats(t, objs, map[WgFastpathWgStat]uint64{WgFastpathWgStatWG_STAT_PASSED_MISS: 1})
	})
}

// TestXDPWithoutMetadataRoomChargesNothing: when the driver gives no room for the metadata (here: the room is already used
// up), XDP passes the frame untouched before judging it, so tc's judgement is the only one that costs a token.
func TestXDPWithoutMetadataRoomChargesNothing(t *testing.T) {
	tp := setup(t)
	cfg := defaultCfg(tp)
	cfg.RatePerS, cfg.Burst = 1, 3
	objs := load(t, cfg)
	putSessions(t, objs, tp)
	before := owner(t, objs)

	// The input already carries as much metadata as the kernel takes (the limit depends on the kernel: the largest input it
	// accepts), so there is no room for ours.
	frameIn := toServerFrame().bytes()
	var (
		ret   uint32
		room  uint32
		opts  *ebpf.RunOptions
		ctxIn xdpCtx
		err   error
	)
	for room = 252; room >= 32; room -= 4 {
		in := append(make([]byte, room), frameIn...)
		ctxIn = xdpCtx{Data: room, DataEnd: uint32(len(in))}
		var ctxOut xdpCtx
		opts = &ebpf.RunOptions{Data: in, DataOut: make([]byte, len(in)+256), Context: ctxIn, ContextOut: &ctxOut}
		if ret, err = objs.WgXdp.Run(opts); err == nil {
			ctxIn = ctxOut
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if ret != xdpPass || ctxIn.Data != room || !bytes.Equal(opts.DataOut[room:], frameIn) {
		t.Fatalf("verdict %d, meta %d bytes (input %d); want XDP_PASS with the frame untouched", ret, ctxIn.Data, room)
	}
	after := owner(t, objs)
	if after.TokensX1000 != before.TokensX1000 || after.RefillNs != before.RefillNs || after.LastSeenNs != before.LastSeenNs {
		t.Errorf("the session was charged: bucket %d/%d seen %d, was %d/%d seen %d",
			after.TokensX1000, after.RefillNs, after.LastSeenNs, before.TokensX1000, before.RefillNs, before.LastSeenNs)
	}
	wantStats(t, objs, map[WgFastpathWgStat]uint64{WgFastpathWgStatWG_STAT_PASSED_META: 1})
}

func TestPassesGROAggregates(t *testing.T) {
	tp := setup(t)
	objs := load(t, defaultCfg(tp))
	putSessions(t, objs, tp)
	in := toServerFrame().bytes()
	ctx := ingress(tp, 0)
	ctx.GsoSize, ctx.GsoSegs = 100, 1
	checkUntouched(t, "gso to server", runTC(t, objs.WgTcIngress, in, ctx), in)
	rep := toClientFrame().bytes()
	ctx = replyCtx(tp.srv)
	ctx.GsoSize, ctx.GsoSegs = 100, 1
	checkUntouched(t, "gso to client", runTC(t, objs.WgTcReply, rep, ctx), rep)
	wantStats(t, objs, nil)
}

func attachAll(t *testing.T, objs *WgFastpathObjects, tp topo, xdp bool) {
	t.Helper()
	for _, a := range []struct {
		prog *ebpf.Program
		dev  int
		at   ebpf.AttachType
	}{
		{objs.WgTcIngress, tp.ext, ebpf.AttachTCXIngress},
		{objs.WgTcEgressGuard, tp.ext, ebpf.AttachTCXEgress},
		{objs.WgTcReply, tp.srv, ebpf.AttachTCXIngress},
	} {
		l, err := link.AttachTCX(link.TCXOptions{Program: a.prog, Interface: a.dev, Attach: a.at, Anchor: link.Head()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
	}
	if xdp {
		l, err := link.AttachXDP(link.XDPOptions{Program: objs.WgXdp, Interface: tp.ext, Flags: link.XDPGenericMode})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
	}
}

// TestXDPPutBackWithoutRoute: XDP rewrote a frame towards a server tc then finds no route to; tc puts it back exactly as it
// arrived, and the demux socket on the entry address (the slow path) receives it from the client.
func TestXDPPutBackWithoutRoute(t *testing.T) {
	tp := setup(t)
	objs := load(t, defaultCfg(tp))
	nowhere := netip.MustParseAddrPort("203.0.113.9:51820")
	if err := objs.WgSessions.Put(WgFastpathWgKey{Index: serverIndex}, fwd(t, client, probe, nowhere, false, 0)); err != nil {
		t.Fatal(err)
	}
	attachAll(t, objs, tp, true)
	demux, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(entry))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = demux.Close() })
	cl := udpIn(t, "bpfcl", client)

	if _, err := cl.WriteToUDPAddrPort(transport(serverIndex), entry); err != nil {
		t.Fatal(err)
	}
	receive(t, demux, client, transport(serverIndex))
	wantStats(t, objs, map[WgFastpathWgStat]uint64{WgFastpathWgStatWG_STAT_PASSED_ROUTE: 1})
}
