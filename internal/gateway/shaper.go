//go:build linux

package gateway

import (
	"errors"
	"fmt"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Shaper keeps every lab interface of the gateway pod to the ceiling the operator wrote for it (the annotation of the pod, see package
// bandwidth): the lab's traffic towards the internet is policed on the ingress of its interface, the traffic towards the lab is shaped
// on the egress. A ceiling of 0 removes both. The ceiling is the same for every lab of the pod; the operator has already divided
// the internet class of the node between the labs that share it.
type Shaper struct {
	mu      sync.Mutex
	kbps    int64
	ifaces  map[string]bool
	applied map[string]int64 // the ceiling each interface has now; an interface is touched only when this differs
}

// NewShaper makes a shaper with no ceiling.
func NewShaper() *Shaper { return &Shaper{ifaces: map[string]bool{}, applied: map[string]int64{}} }

// SetLimit changes the ceiling of every lab interface that is tracked. The first error is returned, the rest are still done.
func (s *Shaper) SetLimit(kbps int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kbps = kbps
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
	if got, ok := s.applied[iface]; ok && got == s.kbps {
		return nil
	}
	var err error
	if s.kbps <= 0 {
		err = clearShaping(iface)
	} else {
		err = applyShaping(iface, s.kbps)
	}
	if err != nil {
		delete(s.applied, iface) // tried again at the next reconcile
		return err
	}
	s.applied[iface] = s.kbps
	return nil
}

var (
	rootHandle    = netlink.MakeHandle(1, 0)
	ingressHandle = netlink.MakeHandle(0xffff, 0)
)

// applyShaping replaces the qdiscs and the police of the interface (replace: no gap in which the interface is unshaped).
func applyShaping(iface string, kbps int64) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("shape %s: %w", iface, err)
	}
	rate, burst, limit := shapeParams(kbps)
	if err := netlink.QdiscReplace(&netlink.Tbf{
		QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Handle: rootHandle, Parent: netlink.HANDLE_ROOT},
		Rate:       rate,
		Limit:      limit,
		Buffer:     netlink.Xmittime(rate, burst),
	}); err != nil {
		return fmt.Errorf("shape the egress of %s: %w", iface, err)
	}
	if err := netlink.QdiscReplace(&netlink.Ingress{
		QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Handle: ingressHandle, Parent: netlink.HANDLE_INGRESS},
	}); err != nil {
		return fmt.Errorf("police the ingress of %s: %w", iface, err)
	}
	police := netlink.NewPoliceAction()
	police.Rate = uint32(min(rate, 1<<32-1))
	police.Burst = burst
	police.Mtu = shapeMTU
	police.ExceedAction = netlink.TC_POLICE_SHOT
	police.NotExceedAction = netlink.TC_POLICE_OK
	if err := netlink.FilterReplace(&netlink.MatchAll{
		FilterAttrs: netlink.FilterAttrs{LinkIndex: link.Attrs().Index, Parent: ingressHandle, Priority: 1, Protocol: unix.ETH_P_ALL},
		Actions:     []netlink.Action{police},
	}); err != nil {
		return fmt.Errorf("police the ingress of %s: %w", iface, err)
	}
	return nil
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
