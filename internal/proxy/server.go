package proxy

import (
	"crypto/subtle"
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

// ProxyProtocol configures ProxyProtocolListener.
type ProxyProtocol struct {
	// Trusted are the peers (IPs or CIDRs) whose PROXY header may be used; empty: the feature is off.
	Trusted []string
	// Secret and Next are the values of the TLV of type TLVType that prove the header comes from our edge: the header's address is
	// used only when the TLV equals one of them (compared in constant time). An empty Secret requires no TLV.
	Secret, Next  string
	TLVType       byte
	HeaderTimeout time.Duration
}

// ProxyProtocolListener wraps the listener so that a connection from one of the trusted peers may begin with a PROXY protocol header,
// whose address becomes the connection's RemoteAddr (policy USE: optional, never required). A connection from any other peer has its
// header consumed and ignored (policy IGNORE) and keeps the peer address. With a secret set, a header from a trusted peer without the
// right secret TLV is ignored too. An empty list returns the listener unchanged.
func ProxyProtocolListener(l net.Listener, o ProxyProtocol) (net.Listener, error) {
	var from []string
	for _, t := range o.Trusted {
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
	var inner net.Listener = &proxyproto.Listener{Listener: l, ReadHeaderTimeout: o.HeaderTimeout, ConnPolicy: policy}
	if o.Secret == "" {
		return inner, nil
	}
	return &secretListener{Listener: inner, secrets: [][]byte{[]byte(o.Secret), []byte(o.Next)}, tlv: proxyproto.PP2Type(o.TLVType)}, nil
}

// secretListener gives a connection the address of its PROXY header only when the header carries the secret.
type secretListener struct {
	net.Listener
	secrets [][]byte // current, next ("" = none)
	tlv     proxyproto.PP2Type
}

func (l *secretListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	pc, ok := c.(*proxyproto.Conn)
	if !ok {
		return c, nil
	}
	return &secretConn{Conn: pc, l: l}, nil
}

type secretConn struct {
	*proxyproto.Conn
	l    *secretListener
	once sync.Once
	addr net.Addr
}

// RemoteAddr is the address of the header when it proves itself, the peer address otherwise.
func (c *secretConn) RemoteAddr() net.Addr {
	c.once.Do(func() {
		c.addr = c.Raw().RemoteAddr()
		if h := c.ProxyHeader(); h != nil && c.l.proves(h) {
			c.addr = c.Conn.RemoteAddr()
		}
	})
	return c.addr
}

// proves says the header has the TLV with the current or the next secret. Both are compared, so the time does not tell which one matched.
func (l *secretListener) proves(h *proxyproto.Header) bool {
	tlvs, err := h.TLVs()
	if err != nil {
		return false
	}
	ok := 0
	for _, t := range tlvs {
		if t.Type != l.tlv {
			continue
		}
		for _, s := range l.secrets {
			if len(s) > 0 {
				ok |= subtle.ConstantTimeCompare(t.Value, s)
			}
		}
	}
	return ok == 1
}
