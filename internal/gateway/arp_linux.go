//go:build linux

package gateway

import (
	"encoding/binary"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// announceARP tells the segment of a lab interface who owns ip now: a gratuitous ARP request and a gratuitous reply, from the interface's
// own hardware address to the broadcast address. A device that holds an ARP entry for the gateway takes the new address at once instead
// of sending to a dead one until the entry expires. (The addresses of the legs are fixed by the node-agent, so this is the backstop.)
func announceARP(iface string, ip net.IP) error {
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("%v is not an IPv4 address", ip)
	}
	link, err := net.InterfaceByName(iface)
	if err != nil {
		return err
	}
	if len(link.HardwareAddr) != 6 {
		return fmt.Errorf("%s has no ethernet address", iface)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(arpProto))
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	addr := &unix.SockaddrLinklayer{Protocol: arpProto, Ifindex: link.Index, Halen: 6}
	copy(addr.Addr[:], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	for _, op := range []uint16{1, 2} { // request, reply
		if err := unix.Sendto(fd, arpFrame(link.HardwareAddr, ip4, op), 0, addr); err != nil {
			return fmt.Errorf("announce %v on %s: %w", ip4, iface, err)
		}
	}
	return nil
}

// arpProto is the Ethernet type of ARP in network byte order, as an AF_PACKET socket wants it.
const arpProto = uint16(unix.ETH_P_ARP&0xff)<<8 | uint16(unix.ETH_P_ARP>>8)

// arpFrame is an Ethernet frame with a gratuitous ARP of the given operation: sender and target protocol address are both ip.
func arpFrame(mac net.HardwareAddr, ip net.IP, op uint16) []byte {
	f := make([]byte, 14+28)
	copy(f[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(f[6:12], mac)
	binary.BigEndian.PutUint16(f[12:14], unix.ETH_P_ARP)
	p := f[14:]
	binary.BigEndian.PutUint16(p[0:2], 1)      // Ethernet
	binary.BigEndian.PutUint16(p[2:4], 0x0800) // IPv4
	p[4], p[5] = 6, 4
	binary.BigEndian.PutUint16(p[6:8], op)
	copy(p[8:14], mac)
	copy(p[14:18], ip)
	if op == 2 {
		copy(p[18:24], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	}
	copy(p[24:28], ip)
	return f
}
