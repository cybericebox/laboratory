package grpc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/cybericebox/laboratory/internal/imagecache"
)

// probeResolver asks the registry itself for every reference, a pinned one too.
type probeResolver struct {
	kc authn.Keychain
	rt http.RoundTripper
}

func (p probeResolver) Resolve(ctx context.Context, ref string) (string, error) {
	return imagecache.ProbeDigest(ctx, ref, p.kc, p.rt)
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
			ip := net.ParseIP(host)
			if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
				ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
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
