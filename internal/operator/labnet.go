package operator

import (
	"fmt"
	"net/netip"
)

// DefaultLabBaseNetwork is the one address space of the lab networks: /24 number 0 is the VPN clients, every other /24 goes to a
// gateway of a lab, VPN or Internet, first free number.
const DefaultLabBaseNetwork = "10.128.0.0/9"

// ResolveLabBaseNetwork gives the lab base network: LAB_BASE_NETWORK, or the default when it is unset. It must be an IPv4 network from /8
// to /18 (at most 65535 gateway networks after the clients', at least 63).
func ResolveLabBaseNetwork(labBase string) (string, error) {
	if labBase == "" {
		return DefaultLabBaseNetwork, nil
	}
	p, err := netip.ParsePrefix(labBase)
	if err != nil || !p.Addr().Is4() || p.Bits() < 8 || p.Bits() > 18 {
		return "", fmt.Errorf("LAB_BASE_NETWORK %q is not an IPv4 network between /8 and /18", labBase)
	}
	return labBase, nil
}
