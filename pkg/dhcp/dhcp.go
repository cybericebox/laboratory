//go:build linux

package dhcp

import (
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"log"
	"net"
)

func nativeServer(cfg Config, handler server4.Handler) (serverRunner, error) {
	return server4.NewServer(cfg.Iface, &net.UDPAddr{Port: 67, IP: net.IPv4zero}, handler)
}
func (e *serverEntry) handle(conn net.PacketConn, peer net.Addr, msg *dhcpv4.DHCPv4) {
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
