package netguard

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybericebox/laboratory/internal/netguard/netguardtest"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestCheckRedirect(t *testing.T) {
	for _, ok := range []string{
		"https://cdn.example.com/blob", "https://CDN.Example.COM/blob", "https://cdn.example.com:443/blob", "https://a-b.c1.example.co.uk/x?y=1",
		"https://xn--80ak6aa92e.example/x",
	} {
		if err := CheckRedirect(mustURL(t, ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://cdn.example.com/blob",          // not https
		"https://user:pw@cdn.example.com/blob", // userinfo
		"https://user@cdn.example.com/blob",    //
		"https://cdn.example.com:8443/blob",    // another port
		"https://cdn.example.com:80/blob",      //
		"https://cdn.example.com:/blob",        // an empty port
		"https://127.0.0.1/blob",               // IP literals in every form
		"https://127.0.0.1:443/blob",           //
		"https://10.0.0.1/blob",                //
		"https://169.254.169.254/latest",       // the metadata address
		"https://2130706433/blob",              // decimal
		"https://0x7f000001/blob",              // hex, no dot
		"https://0x7f.0.0.1/blob",              // mixed
		"https://0177.0.0.1/blob",              // octal
		"https://127.1/blob",                   // short form
		"https://1.2.3.4/blob",                 //
		"https://[::1]/blob",                   // IPv6
		"https://[::ffff:127.0.0.1]/blob",      // IPv4-mapped
		"https://[fe80::1%25eth0]/blob",        // with a zone
		"https://cdn.example.com./blob",        // trailing dot
		"https://cdn.example.com.:443/blob",    //
		"https://localhost/blob",               // no dot
		"https://metadata/blob",                //
		"https://10.0.0.1./blob",               //
		"https://a..b.example/blob",            // empty label
		"https://-a.example.com/blob",          // label starting with a hyphen
		"https://example.1a/blob",              // a last label that starts with a digit
		"https:///blob",                        // no host
		"ftp://cdn.example.com/blob",           //
	} {
		u, err := url.Parse(bad)
		if err != nil {
			continue // not even a URL: it cannot be followed
		}
		if err := CheckRedirect(u); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
}

func TestInternalAddr(t *testing.T) {
	for _, in := range []string{
		"127.0.0.1", "127.255.255.254", "::1", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "fc00::1", "fd12:3456::1",
		"169.254.169.254", "169.254.0.1", "fe80::1", "100.64.0.1", "100.127.255.255", "0.0.0.0", "::", "0.1.2.3", "224.0.0.1", "ff02::1",
		"255.255.255.255", "240.0.0.1", "192.0.0.1", "198.18.0.1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "::ffff:100.64.0.1", // IPv4-mapped
		"64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", // NAT64 of 127.0.0.1 and of the metadata address
		"2002:7f00:1::1", "2002:0a00:0001::", // 6to4 of 127.0.0.1 and 10.0.0.1
		"::7f00:1", // IPv4-compatible 127.0.0.1
	} {
		if !InternalAddr(netip.MustParseAddr(in)) {
			t.Errorf("%s must be internal", in)
		}
	}
	for _, out := range []string{
		"93.184.216.34", "8.8.8.8", "1.1.1.1", "172.15.255.255", "172.32.0.1", "100.63.255.255", "100.128.0.1", "169.253.1.1", "198.17.0.1", "198.20.0.1",
		"2606:4700:4700::1111", "2001:4860:4860::8888", "::ffff:8.8.8.8", "64:ff9b::808:808", "2002:0808:0808::1",
	} {
		if InternalAddr(netip.MustParseAddr(out)) {
			t.Errorf("%s must be allowed", out)
		}
	}
}

// The dialer of a hop checks the address it connects to, after the name is resolved; the dialer of a request to a configured registry
// does not (a registry may be a cluster address).
func TestHopDialRefusesWhatANameResolvesTo(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var accepted atomic.Int64
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	tr := NewTransport(time.Second, netguardtest.DNSServer(t))
	addr := net.JoinHostPort("rebind.test", strconv.Itoa(port))

	if c, err := tr.DialContext(HopContext(context.Background()), "tcp", addr); err == nil || !strings.Contains(err.Error(), "internal address") {
		if c != nil {
			_ = c.Close()
		}
		t.Fatalf("a hop to a name that resolves to the loopback must be refused: %v", err)
	}
	if c, err := tr.DialContext(HopContext(context.Background()), "tcp", l.Addr().String()); err == nil {
		_ = c.Close()
		t.Fatal("a hop to the loopback address itself must be refused")
	}
	time.Sleep(50 * time.Millisecond)
	if accepted.Load() != 0 {
		t.Fatalf("nothing may connect: %d", accepted.Load())
	}
	c, err := tr.DialContext(context.Background(), "tcp", addr) // not a hop: the registry's own request
	if err != nil {
		t.Fatalf("a request that is not a hop is dialed as before: %v", err)
	}
	_ = c.Close()
}
