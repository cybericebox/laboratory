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

func TestAuthorizeCN(t *testing.T) {
	allowed := []string{"platform", "daemon"}

	if err := authorizeCN(ctxWithCN("platform"), allowed); err != nil {
		t.Errorf("platform should pass: %v", err)
	}
	if err := authorizeCN(ctxWithCN("intruder"), allowed); err == nil {
		t.Error("intruder must be rejected")
	}
	if err := authorizeCN(context.Background(), allowed); err == nil {
		t.Error("missing peer cert must be rejected")
	}
}
