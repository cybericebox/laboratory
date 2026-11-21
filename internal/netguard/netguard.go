// Package netguard keeps a client from being led to an internal address by a redirect: the check of a redirect target, the dialer that
// refuses internal addresses when it connects (after the name is resolved, so DNS rebinding cannot pass), and a RoundTripper for
// clients that follow redirects themselves.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrInternalAddress: a connection to an internal address was refused.
var ErrInternalAddress = errors.New("a connection to an internal address was refused")

// A redirect hop is a request to a host a redirect named, not one the operator configured. Its context carries hopKey, and the dialer of
// NewTransport refuses internal addresses for such a request.
type hopKey struct{}

// HopContext marks ctx as the context of a redirect hop.
func HopContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, hopKey{}, struct{}{})
}

// IsHop says whether ctx belongs to a redirect hop.
func IsHop(ctx context.Context) bool { return ctx.Value(hopKey{}) != nil }

// CheckRedirect says whether a redirect to loc (already resolved against the request) may be followed, on the fill path of the registry
// gate and in the tag resolver of the operator: https, no userinfo, the default port (none, or 443), and a host that is a DNS name. A host
// is not a DNS name when it is an IP address in any form (dotted, decimal, hex, octal or short numbers, IPv6, with a zone), has no dot
// ("localhost", "metadata"), ends in a dot, or has an empty or unusual label. There is no list of allowed hosts: blob CDNs vary. What the
// name resolves to is judged at dial time (RefuseInternalAddress).
func CheckRedirect(loc *url.URL) error {
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
	if !PublicDNSName(host) {
		return errors.New("the host is not a DNS name")
	}
	return nil
}

// PublicDNSName: letters, digits and hyphens in non-empty labels, at least two labels, no trailing dot, and not a number in any of the
// forms an address is written in (inet_aton: one to four parts, each decimal, octal or hex).
func PublicDNSName(host string) bool {
	host = strings.ToLower(host)
	if !PlainHostName(host) || !strings.Contains(host, ".") {
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

// RefuseInternalAddress is the Control hook of the dialer of a hop: it runs for each address a connection is about to be made to, after
// the name was resolved, so the address that is refused is the one that would be used.
func RefuseInternalAddress(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("refusing to dial %q: not an address", address)
	}
	if InternalAddr(ap.Addr()) {
		return fmt.Errorf("refusing to dial %s: %w", ap.Addr(), ErrInternalAddress)
	}
	return nil
}

var internalPrefixes = func() []netip.Prefix {
	out := make([]netip.Prefix, 0, 8)
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
func InternalAddr(a netip.Addr) bool {
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
				return InternalAddr(netip.AddrFrom4([4]byte(b[12:16])))
			case p.Bits() == 16: // 6to4: bytes 2 to 5
				return InternalAddr(netip.AddrFrom4([4]byte(b[2:6])))
			}
		}
	}
	return false
}

// PlainHostName is true for dot-separated labels of letters, digits and hyphen, none empty or starting or ending with a hyphen.
func PlainHostName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// NewTransport is the default transport with a dialer that refuses internal addresses for the requests whose context is a hop (HopContext).
// resolver is nil in production (the system resolver); a test passes its own to name an address. Proxies from the environment stay as the
// default transport has them: with one, the connection is made to the proxy, so the check judges the proxy's address and not the target's.
func NewTransport(headerTimeout time.Duration, resolver *net.Resolver) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = headerTimeout
	plain := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Resolver: resolver}
	guarded := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Resolver: resolver, Control: RefuseInternalAddress}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if IsHop(ctx) {
			return guarded.DialContext(ctx, network, addr)
		}
		return plain.DialContext(ctx, network, addr)
	}
	return tr
}

// ErrRedirectRefused: a redirect was not followed (a bad target, or too many redirects).
var ErrRedirectRefused = errors.New("a redirect was refused")

// Hops is a RoundTripper for a client that follows redirects by itself (go-containerregistry's does). A request that is a redirect hop (the
// client sets Request.Response on it) is refused unless it stays on the origin of the first request of the chain, or CheckRedirect accepts
// its target; it is sent with a HopContext, so a transport made by NewTransport dials it through the check. MaxHops bounds the chain.
// The first request is never touched: a registry the operator listed may be on a private address. Credentials are the caller's business
// (a client must not add them to a hop; go-containerregistry sends its token only to the registry's own host).
type Hops struct {
	Base    http.RoundTripper
	MaxHops int
}

// RoundTrip implements http.RoundTripper.
func (h Hops) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Response == nil {
		return h.Base.RoundTrip(req)
	}
	depth, first := 0, req
	for first.Response != nil && first.Response.Request != nil {
		depth, first = depth+1, first.Response.Request
	}
	if h.MaxHops > 0 && depth > h.MaxHops {
		return nil, fmt.Errorf("%w: too many redirects", ErrRedirectRefused)
	}
	if !sameOrigin(first.URL, req.URL) {
		if err := CheckRedirect(req.URL); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRedirectRefused, err)
		}
		req = req.WithContext(HopContext(req.Context()))
	}
	return h.Base.RoundTrip(req)
}

// sameOrigin: the same scheme, host and effective port.
func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}
