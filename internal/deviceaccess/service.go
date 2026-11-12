// Package deviceaccess is the device access service: the one component on a lab node that holds the container runtime socket and
// opens a shell (a console) or moves a file in a lab device on behalf of the L7 proxy.
//
// Path of a console: participant, L7 proxy (authorization), this service on the node of the device, a shell in the device.
//
// The service trusts the proxy and nothing else; the Cilium policy lets only the proxy pods reach it. It does not trust the proxy
// with the container: a request names a lab device (namespace, lab, device) and the service finds the container itself from what the
// runtime shows (see Resolve), copies the process spec from that container (see ProcessFor) and refuses everything that is not a
// lab device with a console. It has no Kubernetes API rights.
package deviceaccess

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
)

// Conn is one stream with the proxy: whole messages, no ordering between the two directions. The WebSocket adapter is in
// server.go; tests use an in-memory one.
type Conn interface {
	// Read returns the next message and whether it is a text message (the control JSON) or a binary one (a frame).
	Read(ctx context.Context) (text bool, msg []byte, err error)
	// WriteText sends a control message; WriteBinary sends a frame.
	WriteText(ctx context.Context, msg []byte) error
	WriteBinary(ctx context.Context, msg []byte) error
}

// Limits are the chart values of the service. A zero limit is no limit, except the timeouts, which have defaults.
type Limits struct {
	// PerClient, PerLab and PerNode bound the consoles open at once: of one client of one group, in one lab, and on this node.
	PerClient, PerLab, PerNode int
	// Idle closes a console that no byte crossed for that long. Lifetime closes any console at that age.
	Idle, Lifetime time.Duration
	// WriteTimeout bounds every write towards the proxy or the device: a peer that does not take bytes for that long is cut.
	WriteTimeout time.Duration
	// Cleanup bounds the time the clean-up of an exec may take.
	Cleanup time.Duration
}

// DefaultLimits are the values of the chart defaults.
func DefaultLimits() Limits {
	return Limits{
		PerClient: 3, PerLab: 10, PerNode: 200,
		Idle: 30 * time.Minute, Lifetime: 8 * time.Hour,
		WriteTimeout: 30 * time.Second, Cleanup: 10 * time.Second,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.WriteTimeout <= 0 {
		l.WriteTimeout = d.WriteTimeout
	}
	if l.Cleanup <= 0 {
		l.Cleanup = d.Cleanup
	}
	return l
}

// Config builds a Service.
type Config struct {
	Runtime Runtime
	// Auth authenticates every open request. It is required: a service without one refuses every request.
	Auth   *Auth
	Limits Limits
	Log    logr.Logger
	// Tap, when set, sees every byte of a console in both directions (see stream.pass). Nothing sets it today.
	Tap Tap
}

// Service serves consoles.
type Service struct {
	rt      Runtime
	auth    *Auth
	lim     Limits
	log     logr.Logger
	tap     Tap
	m       *metrics
	reg     *prometheus.Registry
	counter limiter

	mu       sync.Mutex
	stopping bool
	active   map[*stream]struct{}
	wg       sync.WaitGroup
}

// New returns a Service.
func New(c Config) *Service {
	reg := prometheus.NewRegistry()
	s := &Service{rt: c.Runtime, auth: c.Auth, lim: c.Limits.withDefaults(), log: c.Log, tap: c.Tap, reg: reg, m: newMetrics(reg), active: map[*stream]struct{}{}}
	s.counter = limiter{perClient: map[string]int{}, perLab: map[string]int{}}
	return s
}

// Registry is the metrics registry of the service.
func (s *Service) Registry() *prometheus.Registry { return s.reg }

// Serve handles one stream: the open request is the first text message of conn. It returns when the stream is over and everything
// it started is cleaned up.
func (s *Service) Serve(ctx context.Context, conn Conn) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()

	req, err := readOpen(ctx, conn)
	if err != nil {
		s.refuse(ctx, conn, wire.OpenRequest{}, wire.CodeBadRequest, err)
		return
	}
	// The open request is authenticated before anything else is done with it. Nothing else of it is logged on a failure, and the answer does
	// not say which check failed.
	if err := s.auth.Check(req); err != nil {
		s.refuse(ctx, conn, wire.OpenRequest{}, wire.CodeUnauthorized, err)
		return
	}
	if err := validateOpen(req); err != nil {
		s.refuse(ctx, conn, req, wire.CodeBadRequest, err)
		return
	}
	switch req.Kind {
	case wire.KindConsole:
		s.serveConsole(ctx, conn, req)
	default:
		s.refuse(ctx, conn, req, wire.CodeBadRequest, fmt.Errorf("unknown kind %q", req.Kind))
	}
}

