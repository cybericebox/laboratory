package grpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/cybericebox/laboratory/internal/agent/config"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"github.com/cybericebox/laboratory/pkg/tlsreload"
)

// MaxMessageSize is the largest gRPC message the agent accepts and sends.
const MaxMessageSize = 64 << 20

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
	// A client certificate is optional at the handshake: Enroll is called before the client has one.
	// Every other call requires it (see New).
	files.OptionalClientAuth = true
	return credentials.NewTLS(files.ServerConfig("h2")), nil
}

// New builds the gRPC server with mTLS creds and CN-allowlist interceptors.
func New(cfg *config.Config, impl protobuf.LabManagerServer) (*grpc.Server, error) {
	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(apiErrorInterceptor),
		// Batch calls carry up to MaxItems lab specs and answer with as many objects.
		grpc.MaxRecvMsgSize(MaxMessageSize),
		grpc.MaxSendMsgSize(MaxMessageSize),
	}
	if cfg.ServerTLS.Enabled {
		creds, err := serverCredentials(cfg)
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.Creds(creds))
	}
	if cfg.MTLS.Enabled {
		// Every call must carry a client certificate, and its CN must be a tenant.
		admit := func(ctx context.Context) error {
			if _, err := clientCN(ctx); err != nil {
				return status.Error(codes.Unauthenticated, err.Error())
			}
			if a, ok := impl.(interface{ Authorize(context.Context) error }); ok {
				return a.Authorize(ctx)
			}
			return nil
		}
		opts = append(opts,
			grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
				// Enroll is the one call without a client certificate: the one-time token authenticates it.
				if info.FullMethod != protobuf.LabManager_Enroll_FullMethodName {
					if err := admit(ctx); err != nil {
						return nil, err
					}
				}
				return h(ctx, req)
			}),
			grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
				if err := admit(ss.Context()); err != nil {
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
