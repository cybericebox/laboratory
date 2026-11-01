//go:build linux

package gateway

import (
	"errors"
	"fmt"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Shaper keeps every lab interface of the gateway pod to the ceilings the operator wrote for it (the annotation of the pod, see package
// bandwidth), one for each direction: what the lab sends to the internet (upload) is policed on the ingress of its interface, what it
// receives (download) is shaped on the egress. The pure ACKs of the lab (a TCP segment with only the ACK flag and no payload) are not
// counted in the upload: the ACKs of a fast download must not slow it down. A ceiling of 0 removes that direction. The ceilings are the
// same for every lab of the pod; the operator has already divided what is shared between the labs.
type Shaper struct {
	mu       sync.Mutex
	down, up int64
	ifaces   map[string]bool
	applied  map[string][2]int64 // the ceilings each interface has now; an interface is touched only when they differ
}

// NewShaper makes a shaper with no ceiling.
func NewShaper() *Shaper { return &Shaper{ifaces: map[string]bool{}, applied: map[string][2]int64{}} }

// SetLimit sets the same ceiling in both directions.
func (s *Shaper) SetLimit(kbps int64) error { return s.SetLimits(kbps, kbps) }

// SetLimits changes the ceilings (kbit/s) of every lab interface that is tracked. The first error is returned, the rest are still done.
func (s *Shaper) SetLimits(down, up int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down, s.up = down, up
	var first error
	for iface := range s.ifaces {
		if err := s.applyLocked(iface); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Track puts the current ceiling on a lab interface and keeps it there when the ceiling changes.
func (s *Shaper) Track(iface string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ifaces[iface] = true
	return s.applyLocked(iface)
}

// Forget stops tracking an interface and takes the shaping off it (the interface may be gone already, which is not an error).
func (s *Shaper) Forget(iface string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ifaces, iface)
	delete(s.applied, iface)
	_ = clearShaping(iface)
}

func (s *Shaper) applyLocked(iface string) error {
	want := [2]int64{s.down, s.up}
	if got, ok := s.applied[iface]; ok && got == want {
		return nil
	}
	var err error
	if s.down <= 0 && s.up <= 0 {
		err = clearShaping(iface)
	} else {
		err = applyShaping(iface, s.down, s.up)
	}
	if err != nil {
		delete(s.applied, iface) // tried again at the next reconcile
		return err
	}
	s.applied[iface] = want
	return nil
}

var (
	rootHandle    = netlink.MakeHandle(1, 0)
	ingressHandle = netlink.MakeHandle(0xffff, 0)
)

// applyShaping replaces the qdiscs and the police of the interface (replace: no gap in which the interface is unshaped). down is the
// ceiling of what the lab receives (the egress token bucket), up of what it sends (the ingress police); 0 leaves that direction free.
func applyShaping(iface string, down, up int64) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("shape %s: %w", iface, err)
	}
	if down > 0 {
		rate, burst, limit, _ := shapeParams(down)
		if err := netlink.QdiscReplace(&netlink.Tbf{
			QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Handle: rootHandle, Parent: netlink.HANDLE_ROOT},
			Rate:       rate,
			Limit:      limit,
			Buffer:     netlink.Xmittime(rate, burst),
		}); err != nil {
			return fmt.Errorf("shape the egress of %s: %w", iface, err)
		}
	} else if err := netlink.QdiscDel(&netlink.Tbf{QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Handle: rootHandle, Parent: netlink.HANDLE_ROOT}}); err != nil && !isNoQdisc(err) {
		return fmt.Errorf("remove the egress shaping of %s: %w", iface, err)
	}
	if up <= 0 {
		return clearIngress(link)
	}
	rate, _, _, policeBurst := shapeParams(up)
	// The ingress qdisc cannot be replaced by itself (the kernel answers "invalid argument"): it is added when it is not there.
	if !hasIngress(link) {
		if err := netlink.QdiscAdd(&netlink.Ingress{
			QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Handle: ingressHandle, Parent: netlink.HANDLE_INGRESS},
		}); err != nil {
			return fmt.Errorf("add the ingress qdisc of %s: %w", iface, err)
		}
	}
	police := netlink.NewPoliceAction()
	police.Rate = uint32(min(rate, 1<<32-1))
	police.Burst = policeBurst
	police.Mtu = shapeMTU
	police.ExceedAction = netlink.TC_POLICE_SHOT
	police.NotExceedAction = netlink.TC_POLICE_OK
	// A filter cannot be changed in place (the kernel answers "file exists"): the old ones go, the new ones come. For the moment between
	// the two the ingress is not policed.
	if old, err := netlink.FilterList(link, ingressHandle); err == nil {
		for _, f := range old {
			if err := netlink.FilterDel(f); err != nil {
				return fmt.Errorf("police the ingress of %s: %w", iface, err)
			}
		}
	}
	// 1 and 2: a payload-less ACK is counted in a police of its own (see ackKbps). 3: everything else is policed at the ceiling.
	for i, f := range pureACKFilters(link.Attrs().Index, ackKbps(down)) {
		f.Priority = uint16(i + 1)
		if err := netlink.FilterAdd(f); err != nil {
			return fmt.Errorf("add the pure-ACK filter of %s: %w", iface, err)
		}
	}
	if err := netlink.FilterAdd(&netlink.MatchAll{
		FilterAttrs: netlink.FilterAttrs{LinkIndex: link.Attrs().Index, Parent: ingressHandle, Priority: 3, Protocol: unix.ETH_P_ALL},
		Actions:     []netlink.Action{police},
	}); err != nil {
		return fmt.Errorf("add the police filter of %s (rate %d, burst %d): %w", iface, rate, policeBurst, err)
	}
	return nil
}

