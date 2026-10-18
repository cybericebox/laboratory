//go:build linux

package fastpath

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"

	"github.com/cybericebox/laboratory/internal/proxy/demux"
)

// FastPath is the demux's Sink (sessions written into wg_sessions) and SourceFor (the addresses the slow path would send from).
var (
	_ demux.Sink      = (*FastPath)(nil)
	_ demux.SourceFor = (*FastPath)(nil)
)

const toClientFlag = 1 // wg_fwd.flags: the server -> client direction

// The maps hold addresses and ports in network byte order: the bytes of the packet, read as a native integer.
func nbo32(a netip.Addr) uint32 {
	b := a.As4()
	return binary.NativeEndian.Uint32(b[:])
}

func nbo16(p uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], p)
	return binary.NativeEndian.Uint16(b[:])
}

func addrOf(ip uint32) netip.Addr {
	var b [4]byte
	binary.NativeEndian.PutUint32(b[:], ip)
	return netip.AddrFrom4(b)
}

func portOf(p uint16) uint16 {
	var b [2]byte
	binary.NativeEndian.PutUint16(b[:], p)
	return binary.BigEndian.Uint16(b[:])
}

func ipv4(s demux.Socket) (netip.Addr, error) {
	a, ok := netip.AddrFromSlice(s.IP)
	if !ok || !a.Unmap().Is4() {
		return netip.Addr{}, fmt.Errorf("%v is not an IPv4 address (the fast path is IPv4 only)", s.IP)
	}
	return a.Unmap(), nil
}

func wgAddr(s demux.Socket) (WgFastpathWgAddr, error) {
	a, err := ipv4(s)
	if err != nil {
		return WgFastpathWgAddr{}, err
	}
	return WgFastpathWgAddr{Ip: nbo32(a), Port: nbo16(s.Port)}, nil
}

func socket(ip uint32, port uint16) demux.Socket {
	return demux.Socket{IP: net.IP(addrOf(ip).AsSlice()), Port: portOf(port)}
}

func monotonic() uint64 {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return uint64(ts.Nano())
}

// record is the map value of one direction (see struct wg_fwd): the owner starts its last-seen now and a fresh bucket.
func record(fw demux.Forward) (WgFastpathWgFwd, error) {
	var v WgFastpathWgFwd
	socks := []demux.Socket{fw.Expected, fw.NewSource, fw.Dest}
	if fw.ToClient {
		socks = append(socks, fw.Via)
	}
	var a [4]WgFastpathWgAddr
	for i, s := range socks {
		var err error
		if a[i], err = wgAddr(s); err != nil {
			return v, err
		}
	}
	v.ExpIp, v.ExpPort = a[0].Ip, a[0].Port
	v.NewSrcIp, v.NewSrcPort = a[1].Ip, a[1].Port
	v.DstIp, v.DstPort = a[2].Ip, a[2].Port
	v.Session = fw.Session
	if fw.ToClient {
		v.Flags = toClientFlag
		v.ViaIp, v.ViaPort = a[3].Ip, a[3].Port
		v.ReplyIfindex = uint32(max(fw.ReplyIfindex, 0))
	} else {
		v.LastSeenNs = monotonic()
	}
	return v, nil
}

// Upsert writes one direction of a session. The device a reply direction names gets the reply program first (a same-node
// server's veth, the first time a session names it); if that fails the replies stay on the slow path and the error is returned.
func (f *FastPath) Upsert(fw demux.Forward) error {
	v, err := record(fw)
	if err != nil {
		return err
	}
	var attachErr error
	if fw.ToClient && v.ReplyIfindex != 0 {
		if attachErr = f.replyDevice(int(v.ReplyIfindex)); attachErr != nil {
			v.ReplyIfindex = 0
		}
	}
	if err := f.objs.WgSessions.Put(WgFastpathWgKey{Index: fw.Index}, v); err != nil {
		return fmt.Errorf("write session %d: %w", fw.Index, err)
	}
	return attachErr
}

func (f *FastPath) replyDevice(ifindex int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.isExternal(ifindex) {
		return fmt.Errorf("device %d is external: its replies stay on the slow path", ifindex)
	}
	if _, ok := f.links[pinName(kindReply, ifindex)]; ok {
		return nil
	}
	return f.tcxLocked(kindReply, ifindex)
}

// Delete removes one direction; a missing index is not an error.
func (f *FastPath) Delete(index uint32) error {
	if err := f.objs.WgSessions.Delete(WgFastpathWgKey{Index: index}); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return fmt.Errorf("delete session %d: %w", index, err)
	}
	return nil
}

// TowardsServer is the demux socket's source towards a server: the kernel's preferred source for its address (a connected UDP
// socket sends nothing) and the demux's port.
func (f *FastPath) TowardsServer(server demux.Socket) (demux.Socket, bool) {
	ip, err := ipv4(server)
	if err != nil {
		return demux.Socket{}, false
	}
	f.mu.Lock()
	src, ok := f.sources[ip]
	f.mu.Unlock()
	if !ok {
		c, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, server.Port)))
		if err != nil {
			return demux.Socket{}, false
		}
		local := c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
		_ = c.Close()
		src = local
		f.mu.Lock()
		f.sources[ip] = src
		f.mu.Unlock()
	}
	return demux.Socket{IP: net.IP(src.AsSlice()), Port: f.o.SourcePort}, true
}

