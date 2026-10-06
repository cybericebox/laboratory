package grpc

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
)

// SplitLimits bound what a caller without a client certificate may cost. Such a caller can reach only the Enroll server (see
// Server), so these protect the one door that is open to everyone.
type SplitLimits struct {
	// HandshakeTimeout is how long a connection may take to finish the TLS handshake.
	HandshakeTimeout time.Duration
	// MaxHandshakes is how many handshakes may run at once; a connection that finds no room is closed at once.
	MaxHandshakes int
	// MaxAnonymous is how many connections without a client certificate may be open together. There is no per-address limit: behind the
	// gateway's TLS passthrough every caller has the gateway's address, so one would be a global limit that an anonymous host could use
	// to lock out every tenant.
	MaxAnonymous int
}

// DefaultSplitLimits are what the agent runs with unless the chart says otherwise.
func DefaultSplitLimits() SplitLimits {
	return SplitLimits{HandshakeTimeout: 10 * time.Second, MaxHandshakes: 64, MaxAnonymous: 256}
}

// preHandshaked is the transport credentials of a server that is handed connections whose TLS handshake is already done (by
// splitListener): it only reports what the handshake found.
type preHandshaked struct{}

func (preHandshaked) ServerHandshake(c net.Conn) (net.Conn, credentials.AuthInfo, error) {
	tc, ok := c.(*tlsConn)
	if !ok {
		return nil, nil, errors.New("the connection was not handshaken by the agent's listener")
	}
	return tc, credentials.TLSInfo{State: tc.ConnectionState(), CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.PrivacyAndIntegrity}}, nil
}
func (preHandshaked) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("server only")
}
func (preHandshaked) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls", SecurityVersion: "1.2"}
}
func (preHandshaked) Clone() credentials.TransportCredentials { return preHandshaked{} }
func (preHandshaked) OverrideServerName(string) error         { return nil }

// tlsConn is a connection that finished its handshake; release runs once when it closes.
type tlsConn struct {
	*tls.Conn
	once    sync.Once
	release func()
}

func (c *tlsConn) Close() error {
	c.once.Do(func() {
		if c.release != nil {
			c.release()
		}
	})
	return c.Conn.Close()
}

// splitListener accepts TCP connections, completes the TLS handshake itself and hands each connection to one of two listeners:
// the main one when the client presented a certificate (the handshake verified it against the client CA), the anonymous one when
// it did not. The two are served by two gRPC servers with different limits, so a caller without a certificate never reaches a
// server that accepts large messages, and the size of a message is bounded before anything is decoded.
type splitListener struct {
	inner net.Listener
	cfg   *tls.Config
	lim   SplitLimits

	main, anon *chanListener

	hs    chan struct{} // handshake slots
	mu    sync.Mutex
	conns int // anonymous connections open
}

func newSplitListener(inner net.Listener, cfg *tls.Config, lim SplitLimits) *splitListener {
	def := DefaultSplitLimits()
	if lim.HandshakeTimeout <= 0 {
		lim.HandshakeTimeout = def.HandshakeTimeout
	}
	if lim.MaxHandshakes <= 0 {
		lim.MaxHandshakes = def.MaxHandshakes
	}
	if lim.MaxAnonymous <= 0 {
		lim.MaxAnonymous = def.MaxAnonymous
	}
	return &splitListener{
		inner: inner, cfg: cfg, lim: lim,
		main: newChanListener(inner.Addr()), anon: newChanListener(inner.Addr()),
		hs: make(chan struct{}, lim.MaxHandshakes),
	}
}

// Run accepts until the inner listener closes.
func (s *splitListener) Run() error {
	defer func() { _ = s.main.Close() }()
	defer func() { _ = s.anon.Close() }()
	for {
		c, err := s.inner.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		select {
		case s.hs <- struct{}{}:
			go s.handshake(c)
		default:
			_ = c.Close()
		}
	}
}

func (s *splitListener) handshake(raw net.Conn) {
	defer func() { <-s.hs }()
	tc := tls.Server(raw, s.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), s.lim.HandshakeTimeout)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return
	}
	if tc.ConnectionState().NegotiatedProtocol != "h2" {
		_ = raw.Close() // not gRPC
		return
	}
	c := &tlsConn{Conn: tc}
	if len(tc.ConnectionState().PeerCertificates) > 0 {
		s.main.put(c)
		return
	}
	// Without a certificate: only what the Enroll server allows, and only so many.
	s.mu.Lock()
	if s.conns >= s.lim.MaxAnonymous {
		s.mu.Unlock()
		_ = c.Close()
		return
	}
	s.conns++
	s.mu.Unlock()
	c.release = func() {
		s.mu.Lock()
		s.conns--
		s.mu.Unlock()
	}
	s.anon.put(c)
}

// chanListener is a net.Listener fed by put.
type chanListener struct {
	addr net.Addr
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newChanListener(a net.Addr) *chanListener {
	return &chanListener{addr: a, ch: make(chan net.Conn), done: make(chan struct{})}
}

func (l *chanListener) put(c net.Conn) {
	select {
	case l.ch <- c:
	case <-l.done:
		_ = c.Close()
	case <-time.After(5 * time.Second): // nobody is serving
		_ = c.Close()
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *chanListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *chanListener) Addr() net.Addr { return l.addr }
