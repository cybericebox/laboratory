// Package labtunnel is the client of the gRPC-kind access of a lab device (EDA web-direct, programmatic level): a byte tunnel over a
// WebSocket through the lab proxy to the TCP port of the device service. The proxy never looks at the bytes, so the application's own
// authentication (TLS, mTLS, tokens) runs end to end over the tunnel.
//
//	conn, err := grpc.NewClient("passthrough:///"+host+":443",
//	    grpc.WithContextDialer(labtunnel.Dialer(getHandoff)),
//	    grpc.WithTransportCredentials(yourOwnCredentials))
//
// getHandoff asks the platform for a handoff link for the device's host (a JWT valid for 60 seconds, with use "tunnel"; the platform
// signs it with the tenant's access key). The dialer exchanges it for a tunnel token on first use, refreshes the token before it expires,
// and on a dropped tunnel (a proxy replica restarted, a network blip) reconnects, possibly to another replica, with the token it still
// holds. It asks for a new handoff only when the token cannot be refreshed (past max_until) or the proxy answers 401. The protocol for
// other languages is in docs/tunnel-access.md.
package labtunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Subprotocol is the first WebSocket subprotocol of the tunnel; the second is the token.
const Subprotocol = "cib.tunnel.v1"

const (
	exchangePath = "/_access/exchange"
	refreshPath  = "/_access/refresh"
	tunnelPath   = "/_tunnel"
	readLimit    = 1 << 20
	requestWait  = 20 * time.Second
	maxMargin    = 5 * time.Minute
)

// HTTPError is an answer of the proxy that is not a success.
type HTTPError struct {
	Status int
	Code   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("lab proxy answered %d %s", e.Status, e.Code) }

// ErrUnauthorized is what an HTTPError with status 401 matches.
var ErrUnauthorized = errors.New("lab proxy: unauthorized")

func (e *HTTPError) Is(target error) bool {
	return target == ErrUnauthorized && e.Status == http.StatusUnauthorized
}

type options struct {
	client   *http.Client
	endpoint string
	now      func() time.Time
}

// Option configures a Dialer.
type Option func(*options)

// WithTLSConfig sets the TLS configuration of the connection to the proxy (the system roots by default). It is the proxy's certificate
// that it checks; the application's own TLS over the tunnel is the caller's gRPC transport credentials.
func WithTLSConfig(cfg *tls.Config) Option {
	return func(o *options) {
		o.client = &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, Proxy: http.ProxyFromEnvironment}}
	}
}

// WithHTTPClient sets the HTTP client used for the exchange, the refresh and the WebSocket (its Timeout must be zero).
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.client = c } }

// WithEndpoint sends the requests to a base URL ("https://proxy.example:8443", or "http://127.0.0.1:9000" in a test) instead of
// https://<addr>. The Host header stays the host of addr, because the proxy tells the devices apart by host name.
func WithEndpoint(baseURL string) Option { return func(o *options) { o.endpoint = baseURL } }

// withClock replaces the clock (tests).
func withClock(now func() time.Time) Option { return func(o *options) { o.now = now } }

type session struct {
	token     string
	issued    time.Time
	expiresAt time.Time
	maxUntil  time.Time
}

type dialer struct {
	getHandoff func(ctx context.Context) (string, error)
	o          options
	mu         sync.Mutex
	sessions   map[string]*session
}

// Dialer returns a dial function for grpc.WithContextDialer (and for any code that wants a net.Conn to a device service): addr is the
// host and port of the device's gRPC-kind host on the lab proxy, as gRPC passes it.
func Dialer(getHandoff func(ctx context.Context) (string, error), opts ...Option) func(ctx context.Context, addr string) (net.Conn, error) {
	d := &dialer{getHandoff: getHandoff, sessions: map[string]*session{}}
	d.o.now = time.Now
	for _, opt := range opts {
		opt(&d.o)
	}
	if d.o.client == nil {
		d.o.client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	}
	return d.dial
}