// ackKbps is the ceiling of the payload-less ACKs of a lab: a receiver sends about one 52-byte ACK for two 1448-byte segments, so a
// download of down kbit/s needs about down/56 kbit/s of them. Twice that is allowed, between 256 kbit/s and 4 Mbit/s: a lab cannot use
// the exemption to send a stream of its own (a segment with payload, or a faster stream of empty ones, meets the police of the ceiling).
func ackKbps(down int64) int64 {
	if down <= 0 {
		return 1024
	}
	return min(max(down/28, 256), 4096)
}

// pureACKFilters match an IPv4 TCP segment with the ACK flag alone (no SYN, FIN, RST, PSH) and NO payload: the total length is exactly the
// 20-byte IP header plus a 20-byte TCP header (data offset 5, 40 bytes) or plus 32 bytes with the timestamp options (data offset 8, 52
// bytes). Each has a police of its own at kbps, so what is exempt from the ceiling is bounded too.
func pureACKFilters(linkIndex int, kbps int64) []*netlink.U32 {
	mk := func(totalLen, offset uint32) *netlink.U32 {
		police := netlink.NewPoliceAction()
		rate := uint64(kbps) * 125
		police.Rate = uint32(min(rate, 1<<32-1))
		police.Burst = uint32(max(rate/10, 16<<10)) // a tenth of a second, at least a few hundred ACKs
		police.Mtu = 1600
		police.ExceedAction = netlink.TC_POLICE_SHOT
		police.NotExceedAction = netlink.TC_POLICE_OK
		return &netlink.U32{
			FilterAttrs: netlink.FilterAttrs{LinkIndex: linkIndex, Parent: ingressHandle, Protocol: unix.ETH_P_IP},
			Sel: &netlink.TcU32Sel{
				Flags: netlink.TC_U32_TERMINAL,
				Keys: []netlink.TcU32Key{
					{Mask: 0xff00ffff, Val: 0x45000000 | totalLen, Off: 0},    // version 4, header length 5 words, total length exactly this
					{Mask: 0x00ff0000, Val: 0x00060000, Off: 8},               // protocol TCP
					{Mask: 0xf01f0000, Val: offset<<28 | 0x00100000, Off: 32}, // TCP data offset, and the flags: ACK only
				},
			},
			Actions: []netlink.Action{police},
		}
	}
	return []*netlink.U32{mk(40, 5), mk(52, 8)}
}

// clearIngress removes the ingress qdisc (and with it the filters) when there is one.
func clearIngress(link netlink.Link) error {
	if !hasIngress(link) {
		return nil
	}
	if err := netlink.QdiscDel(&netlink.Ingress{QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Handle: ingressHandle, Parent: netlink.HANDLE_INGRESS}}); err != nil && !isNoQdisc(err) {
		return fmt.Errorf("remove the ingress police of %s: %w", link.Attrs().Name, err)
	}
	return nil
}

func hasIngress(link netlink.Link) bool {
	qdiscs, err := netlink.QdiscList(link)
	if err != nil {
		return false
	}
	for _, q := range qdiscs {
		if _, ok := q.(*netlink.Ingress); ok {
			return true
		}
	}
	return false
}

// clearShaping removes what applyShaping put on the interface; an interface that is gone or was never shaped is fine.
func clearShaping(iface string) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return nil
	}
	var first error
	for _, q := range []netlink.Qdisc{
		&netlink.Tbf{QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Handle: rootHandle, Parent: netlink.HANDLE_ROOT}},
		&netlink.Ingress{QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Handle: ingressHandle, Parent: netlink.HANDLE_INGRESS}},
	} {
		if err := netlink.QdiscDel(q); err != nil && !isNoQdisc(err) && first == nil {
			first = fmt.Errorf("remove the shaping of %s: %w", iface, err)
		}
	}
	return first
}

// isNoQdisc says the interface had no such qdisc (ENOENT, EINVAL for a handle that is not there, ENODEV).
func isNoQdisc(err error) bool {
	for _, e := range []error{unix.ENOENT, unix.EINVAL, unix.ENODEV} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
