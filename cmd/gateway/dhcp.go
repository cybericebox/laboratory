//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
)

type DHCPManager struct {
	mu      sync.Mutex
	servers map[string]*serverEntry
}

type serverEntry struct {
	cancel context.CancelFunc
	pool   *ipPool
}

func newDHCPManager() *DHCPManager {
	return &DHCPManager{servers: make(map[string]*serverEntry)}
}

type DHCPConfig struct {
	Iface    string
	Subnet   string
	Gateway  string
	DNS      string
	Range    string   // "start,end" inclusive; empty = full host range
	Reserved []net.IP // static-addressed devices to skip in allocation
}

func (m *DHCPManager) Start(labName string, cfg DHCPConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[labName]; ok {
		return nil
	}

	_, subnet, err := net.ParseCIDR(cfg.Subnet)
	if err != nil {
		return fmt.Errorf("parse subnet %q: %w", cfg.Subnet, err)
	}
	gw := net.ParseIP(cfg.Gateway)
	dns := net.ParseIP(cfg.DNS)
	if gw == nil {
		return fmt.Errorf("invalid gateway %q", cfg.Gateway)
	}

	pool, err := newIPPool(subnet, gw, cfg.Range, cfg.Reserved)
	if err != nil {
		return fmt.Errorf("init IP pool: %w", err)
	}

	handler := func(conn net.PacketConn, peer net.Addr, msg *dhcpv4.DHCPv4) {
		assigned, err := pool.Allocate(msg.ClientHWAddr)
		if err != nil {
			return
		}
		var reply *dhcpv4.DHCPv4
		switch msg.MessageType() {
		case dhcpv4.MessageTypeDiscover:
			reply, _ = dhcpv4.NewReplyFromRequest(msg,
				dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer),
				dhcpv4.WithYourIP(assigned),
				dhcpv4.WithNetmask(subnet.Mask),
				dhcpv4.WithRouter(gw),
				dhcpv4.WithDNS(dns),
				dhcpv4.WithLeaseTime(86400),
			)
		case dhcpv4.MessageTypeRequest:
			reply, _ = dhcpv4.NewReplyFromRequest(msg,
				dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
				dhcpv4.WithYourIP(assigned),
				dhcpv4.WithNetmask(subnet.Mask),
				dhcpv4.WithRouter(gw),
				dhcpv4.WithDNS(dns),
				dhcpv4.WithLeaseTime(86400),
			)
		}
		if reply != nil {
			_, _ = conn.WriteTo(reply.ToBytes(), peer)
		}
	}

	laddr := &net.UDPAddr{Port: 67, IP: net.ParseIP("0.0.0.0")}
	srv, err := server4.NewServer(cfg.Iface, laddr, handler)
	if err != nil {
		return fmt.Errorf("new DHCP server on %s: %w", cfg.Iface, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.servers[labName] = &serverEntry{cancel: cancel, pool: pool}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	go srv.Serve()
	return nil
}

func (m *DHCPManager) Stop(labName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.servers[labName]; ok {
		e.cancel()
		delete(m.servers, labName)
	}
}

// ipPool allocates IPv4 addresses inside a /24-like subnet. When rangeStart /
// rangeEnd are set the pool only hands out IPs in [start, end] inclusive;
// otherwise the full host range is used. The gateway plus any explicitly
// reserved IPs (devices with static addressing on the same broadcast domain)
// are always skipped.
//
// Allocation is sticky per MAC for the lifetime of the server (renewals get
// the same IP); state is in-memory only, lost on restart (REQ-GW-038).
type ipPool struct {
	mu                 sync.Mutex
	subnet             *net.IPNet
	gw                 net.IP
	rangeStart         net.IP // inclusive; nil → first host
	rangeEnd           net.IP // inclusive; nil → last host
	byMAC              map[string]net.IP
	used               map[string]bool
}

// newIPPool returns an allocator over subnet.
// rangeSpec: empty → full host range; "ip1,ip2" → bounded inclusive.
// reserved: IPs to mark used at construction time (static-addressed devices).
func newIPPool(subnet *net.IPNet, gw net.IP, rangeSpec string, reserved []net.IP) (*ipPool, error) {
	p := &ipPool{
		subnet: subnet,
		gw:     gw.To4(),
		byMAC:  make(map[string]net.IP),
		used:   make(map[string]bool),
	}
	if rangeSpec != "" {
		start, end, err := parseRange(rangeSpec, subnet)
		if err != nil {
			return nil, err
		}
		p.rangeStart = start
		p.rangeEnd = end
	}
	for _, ip := range reserved {
		if ip4 := ip.To4(); ip4 != nil {
			p.used[ip4.String()] = true
		}
	}
	return p, nil
}

// parseRange interprets "ip1,ip2" within subnet and returns inclusive bounds.
func parseRange(spec string, subnet *net.IPNet) (net.IP, net.IP, error) {
	parts := strings.SplitN(spec, ",", 2)
	if len(parts) != 2 {
		return nil, nil, fmt.Errorf("range %q: expected \"start,end\"", spec)
	}
	startStr := strings.TrimSpace(parts[0])
	endStr := strings.TrimSpace(parts[1])
	start := net.ParseIP(startStr)
	end := net.ParseIP(endStr)
	if start == nil || end == nil {
		return nil, nil, fmt.Errorf("range %q: invalid IPs", spec)
	}
	start = start.To4()
	end = end.To4()
	if start == nil || end == nil {
		return nil, nil, fmt.Errorf("range %q: IPv4 required", spec)
	}
	if !subnet.Contains(start) || !subnet.Contains(end) {
		return nil, nil, fmt.Errorf("range %q: IPs outside subnet %s", spec, subnet)
	}
	if ipToUint(start) > ipToUint(end) {
		return nil, nil, fmt.Errorf("range %q: start > end", spec)
	}
	return start, end, nil
}

func ipToUint(ip net.IP) uint32 {
	b := ip.To4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
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
	ones, bits := p.subnet.Mask.Size()
	hostBits := bits - ones
	if hostBits < 2 {
		return nil, fmt.Errorf("subnet %s too small for DHCP", p.subnet)
	}
	total := uint32(1) << uint32(hostBits)

	startOff := uint32(1)
	endOff := total - 2 // last host
	if p.rangeStart != nil {
		startOff = ipToUint(p.rangeStart) - ipToUint(base)
	}
	if p.rangeEnd != nil {
		endOff = ipToUint(p.rangeEnd) - ipToUint(base)
	}

	for i := startOff; i <= endOff; i++ {
		cand := makeIP(base, i)
		if cand.Equal(p.gw) {
			continue
		}
		s := cand.String()
		if p.used[s] {
			continue
		}
		p.used[s] = true
		p.byMAC[key] = cand
		return cand, nil
	}
	return nil, fmt.Errorf("pool exhausted for subnet %s", p.subnet)
}

// makeIP returns base + offset as a 4-byte IPv4 address.
func makeIP(base net.IP, offset uint32) net.IP {
	b := base.To4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += offset
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).To4()
}
