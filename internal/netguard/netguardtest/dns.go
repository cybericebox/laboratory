// Package netguardtest is for tests: a resolver that names a loopback address for any host name.
package netguardtest

import (
	"context"
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// DNSServer answers every A query with 127.0.0.1 (and every other type with nothing): a name that resolves to the loopback, as a
// rebinding host does. The returned resolver asks only it.
func DNSServer(t *testing.T) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			h.Response, h.RCode = true, dnsmessage.RCodeSuccess
			b := dnsmessage.NewBuilder(nil, h)
			_ = b.StartQuestions()
			_ = b.Question(q)
			_ = b.StartAnswers()
			if q.Type == dnsmessage.TypeA {
				_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 1}, dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}})
			}
			if out, err := b.Finish(); err == nil {
				_, _ = pc.WriteTo(out, from)
			}
		}
	}()
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", pc.LocalAddr().String())
	}}
}
