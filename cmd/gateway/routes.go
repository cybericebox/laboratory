//go:build linux

package main

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// nextIP returns ip+1 (e.g. 10.9.5.0 → 10.9.5.1).
func nextIP(ip net.IP) net.IP {
	ip4 := ip.To4()
	result := make(net.IP, 4)
	copy(result, ip4)
	for i := 3; i >= 0; i-- {
		result[i]++
		if result[i] != 0 {
			break
		}
	}
	return result
}

// assignGatewayIP assigns the first host IP in the CIDR to the named interface.
// Returns nil if already assigned (addr exists) or if newly assigned.
func assignGatewayIP(ifaceName string, cidr string) error {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("link %s not found: %w", ifaceName, err)
	}
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("parse CIDR %q: %w", cidr, err)
	}
	gwIP := nextIP(ip)
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: gwIP, Mask: ipNet.Mask}}
	if err := netlink.AddrAdd(link, addr); err != nil {
		// EEXIST means address already set — not an error.
		if err.Error() != "file exists" {
			return fmt.Errorf("addr add: %w", err)
		}
	}
	return nil
}
