//go:build linux

package fastpath

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"

	"github.com/cybericebox/laboratory/internal/proxy/demux"
)

// The loader tests use the topology of prog_test.go (ext0 holds the node address 192.0.2.1, srv0 leads to the server) and a
// bpffs of their own, mounted on a temporary directory, as the pin path.

func pinDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := unix.Mount("bpf", dir, "bpf", 0, ""); err != nil {
		t.Fatalf("mount bpffs: %v", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(dir, unix.MNT_DETACH) })
	return filepath.Join(dir, "cybericebox", "wg-demux")
}

func options(pin string, mode demux.Mode) Options {
	return Options{Mode: mode, PinPath: pin, HostIP: "192.0.2.1", TTL: 3 * time.Minute, SourcePort: probe.Port()}
}

func open(t *testing.T, o Options) *FastPath {
	t.Helper()
	f, err := Open(o)
	if err != nil {
		var ve *ebpf.VerifierError
		if errors.As(err, &ve) {
			t.Fatalf("%+v", ve)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func sockOf(a netip.AddrPort) demux.Socket {
	return demux.Socket{IP: net.IP(a.Addr().AsSlice()), Port: a.Port()}
}

// sessionForwards is the session client <-> server as ConnTrack writes it: the reply direction from srv0.
func sessionForwards(tp topo) (toServer, toClient demux.Forward) {
	toServer = demux.Forward{Index: serverIndex, Session: serverIndex, Expected: sockOf(client), NewSource: sockOf(probe), Dest: sockOf(server)}
	toClient = demux.Forward{Index: clientIndex, Session: serverIndex, Expected: sockOf(server), NewSource: sockOf(entry), Dest: sockOf(client),
		ToClient: true, Via: sockOf(probe), ReplyIfindex: tp.srv}
	return toServer, toClient
}

// programs lists the program IDs attached to a device in tcx order.
func programs(t *testing.T, ifindex int, at ebpf.AttachType) []ebpf.ProgramID {
	t.Helper()
	r, err := link.QueryPrograms(link.QueryOptions{Target: ifindex, Attach: at})
	if err != nil {
		t.Fatal(err)
	}
	var ids []ebpf.ProgramID
	for _, p := range r.Programs {
		ids = append(ids, p.ID)
	}
	return ids
}

func progID(t *testing.T, p *ebpf.Program) ebpf.ProgramID {
	t.Helper()
	info, err := p.Info()
	if err != nil {
		t.Fatal(err)
	}
	id, _ := info.ID()
	return id
}

// dummy is a program that passes everything on: a stand-in for another tool's program (Cilium's, say).
func dummy(t *testing.T, typ ebpf.ProgramType, at ebpf.AttachType, ret int32) *ebpf.Program {
	t.Helper()
	p, err := ebpf.NewProgram(&ebpf.ProgramSpec{Type: typ, AttachType: at, License: "GPL",
		Instructions: asm.Instructions{asm.Mov.Imm(asm.R0, ret), asm.Return()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func entrySet(t *testing.T, f *FastPath) []netip.AddrPort {
	t.Helper()
	got, err := f.Entries()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestOpenTwiceReusesPins: a restarted demux finds the maps of the one before (entries and sessions) and updates the links in
// place instead of attaching a second copy.
func TestOpenTwiceReusesPins(t *testing.T) {
	tp := setup(t)
	pin := pinDir(t)
	f := open(t, options(pin, demux.ModeTC))
	if err := f.SetEntries([]netip.AddrPort{entry}); err != nil {
		t.Fatal(err)
	}
	toServer, toClient := sessionForwards(tp)
	for _, fw := range []demux.Forward{toServer, toClient} {
		if err := f.Upsert(fw); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	g := open(t, options(pin, demux.ModeTC))
	if got := entrySet(t, g); !slices.Equal(got, []netip.AddrPort{entry}) {
		t.Fatalf("entries after a restart: %v", got)
	}
	pinned, err := g.Pinned()
	if err != nil {
		t.Fatal(err)
	}
	if len(pinned) != 2 {
		t.Fatalf("pinned sessions: %+v", pinned)
	}
	for _, p := range pinned {
		want := toServer
		if p.ToClient {
			want = toClient
		}
		if p.Index != want.Index || p.Session != want.Session || p.ReplyIfindex != want.ReplyIfindex || !same(p.Expected, want.Expected) ||
			!same(p.NewSource, want.NewSource) || !same(p.Dest, want.Dest) || p.ToClient && !same(p.Via, want.Via) {
			t.Errorf("pinned %+v, want %+v", p, want)
		}
	}
	for _, c := range []struct {
		dev  int
		at   ebpf.AttachType
		prog *ebpf.Program
	}{
		{tp.ext, ebpf.AttachTCXIngress, g.objs.WgTcIngress},
		{tp.ext, ebpf.AttachTCXEgress, g.objs.WgTcEgressGuard},
		{tp.srv, ebpf.AttachTCXIngress, g.objs.WgTcReply},
	} {
		if got := programs(t, c.dev, c.at); !slices.Equal(got, []ebpf.ProgramID{progID(t, c.prog)}) {
			t.Errorf("device %d %v: programs %v, want only the new one %d", c.dev, c.at, got, progID(t, c.prog))
		}
	}
}

func same(a, b demux.Socket) bool { return a.Port == b.Port && a.IP.Equal(b.IP) }

// TestEnsureOrderAfterForeignHeadAttach: another program put itself first; EnsureOrder puts ours back in front of it.
func TestEnsureOrderAfterForeignHeadAttach(t *testing.T) {
	tp := setup(t)
	f := open(t, options(pinDir(t), demux.ModeTC))
	upsertSession(t, f, tp)
	for _, c := range []struct {
		dev  int
		at   ebpf.AttachType
		prog *ebpf.Program
	}{
		{tp.ext, ebpf.AttachTCXIngress, f.objs.WgTcIngress},
		{tp.ext, ebpf.AttachTCXEgress, f.objs.WgTcEgressGuard},
		{tp.srv, ebpf.AttachTCXIngress, f.objs.WgTcReply},
	} {
		foreign := dummy(t, ebpf.SchedCLS, c.at, -1)
		l, err := link.AttachTCX(link.TCXOptions{Program: foreign, Interface: c.dev, Attach: c.at, Anchor: link.Head()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		if got := programs(t, c.dev, c.at); got[0] != progID(t, foreign) {
			t.Fatalf("the foreign program is not first: %v", got)
		}
	}
	if err := f.EnsureOrder(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dev  int
		at   ebpf.AttachType
		kind string
		old  *ebpf.Program
	}{
		{tp.ext, ebpf.AttachTCXIngress, kindIngress, f.objs.WgTcIngress},
		{tp.ext, ebpf.AttachTCXEgress, kindEgress, f.objs.WgTcEgressGuard},
		{tp.srv, ebpf.AttachTCXIngress, kindReply, f.objs.WgTcReply},
	} {
		// Ours is first again, a second instance of the program (attached before the old link went), and it is the pinned link.
		r, err := link.QueryPrograms(link.QueryOptions{Target: c.dev, Attach: c.at})
		if err != nil {
			t.Fatal(err)
		}
		pinned, err := link.LoadPinnedLink(f.linkPath(pinName(c.kind, c.dev)), nil)
		if err != nil {
			t.Fatal(err)
		}
		info, err := pinned.Info()
		_ = pinned.Close()
		if err != nil {
			t.Fatal(err)
		}
		first, _ := r.Programs[0].LinkID()
		if len(r.Programs) != 2 || first != info.ID || r.Programs[0].ID != info.Program || info.Program == progID(t, c.old) {
			t.Errorf("device %d %v: programs %+v, pinned link %d (program %d), old program %d", c.dev, c.at, r.Programs, info.ID, info.Program, progID(t, c.old))
		}
	}
	// a second run finds everything in order and changes nothing
	before := programs(t, tp.ext, ebpf.AttachTCXIngress)
	if err := f.EnsureOrder(); err != nil {
		t.Fatal(err)
	}
	if got := programs(t, tp.ext, ebpf.AttachTCXIngress); !slices.Equal(got, before) {
		t.Errorf("a second EnsureOrder changed %v to %v", before, got)
	}
}

// TestUpsertDoesNotWaitForEnsureOrder: the sink runs under the ConnTrack lock, so a resync that is busy with the devices must
// never hold it up, not even when a session names a reply device that is not attached yet.
func TestUpsertDoesNotWaitForEnsureOrder(t *testing.T) {
	tp := setup(t)
	f := open(t, options(pinDir(t), demux.ModeTC))
	toServer, toClient := sessionForwards(tp)
	if err := f.Upsert(toServer); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.query = func(o link.QueryOptions) (*link.QueryResult, error) {
		once.Do(func() { close(entered) })
		<-release
		return link.QueryPrograms(o)
	}
	done := make(chan error, 1)
	go func() { done <- f.EnsureOrder() }()
	<-entered
	upserted := make(chan error, 1)
	go func() {
		_, _ = f.TowardsServer(sockOf(server))
		upserted <- f.Upsert(toClient)
	}()
	select {
	case err := <-upserted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("Upsert waited for a running EnsureOrder")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := programs(t, tp.srv, ebpf.AttachTCXIngress); len(got) != 1 {
		t.Fatalf("reply device programs %v, want ours once", got)
	}
}

func upsertSession(t *testing.T, f *FastPath, tp topo) {
	t.Helper()
	toServer, toClient := sessionForwards(tp)
	for _, fw := range []demux.Forward{toServer, toClient} {
		if err := f.Upsert(fw); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSetEntriesReplacesSet(t *testing.T) {
	setup(t)
	f := open(t, options(pinDir(t), demux.ModeTC))
	a, b, c := netip.MustParseAddrPort("192.0.2.1:31820"), netip.MustParseAddrPort("198.51.100.1:51820"), netip.MustParseAddrPort("203.0.113.1:51820")
	if f.Ready() == nil {
		t.Fatal("ready without entries")
	}
	if err := f.SetEntries([]netip.AddrPort{a, b, netip.MustParseAddrPort("[2001:db8::1]:51820")}); err != nil {
		t.Fatal(err)
	}
	if err := f.SetEntries([]netip.AddrPort{b, c}); err != nil {
		t.Fatal(err)
	}
	if got := entrySet(t, f); !slices.Equal(got, []netip.AddrPort{b, c}) {
		t.Fatalf("entries %v, want %v", got, []netip.AddrPort{b, c})
	}
	if err := f.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}
}

// TestLastSeenConvertsClock: the program's CLOCK_MONOTONIC stamps come back as wall time, from the owner of each session only.
func TestLastSeenConvertsClock(t *testing.T) {
	tp := setup(t)
	f := open(t, options(pinDir(t), demux.ModeTC))
	upsertSession(t, f, tp)
	var o WgFastpathWgFwd
	if err := f.objs.WgSessions.Lookup(WgFastpathWgKey{Index: serverIndex}, &o); err != nil {
		t.Fatal(err)
	}
	o.LastSeenNs = monoNow(t) - uint64(70*time.Second)
	if err := f.objs.WgSessions.Put(WgFastpathWgKey{Index: serverIndex}, o); err != nil {
		t.Fatal(err)
	}
	seen, err := f.LastSeen()
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("last seen of the owners only: %v", seen)
	}
	if d := time.Since(seen[serverIndex]); d < 69*time.Second || d > 72*time.Second {
		t.Fatalf("last seen %v ago, want 70s", d)
	}
}

// TestDetachRemovesEverything: proxy-wg detach leaves no program on any device and no pin.
func TestDetachRemovesEverything(t *testing.T) {
	tp := setup(t)
	pin := pinDir(t)
	f := open(t, options(pin, demux.ModeXDP))
	upsertSession(t, f, tp)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Detach(pin); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dev int
		at  ebpf.AttachType
	}{{tp.ext, ebpf.AttachTCXIngress}, {tp.ext, ebpf.AttachTCXEgress}, {tp.srv, ebpf.AttachTCXIngress}} {
		if got := programs(t, c.dev, c.at); len(got) != 0 {
			t.Errorf("device %d %v: programs %v left", c.dev, c.at, got)
		}
	}
	if id := xdpProgram(t, tp.ext); id != 0 {
		t.Errorf("XDP program %d left on ext0", id)
	}
	if _, err := os.Stat(pin); !os.IsNotExist(err) {
		t.Errorf("pin path left: %v", err)
	}
	if err := Detach(pin); err != nil {
		t.Errorf("a second detach: %v", err)
	}
}

// TestXDPRefusesForeignProgram: xdp mode never replaces another tool's XDP program.
func TestXDPRefusesForeignProgram(t *testing.T) {
	tp := setup(t)
	foreign := dummy(t, ebpf.XDP, ebpf.AttachXDP, 2)
	l, err := link.AttachXDP(link.XDPOptions{Program: foreign, Interface: tp.ext, Flags: link.XDPGenericMode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	pin := pinDir(t)
	if f, err := Open(options(pin, demux.ModeXDP)); err == nil {
		_ = f.Close()
		t.Fatal("xdp mode attached over a foreign XDP program")
	}
	if id := xdpProgram(t, tp.ext); id != progID(t, foreign) {
		t.Fatalf("XDP program on ext0 is %d, want the foreign %d", id, progID(t, foreign))
	}
	// the refused Open leaves nothing of its own behind
	for _, at := range []ebpf.AttachType{ebpf.AttachTCXIngress, ebpf.AttachTCXEgress} {
		if got := programs(t, tp.ext, at); len(got) != 0 {
			t.Errorf("a refused Open left programs %v on ext0 %v", got, at)
		}
	}
	if pins, _ := os.ReadDir(filepath.Join(pin, linksDir)); len(pins) != 0 {
		t.Errorf("a refused Open left pins %v", pins)
	}
	// tc mode does not touch XDP; a refused xdp Open keeps the links that were pinned before it
	f := open(t, options(pin, demux.ModeTC))
	_ = f.Close()
	if _, err := Open(options(pin, demux.ModeXDP)); err == nil {
		t.Fatal("xdp mode attached over a foreign XDP program")
	}
	for _, at := range []ebpf.AttachType{ebpf.AttachTCXIngress, ebpf.AttachTCXEgress} {
		if got := programs(t, tp.ext, at); len(got) != 1 {
			t.Errorf("the earlier demux's link on ext0 %v is gone: %v", at, got)
		}
	}
}

// TestOpenForwardsLive: the sessions written through the Sink (byte order, session, via and reply device) forward real
// packets both ways, and the source lookups give what the slow path would use.
func TestOpenForwardsLive(t *testing.T) {
	for _, mode := range []demux.Mode{demux.ModeTC, demux.ModeXDP} {
		t.Run(string(mode), func(t *testing.T) {
			tp := setup(t)
			f := open(t, options(pinDir(t), mode))
			if err := f.SetEntries([]netip.AddrPort{entry}); err != nil {
				t.Fatal(err)
			}
			cl := udpIn(t, "bpfcl", client)
			sv := udpIn(t, "bpfsv", server)

			if src, ok := f.TowardsServer(sockOf(server)); !ok || !same(src, sockOf(probe)) {
				t.Fatalf("towards the server: %v %v, want %v", src, ok, probe)
			}
			if _, ok := f.TowardsClient(sockOf(client)); ok {
				t.Fatal("an entry before any handshake")
			}
			// The client's handshake initiation to the entry address records it (the demux socket is not listening here).
			if _, err := cl.WriteToUDPAddrPort(initiation(clientIndex), entry); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				src, ok := f.TowardsClient(sockOf(client))
				if ok {
					if !same(src, sockOf(entry)) {
						t.Fatalf("towards the client: %v, want %v", src, entry)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the entry of the handshake was not recorded")
				}
				time.Sleep(20 * time.Millisecond)
			}

			toServer, toClient := sessionForwards(tp)
			for _, fw := range []demux.Forward{toServer, toClient} {
				if err := f.Upsert(fw); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := cl.WriteToUDPAddrPort(transport(serverIndex), entry); err != nil {
				t.Fatal(err)
			}
			receive(t, sv, probe, transport(serverIndex))
			if _, err := sv.WriteToUDPAddrPort(transport(clientIndex), probe); err != nil {
				t.Fatal(err)
			}
			receive(t, cl, entry, transport(clientIndex))
			st := f.Stats()
			if st["forwarded_to_server"] != 1 || st["forwarded_to_client"] != 1 {
				t.Fatalf("stats %v", st)
			}

			if err := f.Delete(serverIndex); err != nil {
				t.Fatal(err)
			}
			if err := f.Delete(serverIndex); err != nil {
				t.Fatalf("deleting a missing index: %v", err)
			}
			if pinned, _ := f.Pinned(); len(pinned) != 1 {
				t.Fatalf("after a delete: %+v", pinned)
			}
		})
	}
}

func xdpProgram(t *testing.T, ifindex int) ebpf.ProgramID {
	t.Helper()
	id, err := attachedXDP(ifindex)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
