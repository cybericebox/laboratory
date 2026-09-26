package dhcp

import (
	"fmt"
	"net"
	"sync"
)

// Range is an inclusive host-offset interval in a lab /24.
type Range struct {
	Start int32
	End   int32
}

func ValidateRanges(ranges []Range) error {
	if len(ranges) == 0 {
		return fmt.Errorf("at least one DHCP range is required")
	}
	used := make(map[int32]bool)
	for _, r := range ranges {
		if r.Start < 2 || r.End > 254 || r.Start > r.End {
			return fmt.Errorf("invalid DHCP range %d..%d: hosts must be within 2..254", r.Start, r.End)
		}
		for host := r.Start; host <= r.End; host++ {
			if used[host] {
				return fmt.Errorf("overlapping DHCP range at host %d", host)
			}
			used[host] = true
		}
	}
	return nil
}

// ipPool allocates IPv4 addresses only from configured ranges. Gateway is
// always skipped. Allocation is sticky per MAC (in-memory only).
type ipPool struct {
	mu     sync.Mutex
	subnet *net.IPNet
	gw     net.IP
	ranges []Range
	byMAC  map[string]net.IP
	used   map[string]bool
}

func newIPPool(subnet *net.IPNet, gw net.IP, ranges []Range) *ipPool {
	return &ipPool{
		subnet: subnet,
		gw:     gw.To4(),
		ranges: append([]Range(nil), ranges...),
		byMAC:  make(map[string]net.IP),
		used:   make(map[string]bool),
	}
}

func (p *ipPool) Allocate(mac net.HardwareAddr) (net.IP, error) {
	if mac == nil {
		return nil, fmt.Errorf("nil MAC")
	}
	key := mac.String()

	p.mu.Lock()
	defer p.mu.Unlock()

	if ip, ok := p.byMAC[key]; ok {
		return ip, nil
	}

	base := p.subnet.IP.To4()
	if base == nil {
		return nil, fmt.Errorf("DHCP subnet is not IPv4: %s", p.subnet)
	}
	for _, r := range p.ranges {
		for host := r.Start; host <= r.End; host++ {
			cand := makeIP(base, uint32(host))
			if cand.Equal(p.gw) || p.used[cand.String()] {
				continue
			}
			p.used[cand.String()] = true
			p.byMAC[key] = cand
			return cand, nil
		}
	}
	return nil, fmt.Errorf("pool exhausted for subnet %s", p.subnet)
}

func makeIP(base net.IP, offset uint32) net.IP {
	b := base.To4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += offset
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).To4()
}
