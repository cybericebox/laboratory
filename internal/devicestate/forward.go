package devicestate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"github.com/cybericebox/laboratory/internal/imagecache"
)

// maxHeaderBytes bounds the headers the relay accepts from a client and from the gate: a registry request carries a few hundred bytes.
const maxHeaderBytes = 64 << 10

// pushQuery are the query parameters the snapshot client uses on the push socket: the mount of a blob (mount, from) and the digest that
// ends an upload. The relay drops every other one; what the parameters mean stays the gate's decision (a mount source it has not allowed
// is refused there).
var pushQuery = []string{"mount", "from", "digest"}

// Forward listens on listen (a loopback address) and relays the registry requests of the node's container runtime and the kubelet to
// target, the registry gate. The node-agent uses it to make the cluster-internal registry reachable as localhost:<port>, which the
// runtime treats as plain HTTP, so no host configuration (registries.yaml, hosts.toml, certificates, DNS) is needed.
//
// The relay is HTTP-aware and has the identity of the node and nothing else to give: every request it forwards carries the projected
// ServiceAccount token of the node-agent (WithNodeToken) in X-Cib-Node-Token, and no header the client sent is believed: every X-Cib-*
// header and the client's Authorization header are removed. There is no credential on the node and none is needed: the gate decides by
// the node and the reference (the tenant is in the image path).
//
// This listener serves pulls only: GET and HEAD on /v2/, /v2/c/** (the image cache) and /v2/lab/** (the snapshots); anything else is
// answered with 405 before it reaches the gate. Any process on the node can connect to a loopback port, so what arrives here must be no
// more than what the container runtime gets. Pushes of snapshots go through the unix socket of WithPushSocket (ServePush).
// It returns when ctx ends.
func Forward(ctx context.Context, listen, target string, log logr.Logger, opts ...ForwardOption) error {
	var o forwardOptions
	for _, f := range opts {
		f(&o)
	}
	switch {
	case o.tlsCAFile != "" && o.token != nil:
		if err := o.loadTLS(target); err != nil {
			return err
		}
	case o.tlsCAFile == "" && o.token != nil && !o.allowClearToken:
		return errors.New("registry relay: a node token without a certificate authority (WithTLS) would travel in the clear: refusing to start")
	case o.token == nil && !o.plainDev:
		return errors.New("registry relay: no node token configured (WithNodeToken); plain http without a token needs WithPlainDevelopment")
	case o.token != nil && o.plainDev:
		return errors.New("registry relay: WithPlainDevelopment sends no token and cannot be combined with WithNodeToken")
	}
	tr := o.upstreamTransport()
	scheme := "http"
	if o.tlsConf != nil {
		scheme = "https"
	}
	pull := newServer(relayHandler(scheme, target, tr, o.token, pullAllowed, nil))
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	serve := func(srv *http.Server, ln net.Listener, what string) {
		defer wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error(err, "registry relay", "listener", what)
			errs <- err
		}
	}
	servers := []*http.Server{pull}
	wg.Add(1)
	go serve(pull, ln, "tcp")
	if o.pushSocket != "" {
		pln, err := listenPushSocket(o.pushSocket, o.pushSocketUID)
		if err != nil {
			_ = pull.Close()
			wg.Wait()
			return err
		}
		push := newServer(relayHandler(scheme, target, tr, o.token, pushAllowed, pushQuery))
		servers = append(servers, push)
		wg.Add(1)
		go serve(push, pln, "push socket")
	}
	var result error
	select {
	case <-ctx.Done():
	case result = <-errs:
	}
	for _, s := range servers {
		_ = s.Close()
	}
	wg.Wait()
	if o.pushSocket != "" {
		_ = os.Remove(o.pushSocket)
	}
	return result
}

// ForwardOption configures Forward.
type ForwardOption func(*forwardOptions)

