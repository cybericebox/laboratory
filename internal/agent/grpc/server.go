package grpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/cybericebox/laboratory/internal/agent/config"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"github.com/cybericebox/laboratory/pkg/tlsreload"
)

// MaxRecvMessageSize is the largest gRPC message the agent reads from a caller with a client certificate. Any certificate holder can
// send these, a request is decoded several times, and a connection may carry many streams, so it is a few MiB, not tens (the agent has
// 128Mi). MaxSendMessageSize is what it may answer with: a batch of up to MaxItems objects.
const (
	MaxRecvMessageSize = 4 << 20
	MaxSendMessageSize = 64 << 20
)

// DefaultEnrollMaxMessage is the largest message the Enroll server (the one open to callers without a certificate) reads.
const DefaultEnrollMaxMessage = 64 << 10

// Server is the agent's gRPC front. With mTLS on it is two servers behind one listener: the main one, for connections that
// presented a client certificate, and the Enroll server for those that did not, which knows the Enroll call only, reads
// messages of a few kilobytes, and is rate limited. A connection's kind is decided by the TLS handshake before anything is read
// from it, so nothing an anonymous caller sends is decoded by the main server and no message of megabytes is buffered for it.
type Server struct {
	main, enroll *grpc.Server
	tls          *tlsreload.Files
	lim          SplitLimits

	mu    sync.Mutex
	split *splitListener
	// graceful is set by GracefulStop: Serve then leaves the running calls to finish instead of closing them.
	graceful atomic.Bool
}

// Config of the server's limits (see config.ServerLimits).
func limitsOf(cfg *config.Config) (maxStreams uint32, age, grace time.Duration, minPing time.Duration, enrollMax int, enrollRate float64, enrollBurst int, split SplitLimits) {
	l := cfg.Server
	split = SplitLimits{
		HandshakeTimeout: l.HandshakeTimeout, MaxHandshakes: l.MaxHandshakes, MaxAnonymous: l.MaxAnonymousConns,
	}
	return uint32(l.MaxConcurrentStreams), l.MaxConnectionAge, l.MaxConnectionAgeGrace, l.KeepaliveMinTime, l.EnrollMaxMessage, l.EnrollRate, l.EnrollBurst, split
}

// New builds the server. With mTLS off it refuses to start unless the insecure mode was asked for by name.
func New(cfg *config.Config, impl protobuf.LabManagerServer) (*Server, error) {
	maxStreams, age, grace, minPing, enrollMax, enrollRate, enrollBurst, split := limitsOf(cfg)
	if maxStreams == 0 {
		maxStreams = 64
	}
	if age <= 0 {
		age = time.Hour
	}
	if grace <= 0 {
		grace = time.Minute
	}
	if minPing <= 0 {
		minPing = 10 * time.Second
	}
	if enrollMax <= 0 {
		enrollMax = DefaultEnrollMaxMessage
	}
	if enrollRate <= 0 {
		enrollRate, enrollBurst = 5, 10
	}
	if enrollBurst <= 0 {
		enrollBurst = 10
	}
	recheck := cfg.Server.StreamRecheck
	if recheck <= 0 {
		recheck = 30 * time.Second
	}

	common := []grpc.ServerOption{
		grpc.MaxConcurrentStreams(maxStreams),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: age, MaxConnectionAgeGrace: grace, Time: 2 * time.Minute, Timeout: 20 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: minPing, PermitWithoutStream: false}),
	}
	mainOpts := append(append([]grpc.ServerOption{}, common...),
		grpc.ChainUnaryInterceptor(apiErrorInterceptor),
		// Batch calls carry up to MaxItems lab specs and answer with as many objects.
		grpc.MaxRecvMsgSize(MaxRecvMessageSize),
		grpc.MaxSendMsgSize(MaxSendMessageSize),
	)
	s := &Server{lim: split}

	if !cfg.MTLS.Enabled {
		if !cfg.AllowInsecure {
			return nil, fmt.Errorf("AGENT_MTLS_ENABLED is false: every caller would be the default tenant. Set AGENT_ALLOW_INSECURE=true to run like that on purpose (local development only)")
		}
		if cfg.ServerTLS.Enabled {
			creds, err := plainTLS(cfg)
			if err != nil {
				return nil, err
			}
			mainOpts = append(mainOpts, grpc.Creds(creds))
		}
		s.main = grpc.NewServer(mainOpts...)
		protobuf.RegisterLabManagerServer(s.main, impl)
		return s, nil
	}
	if !cfg.ServerTLS.Enabled {
		return nil, fmt.Errorf("AGENT_MTLS_ENABLED needs AGENT_TLS_ENABLED: client certificates ride on TLS")
	}
	files, err := tlsreload.New(cfg.ServerTLS.CertFile, cfg.ServerTLS.KeyFile, cfg.MTLS.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("load server TLS: %w", err)
	}
	// A client certificate is optional at the handshake: Enroll is called before the client has one. The listener sends a
	// connection without one to the Enroll server only (see Server).
	files.OptionalClientAuth = true
	s.tls = files

	// Every call on the main server carries a client certificate (the listener saw to that); its CN must be a tenant of the
	// current epoch, and a stream asks again while it runs.
	admit := func(ctx context.Context) error {
		cn, err := clientCN(ctx)
		if err != nil {
			return status.Error(codes.Unauthenticated, err.Error())
		}
		if cn == "" {
			return status.Error(codes.Unauthenticated, "the client certificate has no common name")
		}
		if a, ok := impl.(interface{ Authorize(context.Context) error }); ok {
			return a.Authorize(ctx)
		}
		return nil
	}
	mainOpts = append(mainOpts,
		grpc.Creds(preHandshaked{}),
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			// Enroll belongs to the anonymous server; a caller who already has a certificate enrolls with the token the same way,
			// but through a connection without one.
			if info.FullMethod == protobuf.LabManager_Enroll_FullMethodName {
				return nil, status.Error(codes.FailedPrecondition, "Enroll is for a connection without a client certificate: connect without one")
			}
			if err := admit(ctx); err != nil {
				return nil, err
			}
			return h(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			if err := admit(ss.Context()); err != nil {
				return err
			}
			w := watchStream(ss, recheck, admit)
			err := h(srv, w)
			if why := w.failure(); why != nil {
				return why // the stream was cut because the caller's authority ended: say that, not "canceled"
			}
			return err
		}),
	)
	s.main = grpc.NewServer(mainOpts...)
	protobuf.RegisterLabManagerServer(s.main, impl)

	// The Enroll server: the Enroll method only, small messages, a rate limit for all of it.
	limiter := rate.NewLimiter(rate.Limit(enrollRate), enrollBurst)
	enrollOpts := append(append([]grpc.ServerOption{}, common...),
		grpc.Creds(preHandshaked{}),
		grpc.MaxRecvMsgSize(enrollMax),
		grpc.MaxSendMsgSize(1<<20),
		grpc.MaxConcurrentStreams(8),
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if !limiter.Allow() {
				return nil, status.Error(codes.ResourceExhausted, "too many enrollment attempts: try again in a moment")
			}
			return h(ctx, req)
		}),
		grpc.ChainUnaryInterceptor(apiErrorInterceptor),
	)
	s.enroll = grpc.NewServer(enrollOpts...)
	desc := protobuf.LabManager_ServiceDesc
	desc.Methods = nil
	for _, m := range protobuf.LabManager_ServiceDesc.Methods {
		if m.MethodName == "Enroll" {
			desc.Methods = append(desc.Methods, m)
		}
	}
	desc.Streams = nil
	s.enroll.RegisterService(&desc, impl)
	return s, nil
}

