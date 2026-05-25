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

// addLabRoute adds a route so that labCIDR traffic goes via the given link.
func addLabRoute(link netlink.Link, labCIDR string) error {
	_, dst, err := net.ParseCIDR(labCIDR)
	if err != nil {
		return fmt.Errorf("parse labCIDR %q: %w", labCIDR, err)
	}
	return netlink.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       dst,
	})
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
