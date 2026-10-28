//go:build linux

package fastpath

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"

	"github.com/cybericebox/laboratory/internal/proxy/demux"
)

// The links are pinned under <PinPath>/links as <kind>-<ifindex>: ingress and egress on the external devices, xdp on them in
// xdp mode, reply on cilium_host and on the host-side veth of every same-node server a session named. Interface indices are
// not reused while the device lives, and a link whose device went away is defunct (the kernel detached it).
const linksDir = "links"

const (
	kindIngress = "ingress"
	kindEgress  = "egress"
	kindReply   = "reply"
	kindXDP     = "xdp"
)

// FastPath is the loaded fast path of one demux: the programs, the pinned maps, and the links that attach the programs.
//
// Upsert runs under the ConnTrack lock, so mu guards only the in-memory state and is never held across a syscall; the work on
// the devices runs outside it and publishes its result under it. work serialises that work (Open and EnsureOrder); the packet
// path never takes it.
type FastPath struct {
	o    Options
	spec *ebpf.CollectionSpec
	objs WgFastpathObjects
	ext  []int

	work sync.Mutex

	mu      sync.Mutex
	links   map[string]link.Link      // by pin name
	busy    map[string]bool           // pin names being attached outside mu
	sources map[netip.Addr]netip.Addr // the kernel's preferred source towards a server, until the next EnsureOrder
	entries bool                      // SetEntries has given at least one address
	order   error                     // the last EnsureOrder's result

	query func(link.QueryOptions) (*link.QueryResult, error) // link.QueryPrograms; a seam for the tests
}

