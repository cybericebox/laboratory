package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

func tlsStateWithCert(cert *x509.Certificate) tls.ConnectionState {
	return tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
}

func ctxWithCN(cn string) context.Context {
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
	ti := credentials.TLSInfo{State: tlsStateWithCert(cert)}
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: ti})
}

func TestClientCN(t *testing.T) {
	if cn, err := clientCN(ctxWithCN("platform")); err != nil || cn != "platform" {
		t.Errorf("cn %q %v", cn, err)
	}
	if _, err := clientCN(context.Background()); err == nil {
		t.Error("missing peer cert must be rejected")
	}
}
