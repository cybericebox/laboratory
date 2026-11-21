package proxy

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync"
)

// NewHTTPServer is the L7 proxy's HTTPS server with the limits that apply before any routing or authentication: a client
// that dribbles its headers or its body, or opens connections and says nothing, is cut instead of holding a connection
// and a goroutine forever. There is no write timeout (responses stream; an upgraded connection is bounded by the live
// check), and a hijacked connection has its deadlines cleared by the handler.
//
// The client's address is the TCP peer: the load balancer keeps it (externalTrafficPolicy Local), so there is no header in front of the TLS
// to read and none is believed.
//
// The server offers HTTP/2 and HTTP/1.1 (ALPN h2, http/1.1): a browser takes h2; the WebSocket of the console stays on HTTP/1.1. A tlsConfig
// that names its own protocols keeps them.
func NewHTTPServer(cfg *L7Config, handler http.Handler, tlsConfig *tls.Config) *http.Server {
	if tlsConfig != nil && len(tlsConfig.NextProtos) == 0 {
		tlsConfig.NextProtos = []string{"h2", "http/1.1"}
	}
	return &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}
}

// LimitListener returns a listener that accepts at most max connections at a time: a connection past it waits in the kernel's backlog
// until one closes (the header timeout frees the slots a client that says nothing holds). 0 = no limit.
func LimitListener(l net.Listener, max int) net.Listener {
	if max <= 0 {
		return l
	}
	return &limitListener{Listener: l, sem: make(chan struct{}, max), done: make(chan struct{})}
}

type limitListener struct {
	net.Listener
	sem  chan struct{}
	once sync.Once
	done chan struct{}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.sem <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
}

func (l *limitListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.done) })
	return err
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