// Open loads the programs, re-opens the maps a previous demux pinned (or creates and pins them), and attaches the programs at
// the head of the devices: a pinned link of a previous demux gets the new program in place, so forwarding never stops. It
// refuses xdp mode when another XDP program is attached to an external device. A failed Open removes the links it attached
// itself and keeps those of the demux before it.
func Open(o Options) (*FastPath, error) {
	if o.Mode != demux.ModeTC && o.Mode != demux.ModeXDP {
		return nil, fmt.Errorf("the fast path runs in tc or xdp mode, not %q", o.Mode)
	}
	if o.PinPath == "" {
		return nil, errors.New("no pin path")
	}
	ext, err := externalDevices(o)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(o.PinPath, linksDir), 0o700); err != nil {
		return nil, fmt.Errorf("pin path: %w", err)
	}
	f := &FastPath{o: o, ext: ext, links: map[string]link.Link{}, busy: map[string]bool{}, sources: map[netip.Addr]netip.Addr{},
		query: link.QueryPrograms}
	if err := f.load(); err != nil {
		return nil, err
	}
	cfg := WgFastpathWgCfg{TtlNs: uint64(o.TTL), ExtIfindex: uint32(ext[0])}
	if o.Rate > 0 {
		cfg.RatePerS, cfg.Burst = uint64(math.Ceil(o.Rate)), uint64(max(o.Burst, 1))
	}
	if err := f.objs.WgCfg.Put(uint32(0), cfg); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write the config: %w", err)
	}
	created, err := f.attach()
	if err == nil {
		err = f.EnsureOrder()
	}
	if err != nil {
		f.mu.Lock()
		for _, name := range created {
			if l, ok := f.links[name]; ok {
				_ = l.Close()
				delete(f.links, name)
			}
			removePinnedLink(f.linkPath(name))
		}
		f.mu.Unlock()
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// SetRate changes the packet rate of one session (both directions together) in the running fast path; rate 0 = unlimited. Sessions
// already forwarded keep their tokens and meet the new rate at their next refill.
func (f *FastPath) SetRate(rate float64, burst int) error {
	var cfg WgFastpathWgCfg
	if err := f.objs.WgCfg.Lookup(uint32(0), &cfg); err != nil {
		return fmt.Errorf("read the config: %w", err)
	}
	cfg.RatePerS, cfg.Burst = 0, 0
	if rate > 0 {
		cfg.RatePerS, cfg.Burst = uint64(math.Ceil(rate)), uint64(max(burst, 1))
	}
	if err := f.objs.WgCfg.Put(uint32(0), cfg); err != nil {
		return fmt.Errorf("write the config: %w", err)
	}
	return nil
}

func (f *FastPath) load() error {
	spec, err := LoadWgFastpath()
	if err != nil {
		return err
	}
	if f.o.MaxEntries > 0 {
		spec.Maps[WgFastpathMapWgSessions].MaxEntries = uint32(f.o.MaxEntries)
	}
	opts := &ebpf.CollectionOptions{Maps: ebpf.MapOptions{PinPath: f.o.PinPath}}
	err = spec.LoadAndAssign(&f.objs, opts)
	if errors.Is(err, ebpf.ErrMapIncompatible) {
		// A demux of another version (or another capacity) pinned maps of another layout: start afresh, the clients re-handshake.
		for name := range spec.Maps {
			_ = os.Remove(filepath.Join(f.o.PinPath, name))
		}
		err = spec.LoadAndAssign(&f.objs, opts)
	}
	if err != nil {
		return fmt.Errorf("load the fast path: %w", err)
	}
	f.spec = spec
	return nil
}

// externalDevices are the configured devices, or the one that holds the node's address.
func externalDevices(o Options) ([]int, error) {
	if len(o.Interfaces) > 0 {
		var out []int
		for _, name := range o.Interfaces {
			i, err := net.InterfaceByName(name)
			if err != nil {
				return nil, fmt.Errorf("external device %s: %w", name, err)
			}
			out = append(out, i.Index)
		}
		return out, nil
	}
	ip, err := netip.ParseAddr(o.HostIP)
	if err != nil {
		return nil, fmt.Errorf("HOST_IP %q: %w", o.HostIP, err)
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, i := range ifs {
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if got, ok := netip.AddrFromSlice(n.IP); ok && got.Unmap() == ip {
					return []int{i.Index}, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("no device holds HOST_IP %s", ip)
}

func pinName(kind string, ifindex int) string { return kind + "-" + strconv.Itoa(ifindex) }

func parsePinName(name string) (kind string, ifindex int, ok bool) {
	kind, n, found := strings.Cut(name, "-")
	ifindex, err := strconv.Atoi(n)
	return kind, ifindex, found && err == nil && ifindex > 0
}

func (f *FastPath) linkPath(name string) string { return filepath.Join(f.o.PinPath, linksDir, name) }

func (f *FastPath) isExternal(ifindex int) bool {
	for _, e := range f.ext {
		if e == ifindex {
			return true
		}
	}
	return false
}

func programOf(kind string) (name string, at ebpf.AttachType) {
	switch kind {
	case kindIngress:
		return WgFastpathProgWgTcIngress, ebpf.AttachTCXIngress
	case kindEgress:
		return WgFastpathProgWgTcEgressGuard, ebpf.AttachTCXEgress
	case kindXDP:
		return WgFastpathProgWgXdp, ebpf.AttachXDP
	default:
		return WgFastpathProgWgTcReply, ebpf.AttachTCXIngress
	}
}

func (f *FastPath) program(kind string) (*ebpf.Program, ebpf.AttachType) {
	_, at := programOf(kind)
	switch kind {
	case kindIngress:
		return f.objs.WgTcIngress, at
	case kindEgress:
		return f.objs.WgTcEgressGuard, at
	case kindXDP:
		return f.objs.WgXdp, at
	default:
		return f.objs.WgTcReply, at
	}
}

// instance loads another instance of a program on the same maps: tcx holds a program once per device, so a link can only be
// moved by attaching a second instance before the first goes.
func (f *FastPath) instance(kind string) (*ebpf.Program, error) {
	name, _ := programOf(kind)
	spec := f.spec.Copy()
	for p := range spec.Programs {
		if p != name {
			delete(spec.Programs, p)
		}
	}
	coll, err := ebpf.NewCollectionWithOptions(spec, ebpf.CollectionOptions{MapReplacements: map[string]*ebpf.Map{
		WgFastpathMapWgSessions: f.objs.WgSessions, WgFastpathMapWgEntries: f.objs.WgEntries,
		WgFastpathMapWgEntrySeen: f.objs.WgEntrySeen, WgFastpathMapWgCfg: f.objs.WgCfg, WgFastpathMapWgStats: f.objs.WgStats,
	}})
	if err != nil {
		return nil, fmt.Errorf("load another %s: %w", name, err)
	}
	defer coll.Close()
	return coll.DetachProgram(name), nil
}

// claim reserves a pin name for an attach outside mu; false when it is attached already or being attached.
func (f *FastPath) claim(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.links[name]; ok || f.busy[name] {
		return false
	}
	f.busy[name] = true
	return true
}

// publish ends a claim; l (if not nil) becomes the link of the name.
func (f *FastPath) publish(name string, l link.Link) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.busy, name)
	if l != nil {
		f.links[name] = l
	}
}

// ensureLink attaches a program to a device unless it is attached already. created says the link is new (not one pinned before).
func (f *FastPath) ensureLink(kind string, ifindex int) (created bool, err error) {
	name := pinName(kind, ifindex)
	if !f.claim(name) {
		return false, nil
	}
	var l link.Link
	if kind == kindXDP {
		l, created, err = f.xdp(ifindex)
	} else {
		l, created, err = f.tcx(kind, ifindex)
	}
	f.publish(name, l)
	return created, err
}

// attach puts the programs on the external devices and cilium_host, takes over the reply links of earlier runs whose devices
// still exist, and removes every other pinned link (an xdp link after a switch to tc, a device no longer external). It
// returns the links it created; a foreign XDP program is refused before anything is attached.
func (f *FastPath) attach() (created []string, err error) {
	f.work.Lock()
	defer f.work.Unlock()
	if f.o.Mode == demux.ModeXDP {
		for _, ifindex := range f.ext {
			if err := f.refuseForeignXDP(ifindex); err != nil {
				return nil, err
			}
		}
	}
	add := func(kind string, ifindex int) error {
		c, err := f.ensureLink(kind, ifindex)
		if c {
			created = append(created, pinName(kind, ifindex))
		}
		return err
	}
	for _, ifindex := range f.ext {
		kinds := []string{kindIngress, kindEgress}
		if f.o.Mode == demux.ModeXDP {
			kinds = append(kinds, kindXDP)
		}
		for _, kind := range kinds {
			if err := add(kind, ifindex); err != nil {
				return created, err
			}
		}
	}
	if base, ok := f.baseReply(); ok {
		_ = add(kindReply, base)
	}
	pins, err := os.ReadDir(filepath.Join(f.o.PinPath, linksDir))
	if err != nil {
		return created, err
	}
	for _, p := range pins {
		f.mu.Lock()
		_, ours := f.links[p.Name()]
		f.mu.Unlock()
		if ours {
			continue
		}
		kind, ifindex, ok := parsePinName(p.Name())
		if ok && kind == kindReply && !f.isExternal(ifindex) && deviceExists(ifindex) && add(kind, ifindex) == nil {
			continue
		}
		removePinnedLink(f.linkPath(p.Name()))
	}
	return created, nil
}

// baseReply is cilium_host's index when the device exists (it may come up after the demux) and is not external.
func (f *FastPath) baseReply() (int, bool) {
	base, err := net.InterfaceByName(ReplyBaseDevice)
	if err != nil || f.isExternal(base.Index) {
		return 0, false
	}
	return base.Index, true
}

func deviceExists(ifindex int) bool {
	_, err := net.InterfaceByIndex(ifindex)
	return err == nil
}

func removePinnedLink(path string) {
	if l, err := link.LoadPinnedLink(path, nil); err == nil {
		_ = l.Unpin()
		_ = l.Detach()
		_ = l.Close()
	}
	_ = os.Remove(path)
}

// tcx attaches one tcx program at the head of a device, or gives a pinned link of an earlier run the new program (the link
// keeps its place).
func (f *FastPath) tcx(kind string, ifindex int) (link.Link, bool, error) {
	path := f.linkPath(pinName(kind, ifindex))
	prog, at := f.program(kind)
	if l, err := link.LoadPinnedLink(path, nil); err == nil {
		if info, err := l.Info(); err == nil && info.TCX() != nil && int(info.TCX().Ifindex) == ifindex && l.Update(prog) == nil {
			return l, false, nil
		}
		_ = l.Close()
		removePinnedLink(path)
	}
	l, err := link.AttachTCX(link.TCXOptions{Program: prog, Interface: ifindex, Attach: at, Anchor: link.Head()})
	if err != nil {
		return nil, false, fmt.Errorf("attach %s to device %d: %w", kind, ifindex, err)
	}
	if err := l.Pin(path); err != nil {
		_ = l.Close()
		return nil, false, fmt.Errorf("pin %s of device %d: %w", kind, ifindex, err)
	}
	return l, true, nil
}

// pinnedXDP is our pinned XDP link of a device, if it is still attached there.
func (f *FastPath) pinnedXDP(ifindex int) (link.Link, *link.Info) {
	l, err := link.LoadPinnedLink(f.linkPath(pinName(kindXDP, ifindex)), nil)
	if err != nil {
		return nil, nil
	}
	if info, err := l.Info(); err == nil && info.XDP() != nil && int(info.XDP().Ifindex) == ifindex {
		return l, info
	}
	_ = l.Close()
	return nil, nil
}

// refuseForeignXDP fails when the device has an XDP program that is not ours: the xdp mode never replaces it.
func (f *FastPath) refuseForeignXDP(ifindex int) error {
	id, err := attachedXDP(ifindex)
	if err != nil || id == 0 {
		return err
	}
	if l, info := f.pinnedXDP(ifindex); l != nil {
		_ = l.Close()
		if info.Program == id {
			return nil
		}
	}
	return fmt.Errorf("another XDP program (id %d) is attached to device %d: the xdp mode does not replace it (use the tc mode)", id, ifindex)
}

// xdp attaches the XDP program (native, else generic), or updates the pinned link of an earlier run.
func (f *FastPath) xdp(ifindex int) (link.Link, bool, error) {
	path := f.linkPath(pinName(kindXDP, ifindex))
	if l, _ := f.pinnedXDP(ifindex); l != nil {
		if l.Update(f.objs.WgXdp) == nil {
			return l, false, nil
		}
		_ = l.Close()
	}
	removePinnedLink(path)
	if err := f.refuseForeignXDP(ifindex); err != nil {
		return nil, false, err
	}
	l, err := link.AttachXDP(link.XDPOptions{Program: f.objs.WgXdp, Interface: ifindex, Flags: link.XDPDriverMode})
	if err != nil {
		l, err = link.AttachXDP(link.XDPOptions{Program: f.objs.WgXdp, Interface: ifindex, Flags: link.XDPGenericMode})
	}
	if err != nil {
		return nil, false, fmt.Errorf("attach xdp to device %d: %w", ifindex, err)
	}
	if err := l.Pin(path); err != nil {
		_ = l.Close()
		return nil, false, fmt.Errorf("pin xdp of device %d: %w", ifindex, err)
	}
	return l, true, nil
}

// attachedXDP is the XDP program attached to a device in any mode, 0 for none.
func attachedXDP(ifindex int) (ebpf.ProgramID, error) {
	l, err := netlink.LinkByIndex(ifindex)
	if err != nil {
		return 0, fmt.Errorf("device %d: %w", ifindex, err)
	}
	if x := l.Attrs().Xdp; x != nil && x.Attached {
		return ebpf.ProgramID(x.ProgId), nil
	}
	return 0, nil
}

// EnsureOrder puts our tcx programs back in front where another program took the head, attaches the reply program to
// cilium_host if it came up late, and lets go of the reply links of devices that went away. An external device that went
// away is an error (the fast path is then not attached where the clients' packets arrive). It works on a snapshot of the
// links: the sink is never held up by it.
func (f *FastPath) EnsureOrder() error {
	f.work.Lock()
	defer f.work.Unlock()
	f.mu.Lock()
	clear(f.sources)
	f.mu.Unlock()

	var errs []error
	// A re-attach that failed last time is tried again.
	for _, ifindex := range f.ext {
		for _, kind := range []string{kindIngress, kindEgress} {
			if _, err := f.ensureLink(kind, ifindex); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if base, ok := f.baseReply(); ok {
		_, _ = f.ensureLink(kindReply, base)
	}
	f.mu.Lock()
	snapshot := maps.Clone(f.links)
	f.mu.Unlock()
	for name, l := range snapshot {
		kind, ifindex, _ := parsePinName(name)
		if kind == kindXDP {
			continue
		}
		if err := f.front(name, kind, ifindex, l); err != nil {
			errs = append(errs, err)
		}
	}
	f.mu.Lock()
	f.order = errors.Join(errs...)
	f.mu.Unlock()
	return errors.Join(errs...)
}

// front keeps one tcx link first on its device.
func (f *FastPath) front(name, kind string, ifindex int, l link.Link) error {
	info, err := l.Info()
	if err != nil || info.TCX() == nil || int(info.TCX().Ifindex) != ifindex {
		if kind != kindReply {
			return fmt.Errorf("the %s link of device %d is gone", kind, ifindex)
		}
		// The pin goes first: a sink that finds the name unclaimed then pins its own link, not one this removes.
		removePinnedLink(f.linkPath(name))
		f.mu.Lock()
		if f.links[name] == l {
			delete(f.links, name)
		}
		f.mu.Unlock()
		_ = l.Close()
		return nil
	}
	_, at := programOf(kind)
	r, err := f.query(link.QueryOptions{Target: ifindex, Attach: at})
	if err != nil {
		return err
	}
	if len(r.Programs) > 0 {
		if id, ok := r.Programs[0].LinkID(); ok && id == info.ID {
			return nil
		}
	}
	// Attach a second instance at the head, pin it in the old link's place, then let the old one go: the device is never
	// without our program.
	prog, err := f.instance(kind)
	if err != nil {
		return err
	}
	defer func() { _ = prog.Close() }()
	head, err := link.AttachTCX(link.TCXOptions{Program: prog, Interface: ifindex, Attach: at, Anchor: link.Head()})
	if err != nil {
		return fmt.Errorf("re-attach %s to device %d: %w", kind, ifindex, err)
	}
	path := f.linkPath(name)
	// bpffs refuses dots in names; a leftover "-new" pin is removed by the next Open (not a valid pin name).
	if err := head.Pin(path + "-new"); err != nil {
		_ = head.Close()
		return fmt.Errorf("pin %s of device %d: %w", kind, ifindex, err)
	}
	if err := os.Rename(path+"-new", path); err != nil {
		_ = head.Unpin()
		_ = head.Close()
		return fmt.Errorf("move the pin of %s of device %d: %w", kind, ifindex, err)
	}
	f.mu.Lock()
	f.links[name] = head
	f.mu.Unlock()
	_ = l.Detach()
	_ = l.Close()
	return nil
}

// Ready says whether the fast path is attached in front on every device and knows the entry addresses.
func (f *FastPath) Ready() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.order != nil {
		return f.order
	}
	if !f.entries {
		return errors.New("the entry addresses are not known yet")
	}
	return nil
}

// Close lets go of the programs, maps and links; the pins stay, so the kernel keeps forwarding until the next demux takes over.
func (f *FastPath) Close() error {
	f.work.Lock()
	defer f.work.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, l := range f.links {
		_ = l.Close()
		delete(f.links, name)
	}
	return f.objs.Close()
}

// Detach removes every link and pin of the fast path under pinPath: the programs leave the devices and the maps go away. For a
// switch back to the userspace mode, with no demux running on the node.
func Detach(pinPath string) error {
	dir := filepath.Join(pinPath, linksDir)
	pins, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, p := range pins {
		removePinnedLink(filepath.Join(dir, p.Name()))
	}
	if err := os.RemoveAll(pinPath); err != nil {
		return fmt.Errorf("remove %s: %w", pinPath, err)
	}
	return nil
}
