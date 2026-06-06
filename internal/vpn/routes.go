//go:build linux

package vpn

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// AddLabRoute adds a route for labCIDR via the given link.
func AddLabRoute(link netlink.Link, labCIDR string) error {
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

// DelLabRoute removes the route to labCIDR.
func DelLabRoute(labCIDR string) error {
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
