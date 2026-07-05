//go:build linux

package dhcp

import (
	"context"
	"fmt"
	"net"
	"sync"
	
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
)

// Manager runs per-lab DHCP servers keyed by an arbitrary name (lab name).
type Manager struct {
	mu      sync.Mutex
	servers map[string]*serverEntry
}

type serverEntry struct {
	cancel context.CancelFunc
}

func NewManager() *Manager {
	return &Manager{servers: make(map[string]*serverEntry)}
}

// Config describes a DHCP server for one network interface.
// Subnet and Gateway are derived from the lab's allocated CIDR by the caller.
// DNS is optional — omit to suppress the DNS option in responses.
// BindIP is used as the DHCP Server Identifier (option 54) in replies; the
// socket itself always binds 0.0.0.0:67 — binding the unicast address would
// stop broadcast DISCOVERs (dst 255.255.255.255) from ever reaching the
// server. Per-lab isolation on a shared pod comes from SO_BINDTODEVICE
// (server4 sets it from Iface) plus SO_REUSEADDR.
type Config struct {
	Iface   string
	Subnet  string
	Gateway string
	DNS     string
	BindIP  string
}

func (m *Manager) Start(name string, cfg Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[name]; ok {
		return nil
	}
	
	_, subnet, err := net.ParseCIDR(cfg.Subnet)
	if err != nil {
		return fmt.Errorf("parse subnet %q: %w", cfg.Subnet, err)
	}
	gw := net.ParseIP(cfg.Gateway)
	if gw == nil {
		return fmt.Errorf("invalid gateway %q", cfg.Gateway)
	}
	var dns net.IP
	if cfg.DNS != "" {
		dns = net.ParseIP(cfg.DNS)
	}
	
	pool := newIPPool(subnet, gw)
	
	serverID := gw
	if cfg.BindIP != "" {
		if ip := net.ParseIP(cfg.BindIP); ip != nil {
			serverID = ip
		}
	}
	
	baseOpts := func(assigned net.IP) []dhcpv4.Modifier {
		opts := []dhcpv4.Modifier{
			dhcpv4.WithYourIP(assigned),
			dhcpv4.WithNetmask(subnet.Mask),
			dhcpv4.WithRouter(gw),
			dhcpv4.WithLeaseTime(86400),
			dhcpv4.WithServerIP(serverID),
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(serverID)),
		}
		if dns != nil {
			opts = append(opts, dhcpv4.WithDNS(dns))
		}
		return opts
	}
	
	handler := func(conn net.PacketConn, peer net.Addr, msg *dhcpv4.DHCPv4) {
		assigned, err := pool.Allocate(msg.ClientHWAddr)
		if err != nil {
			return
		}
		var reply *dhcpv4.DHCPv4
		switch msg.MessageType() {
		case dhcpv4.MessageTypeDiscover:
			opts := append([]dhcpv4.Modifier{dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer)}, baseOpts(assigned)...)
			reply, _ = dhcpv4.NewReplyFromRequest(msg, opts...)
		case dhcpv4.MessageTypeRequest:
			opts := append([]dhcpv4.Modifier{dhcpv4.WithMessageType(dhcpv4.MessageTypeAck)}, baseOpts(assigned)...)
			reply, _ = dhcpv4.NewReplyFromRequest(msg, opts...)
		}
		if reply != nil {
			_, _ = conn.WriteTo(reply.ToBytes(), peer)
		}
	}
	
	laddr := &net.UDPAddr{Port: 67, IP: net.IPv4zero}
	srv, err := server4.NewServer(cfg.Iface, laddr, handler)
	if err != nil {
		return fmt.Errorf("new DHCP server on %s: %w", cfg.Iface, err)
	}
	
	ctx, cancel := context.WithCancel(context.Background())
	m.servers[name] = &serverEntry{cancel: cancel}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	go srv.Serve()
	return nil
}

func (m *Manager) Stop(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.servers[name]; ok {
		e.cancel()
		delete(m.servers, name)
	}
}

// ipPool allocates IPv4 addresses from the full host range of subnet.
// Gateway is always skipped. Allocation is sticky per MAC (in-memory only).
type ipPool struct {
	mu     sync.Mutex
	subnet *net.IPNet
	gw     net.IP
	byMAC  map[string]net.IP
	used   map[string]bool
}

func newIPPool(subnet *net.IPNet, gw net.IP) *ipPool {
	return &ipPool{
		subnet: subnet,
		gw:     gw.To4(),
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
	ones, bits := p.subnet.Mask.Size()
	hostBits := bits - ones
	if hostBits < 2 {
		return nil, fmt.Errorf("subnet %s too small for DHCP", p.subnet)
	}
	total := uint32(1) << uint32(hostBits)
	
	for i := uint32(1); i <= total-2; i++ {
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

func makeIP(base net.IP, offset uint32) net.IP {
	b := base.To4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += offset
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).To4()
}
