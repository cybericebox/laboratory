//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
)

type DHCPManager struct {
	mu      sync.Mutex
	servers map[string]context.CancelFunc
}

func newDHCPManager() *DHCPManager {
	return &DHCPManager{servers: make(map[string]context.CancelFunc)}
}

type DHCPConfig struct {
	Iface   string
	Subnet  string
	Gateway string
	DNS     string
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

	handler := func(conn net.PacketConn, peer net.Addr, msg *dhcpv4.DHCPv4) {
		var reply *dhcpv4.DHCPv4
		switch msg.MessageType() {
		case dhcpv4.MessageTypeDiscover:
			reply, _ = dhcpv4.NewReplyFromRequest(msg,
				dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer),
				dhcpv4.WithNetmask(subnet.Mask),
				dhcpv4.WithRouter(gw),
				dhcpv4.WithDNS(dns),
				dhcpv4.WithLeaseTime(86400), // 24 hours in seconds
			)
		case dhcpv4.MessageTypeRequest:
			reply, _ = dhcpv4.NewReplyFromRequest(msg,
				dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
				dhcpv4.WithNetmask(subnet.Mask),
				dhcpv4.WithRouter(gw),
				dhcpv4.WithDNS(dns),
				dhcpv4.WithLeaseTime(86400), // 24 hours in seconds
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
	m.servers[labName] = cancel
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
	if cancel, ok := m.servers[labName]; ok {
		cancel()
		delete(m.servers, labName)
	}
}
