//go:build linux

package gateway

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/cybericebox/laboratory/internal/egressproxy"
	"github.com/cybericebox/laboratory/internal/gateway"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// The redirects of a lab's 80/443/53 are installed by the reconciler, which is built after startEgress returns: by then every listener of the
// egress services must stand, so that a connection redirected into them never meets a closed port.
func TestStartEgressListensBeforeItReturns(t *testing.T) {
	cfg := &gateway.Config{
		EgressHTTPPort: freePort(t), EgressTLSPort: freePort(t), EgressDNSPort: freePort(t), EgressStatsPort: freePort(t),
		EgressDNSUpstreams: "192.0.2.1",
	}
	if err := startEgress(cfg, egressproxy.NewLabMap(), logr.Discard()); err != nil {
		t.Fatal(err)
	}
	for name, port := range map[string]int{"http": cfg.EgressHTTPPort, "tls": cfg.EgressTLSPort, "dns tcp": cfg.EgressDNSPort, "summaries": cfg.EgressStatsPort} {
		c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
		if err != nil {
			t.Fatalf("%s is not listening when startEgress returns: %v", name, err)
		}
		_ = c.Close()
	}
	if u, err := net.ListenPacket("udp4", ":"+strconv.Itoa(cfg.EgressDNSPort)); err == nil {
		_ = u.Close()
		t.Fatal("dns udp is not bound when startEgress returns")
	}
}