// base is the URL the requests of an address go to, and the host name the proxy sees.
func (d *dialer) base(addr string) (baseURL, host string, err error) {
	host = addr
	if h, _, serr := net.SplitHostPort(addr); serr == nil {
		host = h
	}
	if d.o.endpoint != "" {
		return strings.TrimSuffix(d.o.endpoint, "/"), host, nil
	}
	return "https://" + addr, host, nil
}

func (d *dialer) dial(ctx context.Context, addr string) (net.Conn, error) {
	base, host, err := d.base(addr)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		token, err := d.token(ctx, addr, base, host, attempt > 0)
		if err != nil {
			return nil, err
		}
		conn, err := d.open(ctx, base, host, token)
		if err == nil {
			return conn, nil
		}
		var he *HTTPError
		if attempt == 0 && errors.As(err, &he) && he.Status == http.StatusUnauthorized {
			// the proxy does not know the token (its secret changed, the key was rotated): forget it and start again from a handoff
			d.forget(addr)
			continue
		}
		return nil, err
	}
}

func (d *dialer) forget(addr string) {
	d.mu.Lock()
	delete(d.sessions, addr)
	d.mu.Unlock()
}

// token returns a token that is good now: the held one while it is, a refreshed one when it is about to end, a new one from a handoff when
// there is none or it cannot be refreshed. fresh forces the handoff.
func (d *dialer) token(ctx context.Context, addr, base, host string, fresh bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.o.now()
	s := d.sessions[addr]
	if s != nil && !fresh && now.Before(s.maxUntil) {
		margin := s.expiresAt.Sub(s.issued) / 4
		if margin > maxMargin {
			margin = maxMargin
		}
		if now.Before(s.expiresAt.Add(-margin)) {
			return s.token, nil
		}
		if now.Before(s.expiresAt) {
			next, err := d.post(ctx, base, host, refreshPath, map[string]string{"token": s.token})
			if err == nil {
				d.sessions[addr] = d.keep(next)
				return next.Token, nil
			}
			var he *HTTPError
			if !errors.As(err, &he) {
				return s.token, nil // the proxy is not reachable now: the token that is still valid is as good as before
			}
			// refused (401, 404): a new handoff decides
		}
	}
	handoff, err := d.getHandoff(ctx)
	if err != nil {
		return "", fmt.Errorf("labtunnel: handoff: %w", err)
	}
	next, err := d.post(ctx, base, host, exchangePath, map[string]string{"handoff": handoff})
	if err != nil {
		return "", err
	}
	d.sessions[addr] = d.keep(next)
	return next.Token, nil
}

func (d *dialer) keep(t tokenAnswer) *session {
	return &session{token: t.Token, issued: d.o.now(), expiresAt: t.ExpiresAt, maxUntil: t.MaxUntil}
}

type tokenAnswer struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	MaxUntil  time.Time `json:"max_until"`
}

func (d *dialer) post(ctx context.Context, base, host, path string, body any) (tokenAnswer, error) {
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, requestWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(raw))
	if err != nil {
		return tokenAnswer{}, err
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.o.client.Do(req)
	if err != nil {
		return tokenAnswer{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		return tokenAnswer{}, &HTTPError{Status: resp.StatusCode, Code: e.Error}
	}
	var out tokenAnswer
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&out); err != nil || out.Token == "" {
		return tokenAnswer{}, fmt.Errorf("labtunnel: bad answer of the proxy: %v", err)
	}
	return out, nil
}

// open upgrades to the tunnel and returns it as a net.Conn: Read and Write carry the bytes, deadlines and Close work as on a TCP connection.
func (d *dialer) open(ctx context.Context, base, host, token string) (net.Conn, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = tunnelPath
	ws, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: d.o.client, Host: host, Subprotocols: []string{Subprotocol, token}, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil {
			return nil, &HTTPError{Status: resp.StatusCode, Code: statusCode(resp)}
		}
		return nil, err
	}
	if ws.Subprotocol() != Subprotocol {
		_ = ws.CloseNow()
		return nil, errors.New("labtunnel: the proxy did not accept the tunnel protocol")
	}
	ws.SetReadLimit(readLimit)
	return newConn(ws, host), nil
}

func statusCode(resp *http.Response) string {
	var e struct {
		Error string `json:"error"`
	}
	if resp.Body != nil {
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
	}
	return e.Error
}
