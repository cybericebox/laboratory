package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/cybericebox/laboratory/internal/agent/config"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func serverCredentials(cfg *config.Config) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}
	tc := &tls.Config{Certificates: []tls.Certificate{cert}}
	if cfg.MTLSEnabled {
		ca, err := os.ReadFile(cfg.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("append client CA")
		}
		tc.ClientAuth = tls.RequireAndVerifyClientCert
		tc.ClientCAs = pool
	}
	return credentials.NewTLS(tc), nil
}

// New builds the gRPC server with mTLS creds and CN-allowlist interceptors.
func New(cfg *config.Config, impl protobuf.LabManagerServer) (*grpc.Server, error) {
	var opts []grpc.ServerOption
	if cfg.TLS.Enabled {
		creds, err := serverCredentials(cfg)
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.Creds(creds))
	}
	if cfg.MTLSEnabled {
		opts = append(opts,
			grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
				if err := authorizeCN(ctx, cfg.AllowedClientCNs); err != nil {
					return nil, err
				}
				return h(ctx, req)
			}),
			grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
				if err := authorizeCN(ss.Context(), cfg.AllowedClientCNs); err != nil {
					return err
				}
				return h(srv, ss)
			}),
		)
	}
	s := grpc.NewServer(opts...)
	protobuf.RegisterLabManagerServer(s, impl)
	return s, nil
}
