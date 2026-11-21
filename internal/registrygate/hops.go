package registrygate

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
)

// A redirect hop is a request to a host a redirect named, not one the operator configured. Its context carries hopKey, and the dialer of
// the upstream transport (newUpstreamTransport) refuses internal addresses for such a request.
type hopKey struct{}

func hopContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, hopKey{}, struct{}{})
}

func isHop(ctx context.Context) bool { return ctx.Value(hopKey{}) != nil }

// checkFillRedirect says whether the fill path may follow a redirect to loc (already resolved against the request): https, no userinfo,
// the default port (none, or 443), and a host that is a DNS name. A host is not a DNS name when it is an IP address in any form (dotted,
// decimal, hex, octal or short numbers, IPv6, with a zone), has no dot ("localhost", "metadata"), ends in a dot, or has an empty or
// unusual label. There is no list of allowed hosts: blob CDNs vary. What the name resolves to is judged at dial time (refuseInternalAddress).
func checkFillRedirect(loc *url.URL) error {
	if loc.Scheme != "https" {
		return errors.New("not https")
	}
	if loc.User != nil {
		return errors.New("userinfo in the address")
	}
	host := loc.Hostname()
	if p := loc.Port(); p != "" && p != "443" {
		return errors.New("not the default port")
	}
	if loc.Port() == "" && strings.HasSuffix(loc.Host, ":") {
		return errors.New("an empty port")
	}
	if !publicDNSName(host) {
		return errors.New("the host is not a DNS name")
	}
	return nil
}

// publicDNSName: letters, digits and hyphens in non-empty labels, at least two labels, no trailing dot, and not a number in any of the
// forms an address is written in (inet_aton: one to four parts, each decimal, octal or hex).
func publicDNSName(host string) bool {
	host = strings.ToLower(host)
	if !plainHostName(host) || !strings.Contains(host, ".") {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) <= 4 {
		numeric := true
		for _, l := range labels {
			if _, err := strconv.ParseUint(l, 0, 32); err != nil {
				numeric = false
				break
			}
		}
		if numeric {
			return false
		}
	}
	// a last label that starts with a digit is a number written some way ("1.2.3.4x" is no DNS name either, no top-level domain has one)
	if l := labels[len(labels)-1]; l[0] >= '0' && l[0] <= '9' {
		return false
	}
	return true
}

// refuseInternalAddress is the Control hook of the dialer of a hop: it runs for each address a connection is about to be made to, after
// the name was resolved, so the address that is refused is the one that would be used.
func refuseInternalAddress(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("refusing to dial %q: not an address", address)
	}
	if internalAddr(ap.Addr()) {
		return fmt.Errorf("refusing to dial %s: an internal address", ap.Addr())
	}
	return nil
}

var internalPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",     // "this network"
		"100.64.0.0/10", // CGNAT
		"192.0.0.0/24",  // IETF protocol assignments
		"198.18.0.0/15", // benchmarking
		"240.0.0.0/4",   // reserved, and the broadcast address
		"64:ff9b::/96",  // NAT64: the embedded IPv4 address is checked as well
		"2002::/16",     // 6to4: likewise
		"::/96",         // IPv4-compatible: likewise
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// internalAddr is true for loopback, private (RFC 1918, fc00::/7), link-local (169.254/16 with the metadata address, fe80::/10), CGNAT,
// unspecified, multicast and the reserved ranges above, and for an IPv6 form that embeds one of them (IPv4-mapped, NAT64, 6to4).
func internalAddr(a netip.Addr) bool {
	a = a.Unmap()
	a = a.WithZone("")
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range internalPrefixes {
		if p.Contains(a) {
			if a.Is4() {
				return true
			}
			b := a.As16()
			switch {
			case p.Bits() == 96: // NAT64, IPv4-compatible: the last four bytes
				return internalAddr(netip.AddrFrom4([4]byte(b[12:16])))
			case p.Bits() == 16: // 6to4: bytes 2 to 5
				return internalAddr(netip.AddrFrom4([4]byte(b[2:6])))
			}
		}
	}
	return false
}
