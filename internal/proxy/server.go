package proxy

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
)

// NewHTTPServer is the L7 proxy's HTTPS server with the limits that apply before any routing or authentication: a client
// that dribbles its headers or its body, or opens connections and says nothing, is cut instead of holding a connection
// and a goroutine forever. There is no write timeout (responses stream; an upgraded connection is bounded by the live
// check), and a hijacked connection has its deadlines cleared by the handler.
func NewHTTPServer(cfg *L7Config, handler http.Handler, tlsConfig *tls.Config) *http.Server {
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

// ProxyProtocolListener wraps the listener so that a connection from one of the trusted peers may begin with a PROXY protocol header,
// whose address becomes the connection's RemoteAddr (policy USE: optional, never required). A connection from any other peer has its
// header consumed and ignored (policy IGNORE) and keeps the peer address. An empty list returns the listener unchanged.
func ProxyProtocolListener(l net.Listener, trusted []string, headerTimeout time.Duration) (net.Listener, error) {
	var from []string
	for _, t := range trusted {
		if t = strings.TrimSpace(t); t != "" {
			from = append(from, t)
		}
	}
	if len(from) == 0 {
		return l, nil
	}
	policy, err := proxyproto.PolicyFromRanges(from, proxyproto.USE, proxyproto.IGNORE)
	if err != nil {
		return nil, fmt.Errorf("L7_PROXY_PROTOCOL_FROM: %w", err)
	}
	return &proxyproto.Listener{Listener: l, ReadHeaderTimeout: headerTimeout, ConnPolicy: policy}, nil
}