// TowardsClient is the entry address the client's handshake initiation arrived at, as the program recorded it.
func (f *FastPath) TowardsClient(client demux.Socket) (demux.Socket, bool) {
	k, err := wgAddr(client)
	if err != nil {
		return demux.Socket{}, false
	}
	var e WgFastpathWgAddr
	if err := f.objs.WgEntrySeen.Lookup(k, &e); err != nil {
		return demux.Socket{}, false
	}
	return socket(e.Ip, e.Port), true
}

// SetEntries replaces the set of entry addresses (IPv4 only; the others stay on the slow path).
func (f *FastPath) SetEntries(addrs []netip.AddrPort) error {
	want := map[WgFastpathWgAddr]bool{}
	for _, a := range addrs {
		if a.Addr().Unmap().Is4() {
			want[WgFastpathWgAddr{Ip: nbo32(a.Addr().Unmap()), Port: nbo16(a.Port())}] = true
		}
	}
	for k := range want {
		if err := f.objs.WgEntries.Put(k, uint8(1)); err != nil {
			return fmt.Errorf("write entry: %w", err)
		}
	}
	var stale []WgFastpathWgAddr
	var k WgFastpathWgAddr
	var v uint8
	it := f.objs.WgEntries.Iterate()
	for it.Next(&k, &v) {
		if !want[k] {
			stale = append(stale, k)
		}
	}
	if err := it.Err(); err != nil {
		return err
	}
	for _, k := range stale {
		if err := f.objs.WgEntries.Delete(k); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return err
		}
	}
	f.mu.Lock()
	f.entries = len(want) > 0
	f.mu.Unlock()
	return nil
}

// Entries lists the entry addresses in the map, sorted.
func (f *FastPath) Entries() ([]netip.AddrPort, error) {
	var out []netip.AddrPort
	var k WgFastpathWgAddr
	var v uint8
	it := f.objs.WgEntries.Iterate()
	for it.Next(&k, &v) {
		out = append(out, netip.AddrPortFrom(addrOf(k.Ip), portOf(k.Port)))
	}
	slices.SortFunc(out, netip.AddrPort.Compare)
	return out, it.Err()
}

// sessions reads the whole session map in batches.
func (f *FastPath) sessions(each func(index uint32, v *WgFastpathWgFwd)) error {
	const chunk = 4096
	keys := make([]WgFastpathWgKey, chunk)
	vals := make([]WgFastpathWgFwd, chunk)
	var cursor ebpf.MapBatchCursor
	for {
		n, err := f.objs.WgSessions.BatchLookup(&cursor, keys, vals, nil)
		for i := 0; i < n; i++ {
			each(keys[i].Index, &vals[i])
		}
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil
		}
		if errors.Is(err, ebpf.ErrNotSupported) {
			return f.iterate(each)
		}
		if err != nil {
			return err
		}
	}
}

func (f *FastPath) iterate(each func(index uint32, v *WgFastpathWgFwd)) error {
	var k WgFastpathWgKey
	var v WgFastpathWgFwd
	it := f.objs.WgSessions.Iterate()
	for it.Next(&k, &v) {
		each(k.Index, &v)
	}
	return it.Err()
}

// LastSeen is the last forwarded packet of every session, by its owner's index (the client -> server direction), in wall time.
func (f *FastPath) LastSeen() (map[uint32]time.Time, error) {
	wall, mono := time.Now(), monotonic()
	out := map[uint32]time.Time{}
	err := f.sessions(func(index uint32, v *WgFastpathWgFwd) {
		if v.Flags&toClientFlag != 0 || v.Session != index {
			return
		}
		at := wall
		if v.LastSeenNs < mono {
			at = wall.Add(-time.Duration(mono - v.LastSeenNs))
		}
		out[index] = at
	})
	return out, err
}

// Pinned lists every direction in the session map, for the restore after a restart.
func (f *FastPath) Pinned() ([]demux.Forward, error) {
	var out []demux.Forward
	err := f.sessions(func(index uint32, v *WgFastpathWgFwd) {
		fw := demux.Forward{
			Index: index, Session: v.Session,
			Expected:  socket(v.ExpIp, v.ExpPort),
			NewSource: socket(v.NewSrcIp, v.NewSrcPort),
			Dest:      socket(v.DstIp, v.DstPort),
			ToClient:  v.Flags&toClientFlag != 0,
		}
		if fw.ToClient {
			fw.Via, fw.ReplyIfindex = socket(v.ViaIp, v.ViaPort), int(v.ReplyIfindex)
		}
		out = append(out, fw)
	})
	slices.SortFunc(out, func(a, b demux.Forward) int {
		switch {
		case a.Index < b.Index:
			return -1
		case a.Index > b.Index:
			return 1
		}
		return 0
	})
	return out, err
}

// Stats sums the program's counters over the CPUs.
func (f *FastPath) Stats() Stats {
	out := Stats{}
	for i, name := range demux.FastPathResults {
		var per []uint64
		if err := f.objs.WgStats.Lookup(uint32(i), &per); err != nil {
			continue
		}
		var n uint64
		for _, v := range per {
			n += v
		}
		out[name] = n
	}
	return out
}
