//go:build linux

package main

import (
	"context"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// waitForInterface blocks until the named interface appears (or ctx is cancelled).
func waitForInterface(ctx context.Context, name string) (netlink.Link, error) {
	if link, err := netlink.LinkByName(name); err == nil {
		return link, nil
	}
	ch := make(chan netlink.LinkUpdate)
	done := make(chan struct{})
	defer close(done)
	if err := netlink.LinkSubscribe(ch, done); err != nil {
		return nil, fmt.Errorf("link subscribe: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case u := <-ch:
			if u.Link.Attrs().Name == name {
				return u.Link, nil
			}
		}
	}
}

// firstHostCIDR returns the first host address of subnet as "ip/prefix"
// (e.g. 10.8.0.0/24 → 10.8.0.1/24).
func firstHostCIDR(subnet *net.IPNet) string {
	gw := nextIP(subnet.IP)
	return (&net.IPNet{IP: gw, Mask: subnet.Mask}).String()
}

// assignIfaceIP assigns the given CIDR (e.g. 10.8.0.1/24) directly to the named
// interface and brings it up. Used for wg0 client-subnet gateway address.
func assignIfaceIP(ifaceName, cidr string) error {
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

// assignGatewayIP assigns the first host IP in labCIDR (e.g. 10.8.N.1/24) to the
// link and brings it up. Idempotent — EEXIST is not an error.
func assignGatewayIP(link netlink.Link, labCIDR string) error {
	ip, ipNet, err := net.ParseCIDR(labCIDR)
	if err != nil {
		return fmt.Errorf("parse labCIDR %q: %w", labCIDR, err)
	}
	gwIP := nextIP(ip)
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: gwIP, Mask: ipNet.Mask}}
	if err := netlink.AddrAdd(link, addr); err != nil && err.Error() != "file exists" {
		return fmt.Errorf("addr add %s: %w", addr, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("link set up: %w", err)
	}
	return nil
}

// nextIP returns ip+1.
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

// addLabRoute adds a route so that labCIDR traffic goes via the given link.
// EEXIST is tolerated (route may already be auto-added by the kernel after
// AddrAdd on a /24).
func addLabRoute(link netlink.Link, labCIDR string) error {
	_, dst, err := net.ParseCIDR(labCIDR)
	if err != nil {
		return fmt.Errorf("parse labCIDR %q: %w", labCIDR, err)
	}
	if err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       dst,
	}); err != nil && err.Error() != "file exists" {
		return err
	}
	return nil
}

// delLabRoute removes the route to labCIDR.
func delLabRoute(labCIDR string) error {
	_, dst, err := net.ParseCIDR(labCIDR)
	if err != nil {
		return fmt.Errorf("parse labCIDR %q: %w", labCIDR, err)
	}
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("route list: %w", err)
	}
	for _, r := range routes {
		if r.Dst != nil && r.Dst.String() == dst.String() {
			if err := netlink.RouteDel(&r); err != nil {
				return fmt.Errorf("route del: %w", err)
			}
		}
	}
	return nil
}
