//go:build linux

package netutil

import (
	"fmt"
	"net"
	
	"github.com/vishvananda/netlink"
)

// NextIP returns ip+1 (e.g. 10.9.5.0 → 10.9.5.1).
func NextIP(ip net.IP) net.IP {
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

// FirstHostCIDR returns the first host address of subnet as "ip/prefix"
// (e.g. 10.8.0.0/24 → 10.8.0.1/24).
func FirstHostCIDR(subnet *net.IPNet) string {
	gw := NextIP(subnet.IP)
	return (&net.IPNet{IP: gw, Mask: subnet.Mask}).String()
}

// AssignIfaceIP assigns cidr (e.g. "10.8.0.1/24") to the named interface
// and brings it up. Idempotent — EEXIST is not an error.
func AssignIfaceIP(ifaceName, cidr string) error {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("link %s: %w", ifaceName, err)
	}
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return fmt.Errorf("parse addr %q: %w", cidr, err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil && err.Error() != "file exists" {
		return fmt.Errorf("addr add %s on %s: %w", cidr, ifaceName, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("link set up %s: %w", ifaceName, err)
	}
	return nil
}

// AssignFirstHostIP assigns the first host IP of cidr to the named interface.
// e.g. cidr="10.9.5.0/24" → assigns 10.9.5.1/24 to ifaceName.
func AssignFirstHostIP(ifaceName, cidr string) error {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("parse CIDR %q: %w", cidr, err)
	}
	gwIP := NextIP(ip)
	return AssignIfaceIP(ifaceName, (&net.IPNet{IP: gwIP, Mask: ipNet.Mask}).String())
}

// AssignFirstHostIPToLink assigns the first host IP of cidr to an already-resolved link.
// Use when the caller already holds a netlink.Link (avoids a second LinkByName lookup).
func AssignFirstHostIPToLink(link netlink.Link, cidr string) error {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("parse CIDR %q: %w", cidr, err)
	}
	gwIP := NextIP(ip)
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: gwIP, Mask: ipNet.Mask}}
	if err := netlink.AddrAdd(link, addr); err != nil && err.Error() != "file exists" {
		return fmt.Errorf("addr add %s on %s: %w", addr, link.Attrs().Name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("link set up %s: %w", link.Attrs().Name, err)
	}
	return nil
}
