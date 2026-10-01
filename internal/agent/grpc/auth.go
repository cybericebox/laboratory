package grpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// clientCN extracts the verified client certificate CN from the mTLS handshake. mTLS already
// proved the cert was signed by our CA; whether the CN is a known tenant is Authorize's call.
func clientCN(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", fmt.Errorf("no peer in context")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return "", fmt.Errorf("no client certificate presented")
	}
	return tlsInfo.State.PeerCertificates[0].Subject.CommonName, nil
}
