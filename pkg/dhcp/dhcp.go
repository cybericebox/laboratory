//go:build linux

package dhcp

import (
	"log"
	"net"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
)

func nativeServer(cfg Config, handler server4.Handler) (serverRunner, error) {
	return server4.NewServer(cfg.Iface, &net.UDPAddr{Port: 67, IP: net.IPv4zero}, handler)
}
func (e *serverEntry) handle(conn net.PacketConn, peer net.Addr, msg *dhcpv4.DHCPv4) {
	e.handlerMu.Lock()
	if e.stopped {
		e.handlerMu.Unlock()
		return
	}
	e.handlers.Add(1)
	e.handlerMu.Unlock()
	defer e.handlers.Done()
	e.mu.Lock()
	cfg := e.cfg
	reply, err := replyForMessage(cfg, e.pool, msg)
	e.mu.Unlock()
	if err != nil {
		log.Printf("DHCP %s: %v", cfg.Iface, err)
		return
	}
	if reply == nil {
		return
	}
	if msg.IsBroadcast() || reply.MessageType() == dhcpv4.MessageTypeNak {
		peer = &net.UDPAddr{IP: net.IPv4bcast, Port: 68}
	}
	if _, err := conn.WriteTo(reply.ToBytes(), peer); err != nil {
		log.Printf("DHCP write %s: %v", cfg.Iface, err)
	}
}