type forwardOptions struct {
	// tlsCAFile makes the relay speak TLS to the registry, trusting only that certificate authority (PEM).
	tlsCAFile string
	tlsConf   *tls.Config
	token     *imagecache.TokenFile
	// pushSocket is the path of the unix socket of the snapshot push, pushSocketUID the owner given to it (-1: leave the owner).
	pushSocket    string
	pushSocketUID int
	// plainDev is the explicit development setup: plain http to the registry and no token at all. allowClearToken is for tests only.
	plainDev, allowClearToken bool
	// transport is the transport towards the registry (tests); nil builds one from the TLS configuration.
	transport http.RoundTripper
}

// WithTLS makes the forwarder dial the registry over TLS, trusting only the certificate authority in caFile; the name the server's
// certificate must carry is the host of the target.
func WithTLS(caFile string) ForwardOption {
	return func(o *forwardOptions) { o.tlsCAFile = caFile }
}

// WithNodeToken gives every request the projected ServiceAccount token in tokenFile (audience laboratory-registry-gate), read again every
// imagecache.TokenReread, in the header X-Cib-Node-Token. A relay without it does not start: it would have no identity.
func WithNodeToken(tokenFile string) ForwardOption {
	return func(o *forwardOptions) { o.token = imagecache.NewTokenFile(tokenFile) }
}

// WithPushSocket makes the relay serve the snapshot push of node-state on a unix socket at path (mode 0600, owned by uid when uid is not
// negative: the user of node-state), apart from the TCP listener that cannot be told from any other process of the node.
func WithPushSocket(path string, uid int) ForwardOption {
	return func(o *forwardOptions) { o.pushSocket, o.pushSocketUID = path, uid }
}

// WithPlainDevelopment is the development setup: the relay speaks plain http to the registry and sends no token, so nothing of the node's
// identity can travel in the clear. A real setup has a certificate authority (WithTLS) and a token (WithNodeToken).
func WithPlainDevelopment() ForwardOption { return func(o *forwardOptions) { o.plainDev = true } }

func (o *forwardOptions) loadTLS(target string) error {
	pem, err := os.ReadFile(o.tlsCAFile)
	if err != nil {
		return fmt.Errorf("registry CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return fmt.Errorf("registry CA %s holds no certificate", o.tlsCAFile)
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		host = target
	}
	o.tlsConf = &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12}
	return nil
}

func (o *forwardOptions) upstreamTransport() http.RoundTripper {
	if o.transport != nil {
		return o.transport
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// The registry is the gate at a fixed address: no proxy of the environment, and no total deadline (a layer is large), but a bound on
	// every wait that is not the transfer itself.
	tr.Proxy = nil
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.ResponseHeaderTimeout = 5 * time.Minute
	tr.IdleConnTimeout = 90 * time.Second
	tr.MaxResponseHeaderBytes = maxHeaderBytes
	if o.tlsConf != nil {
		tr.TLSClientConfig = o.tlsConf
	}
	return tr
}

// A rule says whether the relay forwards a request; the answer is the status the relay itself gives when it does not.
type rule func(r *http.Request) (refusal int)

// pullAllowed is the rule of the TCP listener: GET and HEAD of /v2/, /v2/c/** and /v2/lab/**.
func pullAllowed(r *http.Request) int {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return http.StatusMethodNotAllowed
	}
	p, ok := cleanRequestPath(r)
	if !ok {
		return http.StatusMethodNotAllowed
	}
	if p == "/v2/" || strings.HasPrefix(p, "/v2/c/") || strings.HasPrefix(p, "/v2/lab/") {
		return 0
	}
	return http.StatusMethodNotAllowed
}

// pushAllowed is the rule of the push socket: POST, PATCH, PUT, GET and HEAD under /v2/lab/<namespace>/<lab>/<device>/, nothing else. No
// DELETE: the nodes have none, the gate deletes what a new snapshot replaced.
func pushAllowed(r *http.Request) int {
	switch r.Method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodGet, http.MethodHead:
	default:
		return http.StatusMethodNotAllowed
	}
	p, ok := cleanRequestPath(r)
	if !ok {
		return http.StatusMethodNotAllowed
	}
	rest, ok := strings.CutPrefix(p, "/v2/lab/")
	if !ok {
		return http.StatusMethodNotAllowed
	}
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return http.StatusMethodNotAllowed
	}
	return 0
}

