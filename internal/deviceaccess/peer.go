package deviceaccess

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// The address of the peer of an owner line comes from the node-agent over cable.sock, which is the one input of the service a compromised
// node-agent controls. It is checked strictly when the line is taken and again before the dial, and the URL is built from the parsed
// numbers and the service's own cable port, never from the string it came in: the service can dial a pod IP on the cable port and nothing
// else (no host name, no loopback, link-local, unspecified or multicast address, no zone, no other port, no path).

var errPeer = errors.New("the peer is not a pod IP on the cable port")

// peerAddr parses and checks the peer of a line: a unicast IP literal with the cable port.
func (s *Service) peerAddr(peer string) (netip.AddrPort, error) {
	ap, err := netip.ParseAddrPort(peer)
	// only the canonical form (no leading zeros, no other spelling of the same address) and the cable port
	if err != nil || ap.String() != peer || s.cablePort <= 0 || int(ap.Port()) != s.cablePort {
		return netip.AddrPort{}, errPeer
	}
	a := ap.Addr().Unmap()
	if !a.IsValid() || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() {
		return netip.AddrPort{}, errPeer
	}
	if a.IsLoopback() {
		if !s.allowLoopbackPeers {
			return netip.AddrPort{}, errPeer
		}
	} else if !a.IsGlobalUnicast() {
		return netip.AddrPort{}, errPeer
	}
	return netip.AddrPortFrom(a, ap.Port()), nil
}

// cableURL is the URL of the cable stream of a checked peer: the host is written from the bytes of the address and the port is the
// service's own, so nothing of the string the node-agent sent reaches the dial.
func (s *Service) cableURL(ap netip.AddrPort) string {
	a := ap.Addr()
	var host string
	if a.Is4() {
		b := a.As4()
		host = fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
	} else {
		b := a.As16()
		groups := make([]string, 8)
		for i := range groups {
			groups[i] = strconv.FormatUint(uint64(b[2*i])<<8|uint64(b[2*i+1]), 16)
		}
		host = strings.Join(groups, ":")
	}
	u := url.URL{Scheme: "ws", Host: net.JoinHostPort(host, strconv.Itoa(s.cablePort)), Path: wire.CablePath}
	return u.String()
}
