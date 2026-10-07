//go:build linux

package dhcp

import (
	"context"
	"fmt"
	"log"
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
	if err := ValidateRanges(cfg.Ranges); err != nil {
		return err
	}

	pool := newIPPool(subnet, gw, cfg.Ranges)

	handler := func(conn net.PacketConn, peer net.Addr, msg *dhcpv4.DHCPv4) {
		reply, err := replyForMessage(cfg, pool, msg)
		if err != nil {
			log.Printf("DHCP %s: %v", cfg.Iface, err)
			return
		}
		if reply != nil {
			if msg.IsBroadcast() || reply.MessageType() == dhcpv4.MessageTypeNak {
				peer = &net.UDPAddr{IP: net.IPv4bcast, Port: 68}
			}
			if _, err := conn.WriteTo(reply.ToBytes(), peer); err != nil {
				log.Printf("DHCP write %s: %v", cfg.Iface, err)
			}
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
		_ = srv.Close()
	}()
	go func() { _ = srv.Serve() }()
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
