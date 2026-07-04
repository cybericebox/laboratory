package grpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// authorizeCN extracts the verified client certificate CN from the mTLS
// handshake and checks it against the allowlist. This is the only
// authorization gate — mTLS already proved the cert was signed by our CA.
func authorizeCN(ctx context.Context, allowed []string) error {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return fmt.Errorf("no peer in context")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return fmt.Errorf("no client certificate presented")
	}
	cn := tlsInfo.State.PeerCertificates[0].Subject.CommonName
	for _, a := range allowed {
		if a == cn {
			return nil
		}
	}
	return fmt.Errorf("client CN %q not authorized", cn)
}
