//go:build linux

package xdp

import (
	"encoding/binary"
	"fmt"
	"net"

	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
	ctrl "sigs.k8s.io/controller-runtime"
)

// XDPHandle manages the attached XDP program and BPF map lifecycle.
type XDPHandle struct {
	objs    WgDemuxObjects
	xdpLink link.Link
}

// Load attaches the XDP program to iface, populates xdp_cfg_map, and returns a handle.
// Returns error if BPF is unsupported, capabilities are missing, or iface not found.
// Callers should treat error as non-fatal and fall back to userspace.
func Load(iface string, proxyPort uint16) (*XDPHandle, error) {
	objs := WgDemuxObjects{}
	if err := LoadWgDemuxObjects(&objs, nil); err != nil {
		return nil, fmt.Errorf("load BPF objects: %w", err)
	}

	nl, err := netlink.LinkByName(iface)
	if err != nil {
		objs.Close()
		return nil, fmt.Errorf("netlink: link %q: %w", iface, err)
	}

	proxyIP, err := ifaceIPv4(nl)
	if err != nil {
		objs.Close()
		return nil, fmt.Errorf("get proxy IP on %q: %w", iface, err)
	}

	gwMAC, err := gatewayMAC(nl)
	if err != nil {
		objs.Close()
		return nil, fmt.Errorf("gateway MAC on %q: %w", iface, err)
	}

	cfg := WgDemuxXdpCfg{
		ProxyIp:   binary.BigEndian.Uint32(proxyIP.To4()),
		ProxyPort: htons(proxyPort),
	}
	copy(cfg.GwMac[:], gwMAC)

	cfgKey := uint32(0)
	if err := objs.XdpCfgMap.Put(cfgKey, cfg); err != nil {
		objs.Close()
		return nil, fmt.Errorf("populate xdp_cfg_map: %w", err)
	}

	// Try native (driver) mode first; fall back to generic (SKB) mode.
	l, err := link.AttachXDP(link.XDPOptions{
		Program:   objs.WgDemux,
		Interface: nl.Attrs().Index,
		Flags:     link.XDPDriverMode,
	})
	if err != nil {
		l, err = link.AttachXDP(link.XDPOptions{
			Program:   objs.WgDemux,
			Interface: nl.Attrs().Index,
			Flags:     link.XDPGenericMode,
		})
		if err != nil {
			objs.Close()
			return nil, fmt.Errorf("attach XDP to %q: %w", iface, err)
		}
		ctrl.Log.WithName("xdp").Info("XDP attached in generic (SKB) mode", "iface", iface)
	} else {
		ctrl.Log.WithName("xdp").Info("XDP attached in native (driver) mode", "iface", iface)
	}

	return &XDPHandle{objs: objs, xdpLink: l}, nil
}

// Update inserts or updates a forwarding entry for receiverIndex.
func (h *XDPHandle) Update(receiverIndex uint32, ip net.IP, port uint16) error {
	v := WgDemuxDstEntry{
		Ip:   binary.BigEndian.Uint32(ip.To4()),
		Port: htons(port),
	}
	return h.objs.WgSessions.Put(receiverIndex, v)
}

// Delete removes the forwarding entry for receiverIndex.
func (h *XDPHandle) Delete(receiverIndex uint32) error {
	return h.objs.WgSessions.Delete(receiverIndex)
}

// Close detaches the XDP program and releases all BPF resources.
func (h *XDPHandle) Close() error {
	h.xdpLink.Close()
	return h.objs.Close()
}

func htons(v uint16) uint16 { return (v >> 8) | (v << 8) }

// ifaceIPv4 returns the first IPv4 address on the link.
func ifaceIPv4(l netlink.Link) (net.IP, error) {
	addrs, err := netlink.AddrList(l, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if ip := a.IP.To4(); ip != nil {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("no IPv4 address on interface")
}

// gatewayMAC finds the MAC of the default route next-hop via the ARP/neighbour table.
func gatewayMAC(l netlink.Link) (net.HardwareAddr, error) {
	routes, err := netlink.RouteList(l, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}
	var gwIP net.IP
	for _, r := range routes {
		if r.Dst == nil && r.Gw != nil {
			gwIP = r.Gw
			break
		}
	}
	if gwIP == nil {
		return nil, fmt.Errorf("no default route with gateway found")
	}

	neighs, err := netlink.NeighList(l.Attrs().Index, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("list neighbours: %w", err)
	}
	for _, n := range neighs {
		if n.IP.Equal(gwIP) && len(n.HardwareAddr) > 0 {
			return n.HardwareAddr, nil
		}
	}
	return nil, fmt.Errorf("ARP entry for gateway %s not found", gwIP)
}
