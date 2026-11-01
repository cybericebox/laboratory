package egressproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/time/rate"
)

// DNS is the small resolver of the gateway. The gateway redirects every packet to port 53 of a lab, to whatever address, to it: a lab that
// names a DNS server of its own talks to this one all the same. It answers A queries with the real, public IPv4 addresses of the name
// (looked up through the gateway's own upstream servers), and nothing else: AAAA, HTTPS and SVCB queries get an empty answer (no ECH keys
// reach the browsers, and no IPv6 address the gateway cannot route), the other types too. A name that is an IP address, a tunnel service,
// or has no public address does not resolve. Each lab may ask QueriesPerSecond questions a second.
type DNS struct {
	Labs     *LabMap
	Resolver *Resolver
	Deny     *Denylist
	Stats    *Collector
	Log      logr.Logger

	// QueriesPerSecond is the rate cap of a lab (default 50; negative = no cap).
	QueriesPerSecond float64
	// AnswerTTL is the TTL of the answers (default 30 s).
	AnswerTTL uint32

	mu     sync.Mutex
	limits map[string]*rate.Limiter
}

func (d *DNS) allow(lab string) bool {
	qps := d.QueriesPerSecond
	if qps < 0 {
		return true
	}
	if qps == 0 {
		qps = 50
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.limits == nil {
		d.limits = map[string]*rate.Limiter{}
	}
	l := d.limits[lab]
	if l == nil {
		l = rate.NewLimiter(rate.Limit(qps), int(qps))
		d.limits[lab] = l
	}
	return l.Allow()
}

// Handle answers one query packet; nil means no answer (a lab over its cap, or not a query).
func (d *DNS) Handle(ctx context.Context, from netip.Addr, query []byte) []byte {
	lab, ok := d.Labs.Lookup(from)
	if !ok {
		return nil
	}
	var m dnsmessage.Message
	if err := m.Unpack(query); err != nil || m.Response || len(m.Questions) != 1 {
		return nil
	}
	if !d.allow(lab) {
		if d.Stats != nil {
			d.Stats.Cap(lab, "dns-rate")
		}
		return nil
	}
	q := m.Questions[0]
	resp := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: m.ID, Response: true, Authoritative: false, RecursionDesired: m.RecursionDesired, RecursionAvailable: true, RCode: dnsmessage.RCodeSuccess},
		Questions: m.Questions,
	}
	name, reason := NormalizeName(strings.TrimSuffix(q.Name.String(), "."))
	switch {
	case q.Class != dnsmessage.ClassINET:
		resp.RCode = dnsmessage.RCodeNotImplemented
	case reason != "" || d.Deny.Denied(name):
		resp.RCode = dnsmessage.RCodeNameError
		if d.Stats != nil {
			d.Stats.Denied(lab, "", "dns-"+orWord(reason, ReasonDenied))
		}
	case q.Type == dnsmessage.TypeA:
		ips, err := d.Resolver.LookupIPv4(ctx, name)
		var dnsErr *net.DNSError
		switch {
		case err == nil:
			ttl := d.AnswerTTL
			if ttl == 0 {
				ttl = 30
			}
			for _, ip := range ips {
				resp.Answers = append(resp.Answers, dnsmessage.Resource{
					Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: ttl},
					Body:   &dnsmessage.AResource{A: ip.As4()},
				})
			}
		case errors.Is(err, ErrNoPublicAddress) || (errors.As(err, &dnsErr) && dnsErr.IsNotFound):
			resp.RCode = dnsmessage.RCodeNameError
		default:
			resp.RCode = dnsmessage.RCodeServerFailure
		}
	default:
		// AAAA, HTTPS (65), SVCB (64), and every other type: the name is not denied, there is nothing to say about it
	}
	out, err := resp.Pack()
	if err != nil {
		return nil
	}
	return out
}

func orWord(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ServeUDP answers on the packet connection until ctx ends.
func (d *DNS) ServeUDP(ctx context.Context, pc net.PacketConn) error {
	go func() { <-ctx.Done(); _ = pc.Close() }()
	buf := make([]byte, 4096)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		ua, ok := addr.(*net.UDPAddr)
		if !ok {
			continue
		}
		from, _ := netip.AddrFromSlice(ua.IP)
		q := append([]byte(nil), buf[:n]...)
		go func() {
			if out := d.Handle(ctx, from.Unmap(), q); out != nil {
				_, _ = pc.WriteTo(out, addr)
			}
		}()
	}
}

// ServeTCP answers on the listener until ctx ends (DNS over TCP: a 2-byte length before each message).
func (d *DNS) ServeTCP(ctx context.Context, l net.Listener) error {
	go func() { <-ctx.Done(); _ = l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go d.serveTCPConn(ctx, c)
	}
}

func (d *DNS) serveTCPConn(ctx context.Context, c net.Conn) {
	defer func() { _ = c.Close() }()
	from := remoteAddr(c)
	for i := 0; i < 16; i++ { // a few queries on one connection, then it ends
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		var l [2]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(l[:]))
		if n == 0 || n > 4096 {
			return
		}
		q := make([]byte, n)
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		out := d.Handle(ctx, from, q)
		if out == nil {
			return
		}
		binary.BigEndian.PutUint16(l[:], uint16(len(out)))
		if _, err := c.Write(append(l[:], out...)); err != nil {
			return
		}
	}
}
