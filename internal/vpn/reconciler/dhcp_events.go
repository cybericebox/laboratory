//go:build linux

package reconciler

import (
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/labdhcp"
	"github.com/cybericebox/laboratory/pkg/netutil"
	"github.com/vishvananda/netlink"
)

type appliedNetwork struct {
	Iface, CIDR, Hardware string
	LinkIndex             int
	DHCP                  bool
	DHCPKnown             bool
}

func vpnDHCPInputsChanged(old, next *lab.Lab) bool { return labdhcp.InputsChanged(old, next, "vpn") }
func networkPresent(link netlink.Link, cidr string) bool {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if addr.IPNet != nil && addr.IPNet.String() == firstHostIP(cidr)+"/24" {
			return true
		}
	}
	return false
}
func removeNetworkAddress(state appliedNetwork) { netutil.RemoveFirstHostIP(state.Iface, state.CIDR) }
