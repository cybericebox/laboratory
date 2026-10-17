package grpc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/cybericebox/laboratory/internal/imagecache"
)

// probeResolver asks the registry itself for every reference, a pinned one too.
type probeResolver struct {
	kc authn.Keychain
	rt http.RoundTripper
}

func (p probeResolver) Resolve(ctx context.Context, ref string) (string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}
	// go-containerregistry speaks plain http to localhost, *.local and private hosts: a
	// credential never travels that way.
	if parsed.Context().Registry.Scheme() == "http" {
		return "", errPlainHTTP
	}
	return imagecache.ProbeDigest(ctx, ref, p.kc, p.rt)
}

var errPlainHTTP = errors.New("credentialed check refused: registry would be reached over plain http")

// blockedPrefixes mirrors the platform daemon's dial guard (pkg/email AddressBlocked): ranges
// the netip predicates do not cover (this network, carrier-grade NAT, benchmarking, reserved,
// IETF protocol, NAT64).
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

func addressBlocked(ip netip.Addr) bool {
	ip = ip.Unmap()
	// allowlist: only global unicast passes (loopback, multicast, link-local, unspecified and
	// invalid fall out), then private and the special ranges below are denied
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

var errInternalAddress = errors.New("dial refused: internal address")

// guardedTransport is the transport of credentialed checks: the registry host comes from
// the platform user, so the dial itself (after DNS, redirects included) refuses loopback,
// private, link-local, multicast and unspecified addresses. No proxy from the environment.
func guardedTransport() *http.Transport {
	d := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errInternalAddress
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || addressBlocked(ip) {
				return errInternalAddress
			}
			return nil
		},
	}
	return &http.Transport{
		DialContext:           d.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
}
