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
	Ranges  []Range
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
		if dns == nil || dns.To4() == nil {
			return fmt.Errorf("invalid DHCP DNS address %q", cfg.DNS)
		}
	}
	if err := ValidateRanges(cfg.Ranges); err != nil {
		return err
	}

	pool := newIPPool(subnet, gw, cfg.Ranges)

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
