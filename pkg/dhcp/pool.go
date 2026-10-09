package dhcp

import (
	"fmt"
	"net"
	"sync"
	"time"
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

const (
	leaseDuration   = 24 * time.Hour
	offerDuration   = 30 * time.Second
	declineDuration = 10 * time.Minute
)

type lease struct {
	owner     string
	ip        net.IP
	expires   time.Time
	committed bool
}

// ipPool holds bounded reservations; offers and leases have different TTLs.
type ipPool struct {
	mu         sync.Mutex
	subnet     *net.IPNet
	gw         net.IP
	ranges     []Range
	byMAC      map[string]lease
	quarantine map[string]time.Time
	retired    map[string]lease
	now        func() time.Time
}

func newIPPool(subnet *net.IPNet, gw net.IP, ranges []Range) *ipPool {
	return &ipPool{
		subnet: &net.IPNet{
			IP:   append(net.IP(nil), subnet.IP.Mask(subnet.Mask)...),
			Mask: append(net.IPMask(nil), subnet.Mask...),
		},
		gw:         append(net.IP(nil), gw.To4()...),
		ranges:     append([]Range(nil), ranges...),
		byMAC:      map[string]lease{},
		quarantine: map[string]time.Time{},
		retired:    map[string]lease{},
		now:        time.Now,
	}
}
func validMAC(mac net.HardwareAddr) bool {
	if len(mac) == 0 || len(mac) > 16 {
		return false
	}
	for _, b := range mac {
		if b != 0 {
			return true
		}
	}
	return false
}
func (p *ipPool) purge() {
	now := p.now()
	for ip, r := range p.retired {
		if !now.Before(r.expires) {
			delete(p.retired, ip)
		}
	}
	for owner, r := range p.byMAC {
		if !now.Before(r.expires) {
			delete(p.byMAC, owner)
		}
	}
	for ip, until := range p.quarantine {
		if !now.Before(until) {
			delete(p.quarantine, ip)
		}
	}
}
func (p *ipPool) inRange(ip net.IP) bool {
	ip = ip.To4()
	if ip == nil || !p.subnet.Contains(ip) || ip.Equal(p.gw) {
		return false
	}
	host := int32(ip[3]) - int32(p.subnet.IP.To4()[3])
	for _, r := range p.ranges {
		if host >= r.Start && host <= r.End {
			return true
		}
	}
	return false
}
func (p *ipPool) free(ip net.IP, owner string) bool {
	if _, ok := p.retired[ip.String()]; ok {
		return false
	}
	if _, ok := p.quarantine[ip.String()]; ok {
		return false
	}
	for key, r := range p.byMAC {
		if key != owner && r.ip.Equal(ip) {
			return false
		}
	}
	return true
}
func (p *ipPool) Offer(mac net.HardwareAddr) (net.IP, error) {
	if !validMAC(mac) {
		return nil, fmt.Errorf("invalid MAC")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.purge()
	key := mac.String()
	if r, ok := p.byMAC[key]; ok {
		if p.inRange(r.ip) {
			return append(net.IP(nil), r.ip...), nil
		}
		// Reserve the old committed address until its original TTL while this
		// owner obtains a new eligible offer. Never advertise an unusable lease.
		if r.committed {
			p.retired[r.ip.String()] = r
		}
		delete(p.byMAC, key)
	}
	for _, r := range p.ranges {
		for host := r.Start; host <= r.End; host++ {
			ip := makeIP(p.subnet.IP, uint32(host))
			if !p.inRange(ip) || !p.free(ip, key) {
				continue
			}
			p.byMAC[key] = lease{owner: key, ip: ip, expires: p.now().Add(offerDuration)}
			return append(net.IP(nil), ip...), nil
		}
	}
	return nil, fmt.Errorf("pool exhausted for subnet %s", p.subnet)
}
func (p *ipPool) Commit(mac net.HardwareAddr, ip net.IP) (net.IP, error) {
	if !validMAC(mac) {
		return nil, fmt.Errorf("invalid MAC")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.purge()
	key := mac.String()
	if !p.inRange(ip) || !p.free(ip, key) {
		return nil, fmt.Errorf("requested address unavailable")
	}
	if old, ok := p.byMAC[key]; ok && !old.ip.Equal(ip) {
		return nil, fmt.Errorf("requested address differs from reservation")
	}
	ip = append(net.IP(nil), ip.To4()...)
	p.byMAC[key] = lease{owner: key, ip: ip, expires: p.now().Add(leaseDuration), committed: true}
	return append(net.IP(nil), ip...), nil
}
func (p *ipPool) Allocate(mac net.HardwareAddr) (net.IP, error) {
	ip, err := p.Offer(mac)
	if err != nil {
		return nil, err
	}
	return p.Commit(mac, ip)
}
func (p *ipPool) Release(mac net.HardwareAddr, ip net.IP) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.purge()
	key := mac.String()
	r, ok := p.byMAC[key]
	if !ok || !r.ip.Equal(ip) {
		retired, exists := p.retired[ip.String()]
		if !exists || retired.owner != key {
			return false
		}
		delete(p.retired, ip.String())
		return true
	}
	delete(p.byMAC, key)
	return true
}
func (p *ipPool) Decline(mac net.HardwareAddr, ip net.IP) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.purge()
	key := mac.String()
	r, ok := p.byMAC[key]
	if !ok || !r.ip.Equal(ip) {
		return false
	}
	delete(p.byMAC, key)
	p.quarantine[ip.String()] = p.now().Add(declineDuration)
	return true
}
func (p *ipPool) UpdateRanges(ranges []Range) error {
	if err := ValidateRanges(ranges); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.purge()
	p.ranges = append([]Range(nil), ranges...)
	for key, r := range p.byMAC {
		if !r.committed && !p.inRange(r.ip) {
			delete(p.byMAC, key)
		}
	}
	return nil
}

func makeIP(base net.IP, offset uint32) net.IP {
	b := base.To4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += offset
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).To4()
}