// Shutdown stops new streams, ends the running ones and waits until their execs are removed (or ctx ends).
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	for st := range s.active {
		st.stop(wire.ReasonShutdown)
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) track(st *stream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return false
	}
	s.active[st] = struct{}{}
	return true
}

func (s *Service) untrack(st *stream) {
	s.mu.Lock()
	delete(s.active, st)
	s.mu.Unlock()
}

func readOpen(ctx context.Context, conn Conn) (wire.OpenRequest, error) {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	text, msg, err := conn.Read(rctx)
	if err != nil {
		return wire.OpenRequest{}, err
	}
	if !text || len(msg) > wire.MaxOpen {
		return wire.OpenRequest{}, errors.New("the first message is not an open request")
	}
	return parseOpen(msg)
}

// refuse answers an open request with an error code and counts it.
func (s *Service) refuse(ctx context.Context, conn Conn, req wire.OpenRequest, code string, cause error) {
	s.m.refused.WithLabelValues(code).Inc()
	s.log.Info("refused", "code", code, "kind", req.Kind, "lab", req.Lab, "device", req.Device, "namespace", req.Namespace, "reason", cause.Error())
	wctx, cancel := context.WithTimeout(ctx, s.lim.WriteTimeout)
	defer cancel()
	_ = conn.WriteText(wctx, encodeReply(wire.OpenReply{Error: code}))
}

// resolve finds the device container of a request and maps the refusals of Resolve to the codes a caller sees.
func (s *Service) resolve(ctx context.Context, req wire.OpenRequest) (Device, string, error) {
	dev, err := Resolve(ctx, s.rt, Target{Namespace: req.Namespace, Lab: req.Lab, Device: req.Device})
	switch {
	case err == nil:
		return dev, "", nil
	case errors.Is(err, ErrDisabled):
		return Device{}, wire.CodeDisabled, err
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrAmbiguous):
		return Device{}, wire.CodeNotFound, err
	}
	return Device{}, wire.CodeUnavailable, err
}

// labKey identifies a lab for the limits: the same lab name may exist in two groups.
func labKey(req wire.OpenRequest) string { return req.Namespace + "/" + req.Lab }

func clientKey(req wire.OpenRequest) string { return req.Group + "/" + req.Client }

// limiter counts the open streams.
type limiter struct {
	mu        sync.Mutex
	consoles  int
	perClient map[string]int
	perLab    map[string]int
}

func (l *limiter) acquireConsole(lim Limits, client, lab string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lim.PerNode > 0 && l.consoles >= lim.PerNode ||
		lim.PerClient > 0 && l.perClient[client] >= lim.PerClient ||
		lim.PerLab > 0 && l.perLab[lab] >= lim.PerLab {
		return false
	}
	l.consoles++
	l.perClient[client]++
	l.perLab[lab]++
	return true
}

func (l *limiter) releaseConsole(client, lab string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.consoles--
	dec(l.perClient, client)
	dec(l.perLab, lab)
}

func dec(m map[string]int, k string) {
	if m[k]--; m[k] <= 0 {
		delete(m, k)
	}
}
