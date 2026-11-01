package egressproxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/time/rate"
)

// Proxy is the transparent web proxy of the gateway. The gateway redirects the TCP 80 and 443 of a lab to it (whatever address the lab
// chose); it reads the name from the TLS server name or the HTTP Host, looks the name up itself, and connects there. The address the lab
// chose is never used, so a name in /etc/hosts, a DNS server of the lab's own or a bare address change nothing. TLS is not terminated.
type Proxy struct {
	Labs     *LabMap
	Resolver *Resolver
	Deny     *Denylist
	Stats    *Collector
	Log      logr.Logger

	// MaxConnAge is the longest a connection lives (default 10 minutes).
	MaxConnAge time.Duration
	// HostNewPerSecond is the most new connections per second to one host name through this gateway (default 5; 0 = no limit).
	HostNewPerSecond float64
	// LabMaxConns is the most connections a lab holds open, LabNewPerSecond the most it opens in a second (0 = no limit).
	LabMaxConns, LabNewPerSecond int
	// DialTimeout is how long a connection to the server may take (default 10 s).
	DialTimeout time.Duration
	// HelloTimeout is how long a client may take to send its first flight (default 10 s).
	HelloTimeout time.Duration
	// Dial replaces the dialer (tests); it must refuse what Resolver.Denied refuses.
	Dial func(ctx context.Context, addr string) (net.Conn, error)

	once  sync.Once
	mu    sync.Mutex
	hosts map[string]*rate.Limiter
	labs  map[string]*labLimits
	last  time.Time
}

type labLimits struct {
	open int
	new  *rate.Limiter
}

func (p *Proxy) defaults() { p.once.Do(p.setDefaults) }

func (p *Proxy) setDefaults() {
	if p.MaxConnAge <= 0 {
		p.MaxConnAge = 10 * time.Minute
	}
	if p.DialTimeout <= 0 {
		p.DialTimeout = 10 * time.Second
	}
	if p.HelloTimeout <= 0 {
		p.HelloTimeout = 10 * time.Second
	}
}

// Serve accepts connections on the listener (tls: the listener of port 443) until ctx ends.
func (p *Proxy) Serve(ctx context.Context, l net.Listener, tls bool) error {
	p.defaults()
	go func() { <-ctx.Done(); _ = l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go p.handle(ctx, c, tls)
	}
}

func remoteAddr(c net.Conn) netip.Addr {
	if t, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		if a, ok := netip.AddrFromSlice(t.IP); ok {
			return a.Unmap()
		}
	}
	return netip.Addr{}
}

// admit counts a new connection of a lab against its caps. It returns the release function, or false when the lab is at a cap.
func (p *Proxy) admit(lab string) (func(), string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.labs == nil {
		p.labs = map[string]*labLimits{}
	}
	l := p.labs[lab]
	if l == nil {
		l = &labLimits{}
		if p.LabNewPerSecond > 0 {
			l.new = rate.NewLimiter(rate.Limit(p.LabNewPerSecond), p.LabNewPerSecond)
		}
		p.labs[lab] = l
	}
	if p.LabMaxConns > 0 && l.open >= p.LabMaxConns {
		return nil, "lab-conns"
	}
	if l.new != nil && !l.new.Allow() {
		return nil, "lab-rate"
	}
	l.open++
	return func() {
		p.mu.Lock()
		l.open--
		p.mu.Unlock()
	}, ""
}

