//go:build linux

// netconfig is the device init-container helper. It configures each declared
// interface inside the pod netns: static interfaces get their fixed IP/routes;
// dhcp-preset interfaces lease a dynamic address from the lab DHCP server (for
// images that carry no DHCP client of their own). Interfaces the image handles
// itself (addr.type=dhcp) are not listed here — the operator gives those pods
// NET_ADMIN+NET_RAW instead.
//
// Config is a JSON array passed in the NETCONFIG env var so no user-controlled
// value is ever interpolated into a shell.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/vishvananda/netlink"
)

// ifaceConfig mirrors the operator-rendered per-interface spec.
type ifaceConfig struct {
	Name    string  `json:"name"`
	Mode    string  `json:"mode"` // "static" | "dhcp-preset"
	IP      string  `json:"ip,omitempty"`
	Gateway string  `json:"gateway,omitempty"`
	Routes  []route `json:"routes,omitempty"`
}

type route struct {
	Dst string `json:"dst"`
	Via string `json:"via"`
}

func main() {
	raw := os.Getenv("NETCONFIG")
	if raw == "" {
		return // nothing to do
	}
	var cfgs []ifaceConfig
	if err := json.Unmarshal([]byte(raw), &cfgs); err != nil {
		fatal("parse NETCONFIG: %v", err)
	}
	for _, c := range cfgs {
		if err := configure(c); err != nil {
			fatal("configure %s: %v", c.Name, err)
		}
	}
}

func configure(c ifaceConfig) error {
	link, err := waitLink(c.Name, 5*time.Second)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("link up: %w", err)
	}
	switch c.Mode {
	case "static":
		return applyStatic(link, c)
	case "dhcp-preset":
		return applyDHCPPreset(link, c)
	default:
		return fmt.Errorf("unknown mode %q", c.Mode)
	}
}

func applyStatic(link netlink.Link, c ifaceConfig) error {
	if err := addAddr(link, c.IP); err != nil {
		return err
	}
	if c.Gateway != "" {
		if err := addDefaultRoute(link, c.Gateway); err != nil {
			return err
		}
	}
	for _, r := range c.Routes {
		if err := addRoute(link, r.Dst, r.Via); err != nil {
			return err
		}
	}
	return nil
}

// applyDHCPPreset runs DHCP DORA on the interface and applies the offered address,
// netmask, and default gateway — for images that have no DHCP client of their own.
func applyDHCPPreset(link netlink.Link, c ifaceConfig) error {
	cli, err := nclient4.New(c.Name)
	if err != nil {
		return fmt.Errorf("dhcp client: %w", err)
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lease, err := cli.Request(ctx)
	if err != nil {
		return fmt.Errorf("dhcp request: %w", err)
	}
	ack := lease.ACK
	mask := ack.SubnetMask()
	if mask == nil {
		mask = net.CIDRMask(24, 32)
	}
	ones, _ := mask.Size()
	if err := addAddr(link, fmt.Sprintf("%s/%d", ack.YourIPAddr.String(), ones)); err != nil {
		return err
	}
	for _, gw := range ack.Router() {
		if err := addDefaultRoute(link, gw.String()); err != nil {
			return err
		}
		break
	}
	return nil
}

func addAddr(link netlink.Link, cidr string) error {
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return fmt.Errorf("parse addr %q: %w", cidr, err)
	}
	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("add addr %q: %w", cidr, err)
	}
	return nil
}

func addDefaultRoute(link netlink.Link, gw string) error {
	g := net.ParseIP(gw)
	if g == nil {
		return fmt.Errorf("bad gateway %q", gw)
	}
	return netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: g})
}

func addRoute(link netlink.Link, dst, via string) error {
	_, dstNet, err := net.ParseCIDR(dst)
	if err != nil {
		return fmt.Errorf("bad route dst %q: %w", dst, err)
	}
	g := net.ParseIP(via)
	if g == nil {
		return fmt.Errorf("bad route via %q", via)
	}
	return netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: dstNet, Gw: g})
}

func waitLink(name string, timeout time.Duration) (netlink.Link, error) {
	deadline := time.Now().Add(timeout)
	for {
		if l, err := netlink.LinkByName(name); err == nil {
			return l, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("interface %q did not appear within %s", name, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func fatal(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "netconfig: "+format+"\n", a...)
	os.Exit(1)
}
