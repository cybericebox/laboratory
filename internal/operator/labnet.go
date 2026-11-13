package operator

import (
	"fmt"
	"net/netip"
)

// DefaultLabBaseNetwork is the one address space of the lab networks: /24 number 0 is the VPN clients, every other /24 goes to a
// gateway of a lab, VPN or Internet, first free number.
const DefaultLabBaseNetwork = "10.128.0.0/9"

// ResolveLabBaseNetwork gives the one lab base network. labBase is LAB_BASE_NETWORK. vpnBase and inetBase are the two settings it
// replaces (VPN_BASE_NETWORK, INET_BASE_NETWORK): read only while LAB_BASE_NETWORK is unset, and only when together they are the
// two halves of one network with the VPN half first (so /24 number 0, the clients, stays where it was); anything else stops the start.
func ResolveLabBaseNetwork(labBase, vpnBase, inetBase string) (string, error) {
	if labBase != "" {
		p, err := netip.ParsePrefix(labBase)
		if err != nil || !p.Addr().Is4() || p.Bits() < 8 || p.Bits() > 18 {
			return "", fmt.Errorf("LAB_BASE_NETWORK %q is not an IPv4 network between /8 and /18", labBase)
		}
		return labBase, nil
	}
	if vpnBase == "" && inetBase == "" {
		return DefaultLabBaseNetwork, nil
	}
	vpn, errVPN := netip.ParsePrefix(vpnBase)
	inet, errInet := netip.ParsePrefix(inetBase)
	if errVPN != nil || errInet != nil || vpn.Bits() != inet.Bits() || vpn.Bits() < 9 || !vpn.Addr().Is4() || !inet.Addr().Is4() {
		return "", fmt.Errorf("VPN_BASE_NETWORK %q and INET_BASE_NETWORK %q are replaced by LAB_BASE_NETWORK: set it, or set both old values as the two halves of one network", vpnBase, inetBase)
	}
	parent, _ := vpn.Addr().Prefix(vpn.Bits() - 1)
	lower := netip.PrefixFrom(parent.Addr(), vpn.Bits())
	size := uint32(1) << uint(32-vpn.Bits())
	a := parent.Addr().As4()
	upper := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	upper += size
	upperAddr := netip.AddrFrom4([4]byte{byte(upper >> 24), byte(upper >> 16), byte(upper >> 8), byte(upper)})
	if vpn != lower || inet != netip.PrefixFrom(upperAddr, inet.Bits()) {
		return "", fmt.Errorf("VPN_BASE_NETWORK %q and INET_BASE_NETWORK %q are not the lower and upper half of one network: set LAB_BASE_NETWORK", vpnBase, inetBase)
	}
	return parent.String(), nil
}
