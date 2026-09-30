package grpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/cybericebox/laboratory/internal/agent/config"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"github.com/cybericebox/laboratory/pkg/tlsreload"
)

// serverCredentials serves the cert-manager certificate from the mounted files
// and re-reads it (and the client CA) when they change, so a renewal needs no
// restart.
func serverCredentials(cfg *config.Config) (credentials.TransportCredentials, error) {
	caFile := ""
	if cfg.MTLS.Enabled {
		caFile = cfg.MTLS.ClientCAFile
	}
	files, err := tlsreload.New(cfg.ServerTLS.CertFile, cfg.ServerTLS.KeyFile, caFile)
	if err != nil {
		return nil, fmt.Errorf("load server TLS: %w", err)
	}
	return credentials.NewTLS(files.ServerConfig("h2")), nil
}

// New builds the gRPC server with mTLS creds and CN-allowlist interceptors.
func New(cfg *config.Config, impl protobuf.LabManagerServer) (*grpc.Server, error) {
	opts := []grpc.ServerOption{grpc.ChainUnaryInterceptor(apiErrorInterceptor)}
	if cfg.ServerTLS.Enabled {
		creds, err := serverCredentials(cfg)
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.Creds(creds))
	}
	if cfg.MTLS.Enabled {
		opts = append(opts,
			grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
				if err := authorizeCN(ctx, cfg.MTLS.AllowedClientCNs); err != nil {
					return nil, err
				}
				return h(ctx, req)
			}),
			grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
				if err := authorizeCN(ss.Context(), cfg.MTLS.AllowedClientCNs); err != nil {
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