// plainTLS serves the certificate without asking for client certificates (mTLS off, TLS on).
func plainTLS(cfg *config.Config) (credentials.TransportCredentials, error) {
	files, err := tlsreload.New(cfg.ServerTLS.CertFile, cfg.ServerTLS.KeyFile, "")
	if err != nil {
		return nil, fmt.Errorf("load server TLS: %w", err)
	}
	return credentials.NewTLS(files.ServerConfig("h2")), nil
}

// Serve serves until the listener closes or the server stops.
func (s *Server) Serve(lis net.Listener) error {
	if s.enroll == nil {
		return s.main.Serve(lis)
	}
	sp := newSplitListener(lis, s.tls.ServerConfig("h2"), s.lim)
	s.mu.Lock()
	s.split = sp
	s.mu.Unlock()
	errs := make(chan error, 3)
	go func() { errs <- sp.Run() }()
	go func() { errs <- s.main.Serve(sp.main) }()
	go func() { errs <- s.enroll.Serve(sp.anon) }()
	err := <-errs
	if !s.graceful.Load() { // a deliberate GracefulStop lets the running calls finish; anything else ends them
		s.Stop()
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// Stop closes everything at once.
func (s *Server) Stop() {
	s.main.Stop()
	if s.enroll != nil {
		s.enroll.Stop()
	}
}

// GracefulStop stops accepting connections and lets running calls finish.
func (s *Server) GracefulStop() {
	s.graceful.Store(true)
	s.mu.Lock()
	sp := s.split
	s.mu.Unlock()
	if sp != nil {
		_ = sp.inner.Close()
	}
	if s.enroll != nil {
		s.enroll.GracefulStop()
	}
	s.main.GracefulStop()
}

// watchStream gives a stream a context that ends when the caller's authority does: every interval the stream is admitted
// again, so a certificate that was revoked (a new enrollment) or has expired stops a Monitoring or an export that started
// before. The handler sees its context cancel and returns; the error the caller gets says why.
func watchStream(ss grpc.ServerStream, interval time.Duration, admit func(context.Context) error) *watchedStream {
	ctx, cancel := context.WithCancelCause(ss.Context())
	w := &watchedStream{ServerStream: ss, ctx: ctx}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := admit(ctx); err != nil {
					w.setErr(err)
					cancel(err)
					return
				}
			}
		}
	}()
	// The goroutine ends with the stream: the server cancels the parent when the handler returns.
	return w
}

type watchedStream struct {
	grpc.ServerStream
	ctx context.Context
	mu  sync.Mutex
	err error
}

func (w *watchedStream) Context() context.Context { return w.ctx }
func (w *watchedStream) setErr(err error)         { w.mu.Lock(); w.err = err; w.mu.Unlock() }
func (w *watchedStream) failure() error           { w.mu.Lock(); defer w.mu.Unlock(); return w.err }
