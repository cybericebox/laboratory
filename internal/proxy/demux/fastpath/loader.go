//go:build linux

package fastpath

import (
	"errors"
	"fmt"
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
type FastPath struct {
	o    Options
	objs WgFastpathObjects
	ext  []int

	mu      sync.Mutex
	links   map[string]link.Link      // by pin name
	sources map[netip.Addr]netip.Addr // the kernel's preferred source towards a server, until the next EnsureOrder
	entries bool                      // SetEntries has given at least one address
	order   error                     // the last EnsureOrder's result
}

// Open loads the programs, re-opens the maps a previous demux pinned (or creates and pins them), and attaches the programs at
// the head of the devices: a pinned link of a previous demux gets the new program in place, so forwarding never stops. It
// refuses xdp mode when another XDP program is attached to an external device.
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
	f := &FastPath{o: o, ext: ext, links: map[string]link.Link{}, sources: map[netip.Addr]netip.Addr{}}
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
	if err := f.attach(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.EnsureOrder(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
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

func (f *FastPath) program(kind string) (*ebpf.Program, ebpf.AttachType) {
	switch kind {
	case kindIngress:
		return f.objs.WgTcIngress, ebpf.AttachTCXIngress
	case kindEgress:
		return f.objs.WgTcEgressGuard, ebpf.AttachTCXEgress
	case kindXDP:
		return f.objs.WgXdp, ebpf.AttachXDP
	default:
		return f.objs.WgTcReply, ebpf.AttachTCXIngress
	}
}

// attach puts the programs on the external devices and cilium_host, takes over the reply links of earlier runs whose devices
// still exist, and removes every other pinned link (an xdp link after a switch to tc, a device no longer external).
func (f *FastPath) attach() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ifindex := range f.ext {
		for _, kind := range []string{kindIngress, kindEgress} {
			if err := f.tcxLocked(kind, ifindex); err != nil {
				return err
			}
		}
		if f.o.Mode == demux.ModeXDP {
			if err := f.xdpLocked(ifindex); err != nil {
				return err
			}
		}
	}
	f.baseReplyLocked()
	pins, err := os.ReadDir(filepath.Join(f.o.PinPath, linksDir))
	if err != nil {
		return err
	}
	for _, p := range pins {
		if _, ours := f.links[p.Name()]; ours {
			continue
		}
		kind, ifindex, ok := parsePinName(p.Name())
		if ok && kind == kindReply && !f.isExternal(ifindex) && deviceExists(ifindex) && f.tcxLocked(kind, ifindex) == nil {
			continue
		}
		removePinnedLink(f.linkPath(p.Name()))
	}
	return nil
}

// baseReplyLocked attaches the reply program to cilium_host when the device exists (it may come up after the demux).
func (f *FastPath) baseReplyLocked() {
	base, err := net.InterfaceByName(ReplyBaseDevice)
	if err != nil || f.isExternal(base.Index) {
		return
	}
	if _, ok := f.links[pinName(kindReply, base.Index)]; !ok {
		_ = f.tcxLocked(kindReply, base.Index)
	}
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

// tcxLocked attaches one tcx program at the head of a device, or gives a pinned link of an earlier run the new program (the
// link keeps its place).
func (f *FastPath) tcxLocked(kind string, ifindex int) error {
	name := pinName(kind, ifindex)
	path := f.linkPath(name)
	prog, at := f.program(kind)
	if l, err := link.LoadPinnedLink(path, nil); err == nil {
		if info, err := l.Info(); err == nil && info.TCX() != nil && int(info.TCX().Ifindex) == ifindex && l.Update(prog) == nil {
			f.links[name] = l
			return nil
		}
		_ = l.Close()
		removePinnedLink(path)
	}
	l, err := link.AttachTCX(link.TCXOptions{Program: prog, Interface: ifindex, Attach: at, Anchor: link.Head()})
	if err != nil {
		return fmt.Errorf("attach %s to device %d: %w", kind, ifindex, err)
	}
	if err := l.Pin(path); err != nil {
		_ = l.Close()
		return fmt.Errorf("pin %s of device %d: %w", kind, ifindex, err)
	}
	f.links[name] = l
	return nil
}

// xdpLocked attaches the XDP program (native, else generic), or updates the pinned link of an earlier run. Another program's
// XDP attachment is never replaced.
func (f *FastPath) xdpLocked(ifindex int) error {
	name := pinName(kindXDP, ifindex)
	path := f.linkPath(name)
	if l, err := link.LoadPinnedLink(path, nil); err == nil {
		if info, err := l.Info(); err == nil && info.XDP() != nil && int(info.XDP().Ifindex) == ifindex && l.Update(f.objs.WgXdp) == nil {
			f.links[name] = l
			return nil
		}
		_ = l.Close()
		removePinnedLink(path)
	}
	if id, err := attachedXDP(ifindex); err != nil {
		return err
	} else if id != 0 {
		return fmt.Errorf("another XDP program (id %d) is attached to device %d: the xdp mode does not replace it (use the tc mode)", id, ifindex)
	}
	l, err := link.AttachXDP(link.XDPOptions{Program: f.objs.WgXdp, Interface: ifindex, Flags: link.XDPDriverMode})
	if err != nil {
		l, err = link.AttachXDP(link.XDPOptions{Program: f.objs.WgXdp, Interface: ifindex, Flags: link.XDPGenericMode})
	}
	if err != nil {
		return fmt.Errorf("attach xdp to device %d: %w", ifindex, err)
	}
	if err := l.Pin(path); err != nil {
		_ = l.Close()
		return fmt.Errorf("pin xdp of device %d: %w", ifindex, err)
	}
	f.links[name] = l
	return nil
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
// away is an error (the fast path is then not attached where the clients' packets arrive).
func (f *FastPath) EnsureOrder() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.sources)
	f.baseReplyLocked()
	var errs []error
	// A re-attach that failed last time is tried again.
	for _, ifindex := range f.ext {
		for _, kind := range []string{kindIngress, kindEgress} {
			if _, ok := f.links[pinName(kind, ifindex)]; !ok {
				if err := f.tcxLocked(kind, ifindex); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	for name, l := range f.links {
		kind, ifindex, _ := parsePinName(name)
		if kind == kindXDP {
			continue
		}
		info, err := l.Info()
		if err != nil || info.TCX() == nil || int(info.TCX().Ifindex) != ifindex {
			if kind == kindReply {
				_ = l.Close()
				removePinnedLink(f.linkPath(name))
				delete(f.links, name)
				continue
			}
			errs = append(errs, fmt.Errorf("the %s link of device %d is gone", kind, ifindex))
			continue
		}
		prog, at := f.program(kind)
		r, err := link.QueryPrograms(link.QueryOptions{Target: ifindex, Attach: at})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(r.Programs) > 0 {
			if id, ok := r.Programs[0].LinkID(); ok && id == info.ID {
				continue
			}
		}
		// tcx holds a program once per device, so the old link goes first. For the moment in between the packets take the
		// slow path, which knows every session.
		_ = l.Unpin()
		_ = l.Detach()
		_ = l.Close()
		delete(f.links, name)
		head, err := link.AttachTCX(link.TCXOptions{Program: prog, Interface: ifindex, Attach: at, Anchor: link.Head()})
		if err != nil {
			errs = append(errs, fmt.Errorf("re-attach %s to device %d: %w", kind, ifindex, err))
			continue
		}
		if err := head.Pin(f.linkPath(name)); err != nil {
			errs = append(errs, fmt.Errorf("pin %s of device %d: %w", kind, ifindex, err))
		}
		f.links[name] = head
	}
	f.order = errors.Join(errs...)
	return f.order
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