// cleanRequestPath is the path of a request when it is in its plain form: no empty or dot element, no encoded slash, dot or backslash,
// no control character. What the relay checks must be what the gate reads.
func cleanRequestPath(r *http.Request) (string, bool) {
	p := r.URL.Path
	raw := strings.ToLower(r.URL.EscapedPath())
	if strings.Contains(raw, "%2f") || strings.Contains(raw, "%2e") || strings.Contains(raw, "%5c") || strings.ContainsAny(p, "\\\x00\r\n") {
		return "", false
	}
	if !strings.HasPrefix(p, "/") || strings.Contains(p, "//") {
		return "", false
	}
	for _, elem := range strings.Split(p, "/") {
		if elem == "." || elem == ".." {
			return "", false
		}
	}
	return p, true
}

// newServer is an HTTP server with the bounds a relay needs: no total deadline (a blob is large), a short header deadline, an idle limit and
// a small header limit.
func newServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: maxHeaderBytes}
}

// relayHandler forwards what the rule lets through to target, with the identity of the node: the client's Authorization and every X-Cib-*
// header are removed and the token is set. A token that cannot be read is a 503, never a request without identity.
func relayHandler(scheme, target string, transport http.RoundTripper, token *imagecache.TokenFile, allow rule, query []string) http.Handler {
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = scheme, target, target
			if query != nil {
				q := url.Values{}
				for _, k := range query {
					if v, ok := pr.In.URL.Query()[k]; ok {
						q[k] = v
					}
				}
				pr.Out.URL.RawQuery = q.Encode()
			}
			pr.Out.Header.Del("Authorization")
			imagecache.StripPlatformHeaders(pr.Out.Header)
			if token != nil {
				tok, _ := token.Token() // checked before; a token read again in between that fails leaves the request without it, and the gate refuses it
				pr.Out.Header.Set(imagecache.HeaderNodeToken, tok)
			}
		},
		FlushInterval: -1, // blobs stream
		ErrorHandler:  func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(http.StatusBadGateway) },
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status := allow(r); status != 0 {
			http.Error(w, http.StatusText(status), status)
			return
		}
		if token != nil {
			if _, err := token.Token(); err != nil {
				http.Error(w, "the node has no token for the registry", http.StatusServiceUnavailable)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	})
}

// listenPushSocket creates the unix socket of the snapshot push in a directory of its own (mode 0700, owned by uid when uid is not negative),
// and the socket itself with mode 0600 from the start (a restrictive umask while it is created). A socket left by an earlier run is removed;
// anything else at the path is an error. The directory must not be one shared with other sockets (node-agent.sock, the CNI gate).
func listenPushSocket(sock string, uid int) (net.Listener, error) {
	dir := path.Dir(sock)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("push socket %s: %w", sock, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("push socket %s: %w", sock, err)
	}
	if uid >= 0 {
		if err := os.Chown(dir, uid, -1); err != nil {
			return nil, fmt.Errorf("push socket %s: %w", sock, err)
		}
	}
	if fi, err := os.Lstat(sock); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("push socket %s: the path exists and is not a socket", sock)
		}
		if err := os.Remove(sock); err != nil {
			return nil, fmt.Errorf("push socket %s: %w", sock, err)
		}
	}
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", sock)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("push socket %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("push socket %s: %w", sock, err)
	}
	if uid >= 0 {
		if err := os.Chown(sock, uid, -1); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("push socket %s: %w", sock, err)
		}
	}
	return ln, nil
}

// UnixTransport is a transport that dials the unix socket at sock whatever the URL says: the client keeps the logical host of the registry
// (localhost:<port>) in every name and reference, and the socket is how it reaches the relay.
func UnixTransport(sock string) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}
	return tr
}
