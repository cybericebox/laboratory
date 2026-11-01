package egressproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/cybericebox/laboratory/internal/egress"
)

// Resolver looks a name up for real, through its own upstream servers and never through the pod's resolv.conf (the cluster's DNS answers
// internal names, and the lab chooses nothing here). An address in a range a lab may never reach (internal/egress) is dropped from the
// answer: the proxy never connects there and the DNS server never tells it.
type Resolver struct {
	r    *net.Resolver
	deny []netip.Prefix

	mu    sync.Mutex
	cache map[string]cacheEntry
	// TTL is how long an answer is kept (default 60 s), NegativeTTL how long a name that did not resolve is remembered (default 10 s).
	TTL, NegativeTTL time.Duration
	// Now is the clock (tests).
	Now func() time.Time
}

type cacheEntry struct {
	ips     []netip.Addr
	err     error
	expires time.Time
}

// NewResolver makes a resolver that asks the upstream servers ("ip" or "ip:port"; UDP, and TCP when the answer is truncated).
func NewResolver(upstreams []string) (*Resolver, error) {
	if len(upstreams) == 0 {
		return nil, errors.New("no upstream DNS servers")
	}
	servers := make([]string, 0, len(upstreams))
	for _, u := range upstreams {
		if _, _, err := net.SplitHostPort(u); err != nil {
			u = net.JoinHostPort(u, "53")
		}
		servers = append(servers, u)
	}
	var next int
	var mu sync.Mutex
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		mu.Lock()
		addr := servers[next%len(servers)]
		next++
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}}
	res := &Resolver{r: r, cache: map[string]cacheEntry{}, TTL: time.Minute, NegativeTTL: 10 * time.Second, Now: time.Now}
	for _, c := range egress.DenyV4 {
		res.deny = append(res.deny, netip.MustParsePrefix(c))
	}
	return res, nil
}

// SetStatic fixes the answer for a name (it does not expire): for a deployment that pins a name, and for tests.
func (r *Resolver) SetStatic(name string, ips ...netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[name] = cacheEntry{ips: ips, expires: time.Now().Add(100 * 365 * 24 * time.Hour)}
}

// Denied says an address is in a range a lab never reaches.
func (r *Resolver) Denied(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range r.deny {
		if p.Contains(a) {
			return true
		}
	}
	return a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsMulticast()
}

// ErrNoPublicAddress is the error of a name with no public IPv4 address.
var ErrNoPublicAddress = errors.New("the name has no public IPv4 address")

// LookupIPv4 returns the public IPv4 addresses of a name (the name is already normalized).
func (r *Resolver) LookupIPv4(ctx context.Context, name string) ([]netip.Addr, error) {
	now := r.Now()
	r.mu.Lock()
	if e, ok := r.cache[name]; ok && now.Before(e.expires) {
		r.mu.Unlock()
		return e.ips, e.err
	}
	r.mu.Unlock()
	ips, err := r.lookup(ctx, name)
	ttl := r.TTL
	if err != nil {
		ttl = r.NegativeTTL
	}
	r.mu.Lock()
	if len(r.cache) > 4096 { // a lab can ask for names without end: the cache does not grow with it
		r.cache = map[string]cacheEntry{}
	}
	r.cache[name] = cacheEntry{ips: ips, err: err, expires: now.Add(ttl)}
	r.mu.Unlock()
	return ips, err
}

func (r *Resolver) lookup(ctx context.Context, name string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := r.r.LookupNetIP(ctx, "ip4", name)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range addrs {
		if a = a.Unmap(); a.Is4() && !r.Denied(a) {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoPublicAddress, name)
	}
	return out, nil
}