// hostAllowed counts a new connection to a host name against the per-name rate of the gateway.
func (p *Proxy) hostAllowed(name string) bool {
	if p.HostNewPerSecond <= 0 {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hosts == nil {
		p.hosts = map[string]*rate.Limiter{}
	}
	now := time.Now()
	if now.Sub(p.last) > time.Minute || len(p.hosts) > 10000 { // limiters that refilled long ago are as good as new ones
		p.hosts, p.last = map[string]*rate.Limiter{}, now
	}
	l := p.hosts[name]
	if l == nil {
		burst := max(int(p.HostNewPerSecond), 1)
		l = rate.NewLimiter(rate.Limit(p.HostNewPerSecond), burst)
		p.hosts[name] = l
	}
	return l.Allow()
}

func (p *Proxy) deny(c net.Conn, lab, host, reason string, http bool) {
	if p.Stats != nil {
		p.Stats.Denied(lab, host, reason)
	}
	p.Log.V(1).Info("refused", "lab", lab, "host", host, "reason", reason)
	if http {
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
	}
	_ = c.Close()
}

func (p *Proxy) handle(ctx context.Context, c net.Conn, isTLS bool) {
	defer func() { _ = c.Close() }()
	lab, ok := p.Labs.Lookup(remoteAddr(c))
	if !ok {
		p.deny(c, "", "", ReasonUnknown, false)
		return
	}
	release, capReason := p.admit(lab)
	if release == nil {
		if p.Stats != nil {
			p.Stats.Cap(lab, capReason)
		}
		_ = c.Close()
		return
	}
	defer release()

	_ = c.SetReadDeadline(time.Now().Add(p.HelloTimeout))
	var raw []byte
	var name, reason string
	if isTLS {
		h, err := ReadClientHello(c)
		switch {
		case err != nil:
			p.deny(c, lab, "", ReasonNotTLS, false)
			return
		case h.ECH:
			p.deny(c, lab, "", ReasonECH, false)
			return
		}
		raw = h.Raw
		name, reason = NormalizeName(h.ServerName)
	} else {
		head, host, err := readHTTPHead(c)
		if err != nil {
			p.deny(c, lab, "", ReasonNotHTTP, false)
			return
		}
		raw = head
		name, reason = NormalizeName(host)
	}
	_ = c.SetReadDeadline(time.Time{})
	if reason != "" {
		p.deny(c, lab, "", reason, !isTLS)
		return
	}
	if p.Deny.Denied(name) {
		p.deny(c, lab, name, ReasonDenied, !isTLS)
		return
	}
	if !p.hostAllowed(name) {
		if p.Stats != nil {
			p.Stats.Cap(lab, ReasonHostRate)
		}
		p.deny(c, lab, name, ReasonHostRate, !isTLS)
		return
	}
	ips, err := p.Resolver.LookupIPv4(ctx, name)
	if err != nil {
		p.deny(c, lab, name, ReasonResolve, !isTLS)
		return
	}
	port := "80"
	if isTLS {
		port = "443"
	}
	up, err := p.dial(ctx, ips, port)
	if err != nil {
		p.deny(c, lab, name, ReasonResolve, !isTLS)
		return
	}
	defer func() { _ = up.Close() }()
	if p.Stats != nil {
		p.Stats.Start(lab, name)
	}
	var sent, got int64
	defer func() {
		if p.Stats != nil {
			p.Stats.End(lab, name, atomic.LoadInt64(&sent), atomic.LoadInt64(&got))
		}
	}()
	if _, err := up.Write(raw); err != nil {
		return
	}
	atomic.AddInt64(&sent, int64(len(raw)))
	p.splice(ctx, c, up, &sent, &got)
}

// splice copies both ways until either side ends or the connection is as old as MaxConnAge.
func (p *Proxy) splice(ctx context.Context, a, b net.Conn, sent, got *int64) {
	ctx, cancel := context.WithTimeout(ctx, p.MaxConnAge)
	defer cancel()
	go func() { <-ctx.Done(); _ = a.Close(); _ = b.Close() }()
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn, n *int64) {
		buf := make([]byte, 32<<10)
		for {
			r, err := src.Read(buf)
			if r > 0 {
				atomic.AddInt64(n, int64(r))
				if _, werr := dst.Write(buf[:r]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		if t, ok := dst.(*net.TCPConn); ok {
			_ = t.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(b, a, sent)
	go cp(a, b, got)
	<-done
	<-done
}

// dial connects to the first of the addresses that answers. The control function refuses an address in a denied range even if the
// resolver let it through, so a rebinding between the lookup and the connect cannot reach the inside.
func (p *Proxy) dial(ctx context.Context, ips []netip.Addr, port string) (net.Conn, error) {
	var last error
	for _, ip := range ips {
		addr := net.JoinHostPort(ip.String(), port)
		var c net.Conn
		var err error
		if p.Dial != nil {
			c, err = p.Dial(ctx, addr)
		} else {
			d := net.Dialer{Timeout: p.DialTimeout, Control: func(_, address string, _ syscall.RawConn) error {
				host, _, _ := net.SplitHostPort(address)
				if a, err := netip.ParseAddr(host); err != nil || p.Resolver.Denied(a) {
					return fmt.Errorf("address %s is not public", host)
				}
				return nil
			}}
			c, err = d.DialContext(ctx, "tcp4", addr)
		}
		if err == nil {
			return c, nil
		}
		last = err
	}
	if last == nil {
		last = ErrNoPublicAddress
	}
	return nil, last
}

// readHTTPHead reads an HTTP/1.x request head (up to 16 kB) and returns the bytes read and the Host.
func readHTTPHead(r io.Reader) ([]byte, string, error) {
	const max = 16 << 10
	var buf bytes.Buffer
	br := bufio.NewReader(io.TeeReader(r, &buf))
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, "", err
	}
	// "GET /x HTTP/1.1": three words, the last starts with HTTP/
	f := strings.Fields(line)
	if len(f) != 3 || !strings.HasPrefix(f[2], "HTTP/") {
		return nil, "", errors.New("not an HTTP request line")
	}
	host := ""
	for buf.Len() <= max {
		l, err := br.ReadString('\n')
		if err != nil {
			return nil, "", err
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			// br may have read ahead of the head: what was read is what the server must get
			return buf.Bytes(), host, nil
		}
		if i := strings.IndexByte(l, ':'); i > 0 && strings.EqualFold(strings.TrimSpace(l[:i]), "host") && host == "" {
			host = strings.TrimSpace(l[i+1:])
		}
	}
	return nil, "", errors.New("HTTP head too large")
}

// Listen opens a TCP listener on all addresses of the pod.
func Listen(port int) (net.Listener, error) {
	return net.Listen("tcp4", ":"+strconv.Itoa(port))
}
